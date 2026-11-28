package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

var errDaemonUnavailable = errors.New("local session daemon unavailable")

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

var ensureMu sync.Mutex

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
	if ping(sock) {
		return sock, true
	}
	// Serialize daemon startup across concurrent CLI processes so only one daemon is spawned.
	lf, err := os.OpenFile(sock+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", false
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return "", false
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	if ping(sock) {
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
	line, err := bufio.NewReader(c).ReadBytes('\n')
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

func acquireLease(ctx context.Context, sock string) (net.Conn, bool) {
	var d net.Dialer
	d.Timeout = 200 * time.Millisecond
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, false
	}
	_ = c.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := c.Write([]byte("{\"op\":\"lease\"}\n")); err != nil {
		_ = c.Close()
		return nil, false
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		_ = c.Close()
		return nil, false
	}
	var resp rpcResp
	if json.Unmarshal(line, &resp) != nil || !resp.OK {
		_ = c.Close()
		return nil, false
	}
	_ = c.SetDeadline(time.Time{})
	return c, true
}

// Pool implements transport.SessionPool backed by the short-lived local daemon.
type Pool struct{}

// NewPool returns a SessionPool if the local daemon is enabled, or nil if disabled.
func NewPool() transport.SessionPool {
	if os.Getenv("FAM_NO_DAEMON") == "1" {
		return nil
	}
	return &Pool{}
}

func dialAndSend(ctx context.Context, sock string, req rpcReq) (net.Conn, *bufio.Reader, func(), error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			_ = conn.Close()
		}
	}()
	cleanup := func() {
		close(done)
		_ = conn.Close()
	}
	b, err := json.Marshal(req)
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	b = append(b, '\n')
	if _, err := conn.Write(b); err != nil {
		cleanup()
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, err
	}
	return conn, bufio.NewReaderSize(conn, 64*1024), cleanup, nil
}

func decodeRPCErr(machine, target string, resp rpcResp) error {
	if resp.Err == "" {
		return nil
	}
	if resp.ConnErr {
		return &transport.ConnError{Machine: machine, Target: target, Stderr: resp.ConnStderr}
	}
	if resp.IdentityErr {
		return &transport.IdentityError{Msg: resp.Err}
	}
	return errors.New(resp.Err)
}

func (p *Pool) callOnce(ctx context.Context, sock string, req rpcReq) (rpcResp, error) {
	_, br, cleanup, err := dialAndSend(ctx, sock, req)
	if err != nil {
		return rpcResp{}, err
	}
	defer cleanup()
	line, err := br.ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return rpcResp{}, ctx.Err()
		}
		return rpcResp{}, err
	}
	var resp rpcResp
	if err := json.Unmarshal(line, &resp); err != nil {
		return rpcResp{}, err
	}
	return resp, nil
}

// WarmBootstrap implements transport.SessionPool.
func (p *Pool) WarmBootstrap(ctx context.Context, machine, target, sshConfig, expectedKey, factsScript, secretsRel, expectedSHA string, reqs []transport.SpecReq) (transport.BootstrapResult, transport.WireConn, io.Closer, error) {
	var sock string
	var lease net.Conn
	for attempt := 0; attempt < 2; attempt++ {
		var ok bool
		sock, ok = Ensure(ctx)
		if !ok {
			return transport.BootstrapResult{}, nil, nil, errDaemonUnavailable
		}
		if c, lok := acquireLease(ctx, sock); lok {
			lease = c
			break
		}
	}
	if lease == nil {
		return transport.BootstrapResult{}, nil, nil, errDaemonUnavailable
	}

	resp, err := p.callOnce(ctx, sock, rpcReq{
		Op:               "bootstrap",
		Machine:          machine,
		Target:           target,
		SSHConfig:        sshConfig,
		SessionKey:       expectedKey,
		FactsScript:      factsScript,
		SecretsRel:       secretsRel,
		ExpectedFactsSHA: expectedSHA,
		SpecReqs:         reqs,
	})
	if err != nil {
		_ = lease.Close()
		if ctx.Err() != nil {
			return transport.BootstrapResult{}, nil, nil, ctx.Err()
		}
		return transport.BootstrapResult{}, nil, nil, errDaemonUnavailable
	}
	if rerr := decodeRPCErr(machine, target, resp); rerr != nil {
		_ = lease.Close()
		return transport.BootstrapResult{}, nil, nil, rerr
	}
	var w transport.WireConn
	if resp.HasWire {
		w = &daemonWire{sock: sock, machine: machine, target: target, sshConfig: sshConfig, sessionKey: resp.SessionKey, controlSock: resp.ControlSock}
	}
	return transport.BootstrapResult{
		FactsRes:      resp.FactsRes,
		SecretsData:   resp.SecretsData,
		SecretsExists: resp.SecretsExists,
		SpecHit:       resp.SpecHit,
		SpecResults:   resp.SpecResults,
		HasWire:       resp.HasWire,
		SessionKey:    resp.SessionKey,
		ControlSock:   resp.ControlSock,
	}, w, lease, nil
}

