package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gregor-tokarev/fam/internal/transport"
	"github.com/gregor-tokarev/fam/internal/version"
)

// socketPath returns a short, per-(HOME, XDG_CACHE_HOME, binary) Unix socket path under /tmp/fam-<uid>.
func socketPath() (sock string, watchDir string, ok bool) {
	if os.Getenv("FAM_NO_DAEMON") == "1" {
		return "", "", false
	}
	dir := filepath.Join("/tmp", fmt.Sprintf("fam-%d", os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", false
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", false
	}
	st, sok := info.Sys().(*syscall.Stat_t)
	if !sok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0o700 {
		return "", "", false
	}
	home, _ := os.UserHomeDir()
	cache := os.Getenv("XDG_CACHE_HOME")
	exe, _ := os.Executable()
	exeKey := exe
	if ei, err := os.Stat(exe); err == nil {
		exeKey = fmt.Sprintf("%s:%d:%d", exe, ei.Size(), ei.ModTime().UnixNano())
	}
	h := sha256.Sum256([]byte(version.Version + "\x00" + home + "\x00" + cache + "\x00" + exeKey))
	return filepath.Join(dir, "d-"+hex.EncodeToString(h[:8])+".sock"), home, true
}

var (
	ensureMu   sync.Mutex
	cachedSock string
)

// Ensure returns the path to a running local daemon socket, spawning `fam internal daemon` in a
// detached session if needed. Returns ("", false) on any failure so callers fall back cleanly.
func Ensure(ctx context.Context) (string, bool) {
	sock, watchDir, ok := socketPath()
	if !ok {
		return "", false
	}
	if ping(sock) {
		return sock, true
	}
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if cachedSock == sock && ping(sock) {
		return sock, true
	}
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	cmd := exec.Command(exe, "internal", "daemon", "--socket", sock, "--watch-dir", watchDir)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", false
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", false
		}
		if ping(sock) {
			cachedSock = sock
			return sock, true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return "", false
}

func ping(sock string) bool {
	c, err := net.DialTimeout("unix", sock, 100*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := c.Write([]byte("{\"op\":\"ping\"}\n")); err != nil {
		return false
	}
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return false
	}
	var resp rpcResp
	return json.Unmarshal(line, &resp) == nil && resp.OK
}

// Stop stops the local daemon for the current environment, if running.
func Stop() error {
	sock, _, ok := socketPath()
	if !ok {
		return nil
	}
	c, err := net.DialTimeout("unix", sock, 150*time.Millisecond)
	if err != nil {
		return nil
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	_, _ = c.Write([]byte("{\"op\":\"stop\"}\n"))
	_, _ = bufio.NewReader(c).ReadBytes('\n')
	return nil
}

// Transport proxies SSH operations through the warm local daemon so persistent ControlMaster
// connections and ssh_wire workers survive across consecutive fam commands.
type Transport struct {
	sock      string
	machine   string
	target    string
	sshConfig string
	observer  transport.Observer

	mu          sync.Mutex
	osName      string
	sha256Cmd   string
	supportsSeq bool
	maxConcur   int
	specResults map[string]transport.WireResp
}

// NewTransport returns a daemon-backed SSH transport if the local daemon is available, or nil so
// the caller can fall back directly to transport.NewSSH.
func NewTransport(ctx context.Context, machine, target, sshConfig string, obs transport.Observer) *Transport {
	sock, ok := Ensure(ctx)
	if !ok {
		return nil
	}
	return &Transport{
		sock:      sock,
		machine:   machine,
		target:    target,
		sshConfig: sshConfig,
		observer:  obs,
		sha256Cmd: "sha256sum",
		maxConcur: 8,
	}
}

func (t *Transport) Name() string { return t.machine }
func (t *Transport) Kind() string { return "ssh" }

func (t *Transport) SetOS(goos string) {
	t.mu.Lock()
	t.osName = goos
	if goos == "darwin" {
		t.sha256Cmd = "shasum -a 256"
	} else {
		t.sha256Cmd = "sha256sum"
	}
	t.mu.Unlock()
	_, _ = t.call(context.Background(), rpcReq{Op: "set_os", OS: goos})
}

func (t *Transport) SHA256Cmd() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sha256Cmd
}

func (t *Transport) MaxConcurrency() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.maxConcur > 0 {
		return t.maxConcur
	}
	return 8
}

