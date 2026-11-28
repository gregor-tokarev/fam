package transport

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// SSH runs commands on a worker with the system `ssh` command and ControlMaster connection reuse.
// When python3 >= 3.6 is available on the worker, SSH keeps a single persistent multiplexed
// `ssh -T` worker process alive for the run to avoid per-command SSH channel handshakes and forks.
type SSH struct {
	Machine string
	// Target is `user@address` or `address`.
	Target string
	// ConfigFile is given to `ssh -F` when it is not empty.
	ConfigFile     string
	ConnectTimeout time.Duration
	Observer       Observer
	files

	wireMu sync.Mutex
	wire   *sshWire
	hasPy3 bool
}

// NewSSH returns an SSH transport for one worker.
func NewSSH(machine, target, configFile string, obs Observer) *SSH {
	s := &SSH{Machine: machine, Target: target, ConfigFile: configFile, Observer: obs}
	s.files.run = s.Run
	return s
}

func controlDir() (string, error) {
	// Keep the socket path short, including on macOS with a long TMPDIR.
	dir := filepath.Join("/tmp", fmt.Sprintf("fam-%d", os.Getuid()))
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("SSH control directory %s must be a directory, not a symlink", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("SSH control directory %s must belong to the current user and have mode 0700", dir)
	}
	return dir, nil
}

func (s *SSH) baseArgs() ([]string, error) {
	dir, err := controlDir()
	if err != nil {
		return nil, err
	}
	var args []string
	if s.ConfigFile != "" {
		args = append(args, "-F", s.ConfigFile)
	}
	timeout := 20
	if s.ConnectTimeout > 0 {
		timeout = int(s.ConnectTimeout.Seconds())
	}
	args = append(args,
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+filepath.Join(dir, "%C"),
		"-o", "ControlPersist=120",
		"-o", "BatchMode=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", timeout),
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
	)
	return args, nil
}

// Name implements Transport.
func (s *SSH) Name() string { return s.Machine }

// Kind implements Transport.
func (s *SSH) Kind() string { return "ssh" }

// Run implements Transport.
func (s *SSH) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Sudo && len(c.Stdin) > 0 {
		return Result{}, errors.New("internal error: a sudo command cannot read stdin")
	}
	if !c.LoginShell && !c.Interactive {
		if w := s.getWire(); w != nil {
			return s.runWire(ctx, w, c)
		}
	}
	return s.runDirect(ctx, c)
}

func (s *SSH) runWire(ctx context.Context, w *sshWire, c Cmd) (Result, error) {
	req := wireReq{
		Op:        "exec",
		Script:    c.Script,
		Sudo:      c.Sudo,
		TimeoutMS: c.Timeout.Milliseconds(),
	}
	if len(c.Stdin) > 0 {
		req.StdinB64 = base64.StdEncoding.EncodeToString(c.Stdin)
	}
	start := time.Now()
	resp, err := w.call(ctx, req)
	if err != nil {
		if c.Sudo && ctx.Err() != nil {
			s.closeWire()
		}
		res := Result{Duration: time.Since(start)}
		if IsConnError(err) {
			res.Code = 255
		}
		s.observe(c, res, err)
		return res, err
	}
	outB, _ := base64.StdEncoding.DecodeString(resp.StdoutB64)
	errB, _ := base64.StdEncoding.DecodeString(resp.StderrB64)
	res := Result{
		Stdout:   outB,
		Stderr:   errB,
		Code:     resp.Code,
		Duration: time.Duration(resp.DurationMS) * time.Millisecond,
	}
	if resp.TimedOut {
		err = fmt.Errorf("command on %s stopped after the timeout: %w", s.Machine, context.DeadlineExceeded)
		s.observe(c, res, err)
		return res, err
	}
	if resp.Err != "" {
		err = errors.New(resp.Err)
		s.observe(c, res, err)
		return res, err
	}
	if res.Code == 255 {
		err = &ConnError{Machine: s.Machine, Target: s.Target, Stderr: string(res.Stderr)}
		s.observe(c, res, err)
		return res, err
	}
	s.observe(c, res, nil)
	return res, nil
}