// Refresh implements transport.SessionPool.
func (p *Pool) Refresh(ctx context.Context, machine, target, sshConfig, sessionKey, controlSock string) (transport.WireConn, string, bool, error) {
	sock, ok := Ensure(ctx)
	if !ok {
		return nil, "", false, &transport.ConnError{Machine: machine, Target: target, Stderr: errDaemonUnavailable.Error()}
	}
	resp, err := p.callOnce(ctx, sock, rpcReq{
		Op:          "refresh",
		Machine:     machine,
		Target:      target,
		SSHConfig:   sshConfig,
		SessionKey:  sessionKey,
		ControlSock: controlSock,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", false, ctx.Err()
		}
		return nil, "", false, &transport.ConnError{Machine: machine, Target: target, Stderr: "daemon refresh: " + err.Error()}
	}
	if rerr := decodeRPCErr(machine, target, resp); rerr != nil {
		return nil, "", false, rerr
	}
	if sessionKey != "" && resp.SessionKey != sessionKey {
		return nil, "", false, &transport.ConnError{Machine: machine, Target: target, Stderr: "daemon refresh returned mismatched session key"}
	}
	if !resp.HasWire {
		return nil, resp.ControlSock, false, nil
	}
	return &daemonWire{sock: sock, machine: machine, target: target, sshConfig: sshConfig, sessionKey: resp.SessionKey, controlSock: resp.ControlSock}, resp.ControlSock, true, nil
}

// Reconnect implements transport.SessionPool.
func (p *Pool) Reconnect(ctx context.Context, machine, target, sshConfig, sessionKey, controlSock string) error {
	sock, _, ok := socketPath()
	if !ok {
		return nil
	}
	resp, err := p.callOnce(ctx, sock, rpcReq{
		Op:          "reconnect",
		Machine:     machine,
		Target:      target,
		SSHConfig:   sshConfig,
		SessionKey:  sessionKey,
		ControlSock: controlSock,
	})
	if err != nil {
		return nil
	}
	return decodeRPCErr(machine, target, resp)
}

// daemonWire relays raw WireReq / WireResp frames to the daemon's pooled sshWire,
// bound to both the verified sessionKey and the exact ControlMaster socket instance.
type daemonWire struct {
	sock        string
	machine     string
	target      string
	sshConfig   string
	sessionKey  string
	controlSock string
}

func (w *daemonWire) Call(ctx context.Context, req transport.WireReq) (transport.WireResp, error) {
	_, br, cleanup, err := dialAndSend(ctx, w.sock, rpcReq{
		Op:          "wire",
		Machine:     w.machine,
		Target:      w.target,
		SSHConfig:   w.sshConfig,
		SessionKey:  w.sessionKey,
		ControlSock: w.controlSock,
		Wire:        &req,
	})
	if err != nil {
		if ctx.Err() != nil {
			return transport.WireResp{}, ctx.Err()
		}
		return transport.WireResp{}, &transport.ConnError{Machine: w.machine, Target: w.target, Stderr: "daemon wire: " + err.Error()}
	}
	defer cleanup()
	line, err := br.ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return transport.WireResp{}, ctx.Err()
		}
		return transport.WireResp{}, &transport.ConnError{Machine: w.machine, Target: w.target, Stderr: "daemon wire read: " + err.Error()}
	}
	var resp rpcResp
	if err := json.Unmarshal(line, &resp); err != nil {
		return transport.WireResp{}, err
	}
	if rerr := decodeRPCErr(w.machine, w.target, resp); rerr != nil {
		return transport.WireResp{}, rerr
	}
	if resp.Wire == nil {
		return transport.WireResp{}, nil
	}
	return *resp.Wire, nil
}

func (w *daemonWire) CallStream(ctx context.Context, req transport.WireReq, onResp func(transport.WireResp)) error {
	_, br, cleanup, err := dialAndSend(ctx, w.sock, rpcReq{
		Op:          "wire_stream",
		Machine:     w.machine,
		Target:      w.target,
		SSHConfig:   w.sshConfig,
		SessionKey:  w.sessionKey,
		ControlSock: w.controlSock,
		Wire:        &req,
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &transport.ConnError{Machine: w.machine, Target: w.target, Stderr: "daemon stream: " + err.Error()}
	}
	defer cleanup()
	for {
		line, rerr := br.ReadBytes('\n')
		if rerr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &transport.ConnError{Machine: w.machine, Target: w.target, Stderr: "daemon stream read: " + rerr.Error()}
		}
		var resp rpcResp
		if err := json.Unmarshal(line, &resp); err != nil {
			return err
		}
		if resp.Wire != nil {
			onResp(*resp.Wire)
		}
		if resp.Done {
			return decodeRPCErr(w.machine, w.target, resp)
		}
	}
}

func (w *daemonWire) Close() error { return nil }