func (t *Transport) SupportsApplySeq() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.supportsSeq
}

func (t *Transport) observe(c transport.Cmd, r transport.Result, err error) {
	if t.observer != nil {
		t.observer(t.machine, c, r, err)
	}
}

func (t *Transport) decodeErr(resp rpcResp) error {
	if resp.Err == "" {
		return nil
	}
	if resp.ConnErr {
		return &transport.ConnError{Machine: t.machine, Target: t.target, Stderr: resp.ConnStderr}
	}
	if resp.SkillFallbk {
		return errDaemonSkillFallback
	}
	return errors.New(resp.Err)
}

var errDaemonSkillFallback = errors.New("use shell skill write")

func (t *Transport) call(ctx context.Context, req rpcReq) (rpcResp, error) {
	req.Machine = t.machine
	req.Target = t.target
	req.SSHConfig = t.sshConfig
	t.mu.Lock()
	if req.OS == "" {
		req.OS = t.osName
	}
	t.mu.Unlock()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", t.sock)
	if err != nil {
		return rpcResp{}, &transport.ConnError{Machine: t.machine, Target: t.target, Stderr: "daemon connect: " + err.Error()}
	}
	defer conn.Close()

	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			_ = conn.Close()
		}
	}()
	defer close(done)

	b, err := json.Marshal(req)
	if err != nil {
		return rpcResp{}, err
	}
	b = append(b, '\n')
	if _, err := conn.Write(b); err != nil {
		if ctx.Err() != nil {
			return rpcResp{}, ctx.Err()
		}
		return rpcResp{}, &transport.ConnError{Machine: t.machine, Target: t.target, Stderr: "daemon write: " + err.Error()}
	}
	br := bufio.NewReaderSize(conn, 64*1024)
	line, err := br.ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return rpcResp{}, ctx.Err()
		}
		return rpcResp{}, &transport.ConnError{Machine: t.machine, Target: t.target, Stderr: "daemon read: " + err.Error()}
	}
	var resp rpcResp
	if err := json.Unmarshal(line, &resp); err != nil {
		return rpcResp{}, err
	}
	return resp, nil
}

// BootstrapSpec implements transport.SpecBootstrapper.
func (t *Transport) BootstrapSpec(ctx context.Context, factsScript, secretsRelPath, expectedFactsSHA string, reqs []transport.SpecReq) (transport.Result, []byte, bool, bool, error) {
	resp, err := t.call(ctx, rpcReq{
		Op:               "bootstrap",
		FactsScript:      factsScript,
		SecretsRel:       secretsRelPath,
		ExpectedFactsSHA: expectedFactsSHA,
		SpecReqs:         reqs,
	})
	if err != nil {
		t.observe(transport.Cmd{Label: "collect facts"}, transport.Result{Code: 255}, err)
		return transport.Result{Code: 255}, nil, false, false, err
	}
	rerr := t.decodeErr(resp)
	t.observe(transport.Cmd{Label: "collect facts"}, resp.FactsRes, rerr)
	if rerr != nil {
		return resp.FactsRes, nil, false, false, rerr
	}
	if resp.SecretsExists {
		t.observe(transport.Cmd{Label: "read ~/" + secretsRelPath, Secret: true}, transport.Result{}, nil)
	}
	t.mu.Lock()
	t.supportsSeq = resp.SupportsSeq
	if resp.MaxConcur > 0 {
		t.maxConcur = resp.MaxConcur
	}
	if resp.SHA256Cmd != "" {
		t.sha256Cmd = resp.SHA256Cmd
	}
	if resp.SpecHit && len(resp.SpecResults) > 0 {
		t.specResults = resp.SpecResults
	} else {
		t.specResults = nil
	}
	t.mu.Unlock()
	return resp.FactsRes, resp.SecretsData, resp.SecretsExists, resp.SpecHit, nil
}