// Stat overrides files.Stat to execute in-process on the remote persistent worker when active.
func (s *SSH) Stat(ctx context.Context, path string, sudo bool) (FileInfo, error) {
	w := s.getWire()
	if w == nil {
		return s.files.Stat(ctx, path, sudo)
	}
	start := time.Now()
	resp, err := w.call(ctx, wireReq{Op: "stat", Path: path, Sudo: sudo})
	if err != nil {
		s.observe(Cmd{Label: "stat " + path, Sudo: sudo}, Result{Duration: time.Since(start)}, err)
		return FileInfo{}, err
	}
	dur := time.Duration(resp.DurationMS) * time.Millisecond
	if resp.Err != "" {
		err = errors.New(resp.Err)
		s.observe(Cmd{Label: "stat " + path, Sudo: sudo}, Result{Code: 5, Duration: dur}, err)
		return FileInfo{}, err
	}
	s.observe(Cmd{Label: "stat " + path, Sudo: sudo}, Result{Duration: dur}, nil)
	if !resp.Exists {
		return FileInfo{}, nil
	}
	return FileInfo{
		Exists: true,
		Type:   resp.Type,
		Mode:   os.FileMode(resp.Mode),
		SHA256: resp.SHA256,
		Target: resp.Target,
	}, nil
}

// ReadFile overrides files.ReadFile to execute in-process on the remote persistent worker.
func (s *SSH) ReadFile(ctx context.Context, path string, sudo bool) ([]byte, bool, error) {
	if s.getWire() == nil {
		return s.files.ReadFile(ctx, path, sudo)
	}
	data, info, err := s.ReadFileInfo(ctx, path, sudo)
	return data, info.Exists, err
}

// ReadFileInfo overrides files.ReadFileInfo to read content + mode in a single RPC.
func (s *SSH) ReadFileInfo(ctx context.Context, path string, sudo bool) ([]byte, FileInfo, error) {
	w := s.getWire()
	if w == nil {
		return s.files.ReadFileInfo(ctx, path, sudo)
	}
	start := time.Now()
	resp, err := w.call(ctx, wireReq{Op: "read", Path: path, Sudo: sudo})
	if err != nil {
		s.observe(Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, Result{Duration: time.Since(start)}, err)
		return nil, FileInfo{}, err
	}
	dur := time.Duration(resp.DurationMS) * time.Millisecond
	if resp.Err != "" {
		err = errors.New(resp.Err)
		s.observe(Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, Result{Code: 5, Duration: dur}, err)
		return nil, FileInfo{}, err
	}
	if resp.NotRegular {
		err = fmt.Errorf("%s exists and is not a regular file", path)
		s.observe(Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, Result{Code: 4, Duration: dur}, err)
		return nil, FileInfo{Exists: true}, err
	}
	if !resp.Exists {
		s.observe(Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, Result{Code: 3, Duration: dur}, nil)
		return nil, FileInfo{}, nil
	}
	data, err := base64.StdEncoding.DecodeString(resp.DataB64)
	if err != nil {
		return nil, FileInfo{}, fmt.Errorf("decode read %s: %w", path, err)
	}
	s.observe(Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, Result{Duration: dur}, nil)
	return data, FileInfo{
		Exists: true,
		Type:   resp.Type,
		Mode:   os.FileMode(resp.Mode),
		SHA256: resp.SHA256,
	}, nil
}

// WriteFile overrides files.WriteFile to write atomically in a single RPC.
func (s *SSH) WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode, sudo bool) error {
	w := s.getWire()
	if w == nil {
		return s.files.WriteFile(ctx, path, data, mode, sudo)
	}
	start := time.Now()
	m := uint32(mode.Perm())
	resp, err := w.call(ctx, wireReq{
		Op:      "write",
		Path:    path,
		DataB64: base64.StdEncoding.EncodeToString(data),
		Mode:    &m,
		Sudo:    sudo,
	})
	if err != nil {
		s.observe(Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, Result{Duration: time.Since(start)}, err)
		return err
	}
	dur := time.Duration(resp.DurationMS) * time.Millisecond
	if resp.Err != "" {
		err = errors.New(resp.Err)
		s.observe(Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, Result{Code: 1, Duration: dur}, err)
		return err
	}
	s.observe(Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, Result{Duration: dur}, nil)
	return nil
}