// ClearSpecCache implements transport.SpecBootstrapper.
func (t *Transport) ClearSpecCache() {
	t.mu.Lock()
	t.specResults = nil
	t.mu.Unlock()
}

func (t *Transport) lookupSpec(key string) (transport.WireResp, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.specResults == nil {
		return transport.WireResp{}, false
	}
	wr, ok := t.specResults[key]
	return wr, ok
}

// Run implements transport.Transport.
func (t *Transport) Run(ctx context.Context, c transport.Cmd) (transport.Result, error) {
	if len(c.Stdin) == 0 && !c.LoginShell && !c.Interactive {
		if wr, ok := t.lookupSpec(fmt.Sprintf("exec:%t:%s", c.Sudo, c.Script)); ok {
			outB, _ := base64.StdEncoding.DecodeString(wr.StdoutB64)
			errB, _ := base64.StdEncoding.DecodeString(wr.StderrB64)
			res := transport.Result{Stdout: outB, Stderr: errB, Code: wr.Code, Duration: time.Duration(wr.DurationMS) * time.Millisecond}
			var err error
			switch {
			case wr.TimedOut:
				err = fmt.Errorf("command on %s stopped after the timeout: %w", t.machine, context.DeadlineExceeded)
			case wr.Err != "":
				err = errors.New(wr.Err)
			case res.Code == 255:
				err = &transport.ConnError{Machine: t.machine, Target: t.target, Stderr: string(res.Stderr)}
			}
			t.observe(c, res, err)
			return res, err
		}
	}
	resp, err := t.call(ctx, rpcReq{
		Op:          "run",
		Script:      c.Script,
		Stdin:       c.Stdin,
		Sudo:        c.Sudo,
		LoginShell:  c.LoginShell,
		Interactive: c.Interactive,
		TimeoutMS:   c.Timeout.Milliseconds(),
		Label:       c.Label,
		Secret:      c.Secret,
	})
	if err != nil {
		t.observe(c, transport.Result{Code: 255}, err)
		return transport.Result{Code: 255}, err
	}
	rerr := t.decodeErr(resp)
	t.observe(c, resp.Result, rerr)
	return resp.Result, rerr
}

// Stat implements transport.Transport.
func (t *Transport) Stat(ctx context.Context, path string, sudo bool) (transport.FileInfo, error) {
	if wr, ok := t.lookupSpec(fmt.Sprintf("stat:%t:%s", sudo, path)); ok {
		dur := time.Duration(wr.DurationMS) * time.Millisecond
		if wr.Err != "" {
			err := errors.New(wr.Err)
			t.observe(transport.Cmd{Label: "stat " + path, Sudo: sudo}, transport.Result{Code: 5, Duration: dur}, err)
			return transport.FileInfo{}, err
		}
		t.observe(transport.Cmd{Label: "stat " + path, Sudo: sudo}, transport.Result{Duration: dur}, nil)
		if !wr.Exists {
			return transport.FileInfo{}, nil
		}
		return transport.FileInfo{Exists: true, Type: wr.Type, Mode: os.FileMode(wr.Mode), SHA256: wr.SHA256, Target: wr.Target}, nil
	}
	resp, err := t.call(ctx, rpcReq{Op: "stat", Path: path, Sudo: sudo})
	if err != nil {
		t.observe(transport.Cmd{Label: "stat " + path, Sudo: sudo}, transport.Result{Code: 255}, err)
		return transport.FileInfo{}, err
	}
	rerr := t.decodeErr(resp)
	t.observe(transport.Cmd{Label: "stat " + path, Sudo: sudo}, transport.Result{}, rerr)
	return resp.FileInfo, rerr
}

// ReadFile implements transport.Transport.
func (t *Transport) ReadFile(ctx context.Context, path string, sudo bool) ([]byte, bool, error) {
	data, fi, err := t.ReadFileInfo(ctx, path, sudo)
	return data, fi.Exists, err
}

// ReadFileInfo implements transport.Transport.
func (t *Transport) ReadFileInfo(ctx context.Context, path string, sudo bool) ([]byte, transport.FileInfo, error) {
	if wr, ok := t.lookupSpec(fmt.Sprintf("read:%t:%s", sudo, path)); ok {
		dur := time.Duration(wr.DurationMS) * time.Millisecond
		if wr.Err != "" {
			err := errors.New(wr.Err)
			t.observe(transport.Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, transport.Result{Code: 5, Duration: dur}, err)
			return nil, transport.FileInfo{}, err
		}
		if wr.NotRegular {
			err := fmt.Errorf("%s exists and is not a regular file", path)
			t.observe(transport.Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, transport.Result{Code: 4, Duration: dur}, err)
			return nil, transport.FileInfo{Exists: true}, err
		}
		if !wr.Exists {
			t.observe(transport.Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, transport.Result{Code: 3, Duration: dur}, nil)
			return nil, transport.FileInfo{}, nil
		}
		data, err := base64.StdEncoding.DecodeString(wr.DataB64)
		if err != nil {
			return nil, transport.FileInfo{}, err
		}
		t.observe(transport.Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, transport.Result{Duration: dur}, nil)
		return data, transport.FileInfo{Exists: true, Type: wr.Type, Mode: os.FileMode(wr.Mode), SHA256: wr.SHA256}, nil
	}
	resp, err := t.call(ctx, rpcReq{Op: "read", Path: path, Sudo: sudo})
	if err != nil {
		t.observe(transport.Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, transport.Result{Code: 255}, err)
		return nil, transport.FileInfo{}, err
	}
	rerr := t.decodeErr(resp)
	t.observe(transport.Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, transport.Result{}, rerr)
	return resp.Data, resp.FileInfo, rerr
}

// WriteFile implements transport.Transport.
func (t *Transport) WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode, sudo bool) error {
	resp, err := t.call(ctx, rpcReq{Op: "write", Path: path, Data: data, Mode: mode, Sudo: sudo})
	if err != nil {
		t.observe(transport.Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, transport.Result{Code: 255}, err)
		return err
	}
	rerr := t.decodeErr(resp)
	t.observe(transport.Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, transport.Result{}, rerr)
	return rerr
}

// WriteFileVerify implements transport.VerifiedWriter.
func (t *Transport) WriteFileVerify(ctx context.Context, path string, data []byte, mode os.FileMode, sudo bool) (transport.FileInfo, error) {
	resp, err := t.call(ctx, rpcReq{Op: "write_verify", Path: path, Data: data, Mode: mode, Sudo: sudo})
	if err != nil {
		t.observe(transport.Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, transport.Result{Code: 255}, err)
		return transport.FileInfo{}, err
	}
	rerr := t.decodeErr(resp)
	t.observe(transport.Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, transport.Result{}, rerr)
	return resp.FileInfo, rerr
}

// SkillCheck implements transport.SkillOps.
func (t *Transport) SkillCheck(ctx context.Context, dir, marker string) (transport.SkillState, error) {
	if wr, ok := t.lookupSpec("skill:" + dir + "\x00" + marker); ok {
		dur := time.Duration(wr.DurationMS) * time.Millisecond
		if wr.Err != "" {
			err := errors.New(wr.Err)
			t.observe(transport.Cmd{Label: "digest of " + dir}, transport.Result{Code: 1, Duration: dur}, err)
			return transport.SkillState{}, err
		}
		t.observe(transport.Cmd{Label: "digest of " + dir}, transport.Result{Duration: dur}, nil)
		return transport.SkillState{Exists: wr.Exists, IsDir: wr.IsDir, Managed: wr.Managed, Digest: wr.Digest}, nil
	}
	resp, err := t.call(ctx, rpcReq{Op: "skill_check", Dir: dir, Marker: marker})
	if err != nil {
		t.observe(transport.Cmd{Label: "digest of " + dir}, transport.Result{Code: 255}, err)
		return transport.SkillState{}, err
	}
	rerr := t.decodeErr(resp)
	t.observe(transport.Cmd{Label: "digest of " + dir}, transport.Result{}, rerr)
	return resp.SkillState, rerr
}