// WriteFileVerify implements VerifiedWriter in a single RPC on the remote persistent worker.
func (s *SSH) WriteFileVerify(ctx context.Context, path string, data []byte, mode os.FileMode, sudo bool) (FileInfo, error) {
	w := s.getWire()
	if w == nil {
		if err := s.files.WriteFile(ctx, path, data, mode, sudo); err != nil {
			return FileInfo{}, err
		}
		return s.files.Stat(ctx, path, sudo)
	}
	start := time.Now()
	m := uint32(mode.Perm())
	resp, err := w.call(ctx, wireReq{
		Op:      "write",
		Path:    path,
		DataB64: base64.StdEncoding.EncodeToString(data),
		Mode:    &m,
		Sudo:    sudo,
		Verify:  true,
	})
	if err != nil {
		s.observe(Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, Result{Duration: time.Since(start)}, err)
		return FileInfo{}, err
	}
	dur := time.Duration(resp.DurationMS) * time.Millisecond
	if resp.Err != "" {
		err = errors.New(resp.Err)
		s.observe(Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, Result{Code: 1, Duration: dur}, err)
		return FileInfo{}, err
	}
	s.observe(Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, Result{Duration: dur}, nil)
	return FileInfo{
		Exists: resp.Exists,
		Type:   resp.Type,
		Mode:   os.FileMode(resp.Mode),
		SHA256: resp.SHA256,
		Target: resp.Target,
	}, nil
}

// SkillCheck implements SkillOps in a single in-process RPC when the remote worker is active.
func (s *SSH) SkillCheck(ctx context.Context, dir, marker string) (SkillState, error) {
	w := s.getWire()
	if w == nil {
		return s.skillCheckFallback(ctx, dir, marker)
	}
	start := time.Now()
	resp, err := w.call(ctx, wireReq{Op: "skill_check", Dir: dir, Marker: marker})
	if err != nil {
		s.observe(Cmd{Label: "digest of " + dir}, Result{Duration: time.Since(start)}, err)
		return SkillState{}, err
	}
	dur := time.Duration(resp.DurationMS) * time.Millisecond
	if resp.Err != "" {
		err = errors.New(resp.Err)
		s.observe(Cmd{Label: "digest of " + dir}, Result{Code: 1, Duration: dur}, err)
		return SkillState{}, err
	}
	s.observe(Cmd{Label: "digest of " + dir}, Result{Duration: dur}, nil)
	return SkillState{
		Exists:  resp.Exists,
		IsDir:   resp.IsDir,
		Managed: resp.Managed,
		Digest:  resp.Digest,
	}, nil
}

func (s *SSH) skillCheckFallback(ctx context.Context, dir, marker string) (SkillState, error) {
	fi, err := s.Stat(ctx, dir, false)
	if err != nil {
		return SkillState{}, err
	}
	if !fi.Exists {
		return SkillState{}, nil
	}
	if fi.Type != "dir" {
		return SkillState{Exists: true, IsDir: false}, nil
	}
	m, err := s.Stat(ctx, dir+"/"+marker, false)
	if err != nil {
		return SkillState{}, err
	}
	if !m.Exists {
		return SkillState{Exists: true, IsDir: true, Managed: false}, nil
	}
	// Let the engine compute the shell digest if wire is unavailable.
	return SkillState{Exists: true, IsDir: true, Managed: true, Digest: ""}, nil
}