// SkillWrite implements transport.SkillOps.
func (t *Transport) SkillWrite(ctx context.Context, dir, marker string, tarball []byte) error {
	resp, err := t.call(ctx, rpcReq{Op: "skill_write", Dir: dir, Marker: marker, Data: tarball})
	if err != nil {
		t.observe(transport.Cmd{Label: "copy skill " + filepath.Base(dir), Stdin: tarball}, transport.Result{Code: 255}, err)
		return err
	}
	if resp.SkillFallbk {
		return transport.ErrUseFallbackSkillWrite()
	}
	rerr := t.decodeErr(resp)
	t.observe(transport.Cmd{Label: "copy skill " + filepath.Base(dir), Stdin: tarball}, transport.Result{}, rerr)
	return rerr
}

// ApplySeq implements transport.SeqApplier with live per-item streaming.
func (t *Transport) ApplySeq(ctx context.Context, ops []transport.ApplyOp, onResult func(transport.ApplyOpResult)) error {
	req := rpcReq{
		Op:        "apply_seq",
		Machine:   t.machine,
		Target:    t.target,
		SSHConfig: t.sshConfig,
		Ops:       ops,
	}
	t.mu.Lock()
	req.OS = t.osName
	t.mu.Unlock()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", t.sock)
	if err != nil {
		return &transport.ConnError{Machine: t.machine, Target: t.target, Stderr: "daemon connect: " + err.Error()}
	}
	defer conn.Close()

	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			_ = conn.Close()
		}
	}()
	defer close(done)

	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := conn.Write(b); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &transport.ConnError{Machine: t.machine, Target: t.target, Stderr: "daemon write: " + err.Error()}
	}
	byIdx := make(map[int]transport.ApplyOp, len(ops))
	for _, op := range ops {
		byIdx[op.Index] = op
	}
	br := bufio.NewReaderSize(conn, 64*1024)
	for {
		line, rerr := br.ReadBytes('\n')
		if rerr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &transport.ConnError{Machine: t.machine, Target: t.target, Stderr: "daemon stream: " + rerr.Error()}
		}
		var resp rpcResp
		if err := json.Unmarshal(line, &resp); err != nil {
			return err
		}
		if resp.Item != nil {
			it := *resp.Item
			op := byIdx[it.Index]
			var opErr error
			code := 0
			if it.Err != "" {
				opErr = errors.New(it.Err)
				code = 1
			}
			t.observe(transport.Cmd{Label: op.Label, Sudo: op.Sudo, Secret: op.Secret}, transport.Result{Code: code, Duration: time.Duration(it.DurationMS) * time.Millisecond}, opErr)
			onResult(it)
		}
		if resp.Done {
			t.mu.Lock()
			t.supportsSeq = resp.SupportsSeq
			t.mu.Unlock()
			return t.decodeErr(resp)
		}
	}
}

// RefreshSession implements transport.SessionRefresher.
func (t *Transport) RefreshSession(ctx context.Context) error {
	resp, err := t.call(ctx, rpcReq{Op: "refresh", IfNeeded: false})
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.supportsSeq = resp.SupportsSeq
	if resp.MaxConcur > 0 {
		t.maxConcur = resp.MaxConcur
	}
	t.mu.Unlock()
	return t.decodeErr(resp)
}

// RefreshSessionIfNeeded implements transport.SessionRefresher.
func (t *Transport) RefreshSessionIfNeeded(ctx context.Context) error {
	resp, err := t.call(ctx, rpcReq{Op: "refresh", IfNeeded: true})
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.supportsSeq = resp.SupportsSeq
	if resp.MaxConcur > 0 {
		t.maxConcur = resp.MaxConcur
	}
	t.mu.Unlock()
	return t.decodeErr(resp)
}

// Reconnect implements transport.Transport.
func (t *Transport) Reconnect(ctx context.Context) error {
	resp, err := t.call(ctx, rpcReq{Op: "reconnect"})
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.supportsSeq = false
	t.specResults = nil
	t.mu.Unlock()
	return t.decodeErr(resp)
}

// Close keeps the underlying daemon session warm for subsequent commands.
func (t *Transport) Close() error {
	t.ClearSpecCache()
	return nil
}