// SkillWrite implements SkillOps in a single in-process RPC when the remote worker is active.
func (s *SSH) SkillWrite(ctx context.Context, dir, marker string, tarball []byte) error {
	w := s.getWire()
	if w == nil {
		return errUseFallbackSkillWrite
	}
	start := time.Now()
	resp, err := w.call(ctx, wireReq{
		Op:     "skill_write",
		Dir:    dir,
		Marker: marker,
		TarB64: base64.StdEncoding.EncodeToString(tarball),
	})
	if err != nil {
		s.observe(Cmd{Label: "copy skill " + filepath.Base(dir), Stdin: tarball}, Result{Duration: time.Since(start)}, err)
		return err
	}
	dur := time.Duration(resp.DurationMS) * time.Millisecond
	if resp.Err != "" {
		err = errors.New(resp.Err)
		s.observe(Cmd{Label: "copy skill " + filepath.Base(dir), Stdin: tarball}, Result{Code: 1, Duration: dur}, err)
		return err
	}
	s.observe(Cmd{Label: "copy skill " + filepath.Base(dir), Stdin: tarball}, Result{Duration: dur}, nil)
	return nil
}

// ErrSkillFallback signals SkillTask to fall back to shell execution when wire is unavailable.
var errUseFallbackSkillWrite = errors.New("use shell skill write")

// IsSkillFallback reports whether err asks the caller to use shell skill operations.
func IsSkillFallback(err error) bool { return errors.Is(err, errUseFallbackSkillWrite) }

func (s *SSH) runDirect(ctx context.Context, c Cmd) (Result, error) {
	args, err := s.baseArgs()
	if err != nil {
		return Result{}, err
	}
	var remote string
	switch {
	case c.LoginShell && c.Interactive:
		args = append(args, "-tt")
		remote = `exec "$SHELL" -i -c ` + Quote(c.Script)
	case c.LoginShell:
		args = append(args, "-T")
		remote = c.Script
	case c.Sudo:
		args = append(args, "-T")
		remote = "sudo -n sh -c " + Quote(c.Script)
	default:
		args = append(args, "-T")
		remote = "sh -c " + Quote(c.Script)
	}
	args = append(args, s.Target, "--", remote)
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = bytes.NewReader(c.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	err = cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Duration: time.Since(start)}
	if c.Interactive {
		res.Stdout = bytes.ReplaceAll(res.Stdout, []byte("\r\n"), []byte("\n"))
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			err = fmt.Errorf("run ssh: %w", err)
			s.observe(c, res, err)
			return res, err
		}
		res.Code = ee.ExitCode()
		if res.Code == -1 && ctx.Err() != nil {
			err = fmt.Errorf("command on %s stopped after the timeout: %w", s.Machine, ctx.Err())
			s.observe(c, res, err)
			return res, err
		}
	}
	// Exit code 255 is a connection error. Classify it before the script result.
	if res.Code == 255 {
		err := &ConnError{Machine: s.Machine, Target: s.Target, Stderr: string(res.Stderr)}
		s.observe(c, res, err)
		return res, err
	}
	s.observe(c, res, nil)
	return res, nil
}

func (s *SSH) observe(c Cmd, r Result, err error) {
	if s.Observer != nil {
		s.Observer(s.Machine, c, r, err)
	}
}

func (s *SSH) control(ctx context.Context, op string) error {
	args, err := s.baseArgs()
	if err != nil {
		return err
	}
	args = append(args, "-O", op, s.Target)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil && op == "exit" {
		// No master connection: nothing to close.
		return nil
	}
	if err != nil {
		return fmt.Errorf("ssh -O %s: %s", op, bytes.TrimSpace(out))
	}
	return nil
}

// Reconnect implements Transport. It closes the persistent worker and ControlMaster connection.
func (s *SSH) Reconnect(ctx context.Context) error {
	s.closeWire()
	return s.control(ctx, "exit")
}

// Close implements Transport. It tears down the ControlMaster immediately without waiting an
// extra network round trip for the persistent worker channel to drain.
func (s *SSH) Close() error {
	s.wireMu.Lock()
	w := s.wire
	s.wire = nil
	s.wireMu.Unlock()
	if w != nil {
		w.beginClose()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := s.control(ctx, "exit")
	if w != nil {
		w.finishClose()
	}
	return err
}
