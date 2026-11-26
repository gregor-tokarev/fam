package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type sshWorker struct {
	alias   string
	port    int
	keyPath string
	homeDir string
}

// sshCluster runs a single unprivileged local sshd behind one loopback TCP proxy
// port per worker alias. Each worker gets its own isolated HOME via an
// authorized_keys command wrapper and a fixed one-way network delay (RTT/2).
type sshCluster struct {
	user      string
	cmd       *exec.Cmd
	logFile   *os.File
	exited    chan error
	done      chan struct{}
	closeOnce sync.Once
	listeners []net.Listener
	workers   map[string]*sshWorker
}

func startSSHCluster(ctx context.Context, root string, aliases []string, rtt time.Duration) (*sshCluster, error) {
	sshdBin, err := findSSHD()
	if err != nil {
		return nil, err
	}
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("current user: %w", err)
	}
	sshdPorts, err := allocatePorts(1)
	if err != nil {
		return nil, err
	}
	sshdPort := sshdPorts[0]

	dir := filepath.Join(root, "sshd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	hostKey := filepath.Join(dir, "host_ed25519")
	if err := keygen(ctx, hostKey); err != nil {
		return nil, fmt.Errorf("host keygen: %w", err)
	}

	c := &sshCluster{
		user:    u.Username,
		exited:  make(chan error, 1),
		done:    make(chan struct{}),
		workers: make(map[string]*sshWorker, len(aliases)),
	}
	cleanupOnErr := func() { _ = c.Close() }

	var authKeys strings.Builder
	for _, alias := range aliases {
		wDir := filepath.Join(dir, "workers", alias)
		homeDir := filepath.Join(wDir, "home")
		if err := os.MkdirAll(homeDir, 0o755); err != nil {
			cleanupOnErr()
			return nil, err
		}
		keyPath := filepath.Join(wDir, "id_ed25519")
		if err := keygen(ctx, keyPath); err != nil {
			cleanupOnErr()
			return nil, fmt.Errorf("worker %s keygen: %w", alias, err)
		}
		pubBytes, err := os.ReadFile(keyPath + ".pub")
		if err != nil {
			cleanupOnErr()
			return nil, err
		}
		pub := strings.TrimSpace(string(pubBytes))

		wrapPath := filepath.Join(wDir, "wrap.sh")
		wrap := fmt.Sprintf(`#!/bin/sh
export HOME=%s
export XDG_CACHE_HOME=%s
export XDG_STATE_HOME=%s
export XDG_CONFIG_HOME=%s
unset SHLVL
cd "$HOME" || exit 1
exec "${SHELL:-/bin/sh}" -c "$SSH_ORIGINAL_COMMAND"
`,
			shQuote(homeDir),
			shQuote(filepath.Join(homeDir, ".cache")),
			shQuote(filepath.Join(homeDir, ".local", "state")),
			shQuote(filepath.Join(homeDir, ".config")),
		)
		if err := os.WriteFile(wrapPath, []byte(wrap), 0o755); err != nil {
			cleanupOnErr()
			return nil, err
		}
		fmt.Fprintf(&authKeys, "command=%q %s\n", shQuote(wrapPath), pub)

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			cleanupOnErr()
			return nil, fmt.Errorf("listen proxy for %s: %w", alias, err)
		}
		c.listeners = append(c.listeners, ln)
		c.workers[alias] = &sshWorker{
			alias:   alias,
			port:    ln.Addr().(*net.TCPAddr).Port,
			keyPath: keyPath,
			homeDir: homeDir,
		}
	}

	authKeysPath := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(authKeysPath, []byte(authKeys.String()), 0o600); err != nil {
		cleanupOnErr()
		return nil, err
	}

	cfg := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %q
PidFile none
UsePAM no
PrintMotd no
PrintLastLog no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
AuthorizedKeysFile %q
StrictModes no
AllowUsers %s
AllowTcpForwarding no
X11Forwarding no
MaxSessions 10
MaxStartups 64:30:100
LogLevel ERROR
`, sshdPort, hostKey, authKeysPath, u.Username)

	cfgPath := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		cleanupOnErr()
		return nil, err
	}

	logPath := filepath.Join(dir, "sshd.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		cleanupOnErr()
		return nil, err
	}
	c.logFile = logFile

	c.cmd = exec.Command(sshdBin, "-D", "-e", "-f", cfgPath)
	c.cmd.Stderr = logFile
	if err := c.cmd.Start(); err != nil {
		cleanupOnErr()
		return nil, fmt.Errorf("start sshd: %w", err)
	}
	go func() { c.exited <- c.cmd.Wait() }()

	if err := waitPorts(ctx, []int{sshdPort}, c.exited); err != nil {
		cleanupOnErr()
		b, _ := os.ReadFile(logPath)
		msg := strings.TrimSpace(string(b))
		if msg != "" {
			return nil, fmt.Errorf("sshd failed to start: %w\n%s", err, msg)
		}
		return nil, fmt.Errorf("sshd failed to start: %w", err)
	}

	backendAddr := fmt.Sprintf("127.0.0.1:%d", sshdPort)
	oneWay := rtt / 2
	for _, ln := range c.listeners {
		go c.serveProxy(ln, backendAddr, oneWay)
	}
	return c, nil
}

func (c *sshCluster) serveProxy(ln net.Listener, backendAddr string, oneWay time.Duration) {
	for {
		client, err := ln.Accept()
		if err != nil {
			return
		}
		go func(client net.Conn) {
			defer client.Close()
			c.handleProxyConn(client, backendAddr, oneWay)
		}(client)
	}
}

func (c *sshCluster) handleProxyConn(client net.Conn, backendAddr string, oneWay time.Duration) {
	backend, err := net.DialTimeout("tcp", backendAddr, 5*time.Second)
	if err != nil {
		return
	}
	defer backend.Close()

	if tc, ok := client.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	if tc, ok := backend.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		delayPump(client, backend, oneWay, c.done)
	}()
	go func() {
		defer wg.Done()
		delayPump(backend, client, oneWay, c.done)
	}()
	wg.Wait()
}

type delayedChunk struct {
	due  time.Time
	data []byte
}

func delayPump(src, dst net.Conn, delay time.Duration, done <-chan struct{}) {
	defer func() {
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	if delay <= 0 {
		_, _ = io.Copy(dst, src)
		return
	}

	ch := make(chan delayedChunk, 256)
	pumpDone := make(chan struct{})
	defer close(pumpDone)
	go func() {
		defer close(ch)
		buf := make([]byte, 64*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				select {
				case ch <- delayedChunk{due: time.Now().Add(delay), data: cp}:
				case <-done:
					return
				case <-pumpDone:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for chunk := range ch {
		if wait := time.Until(chunk.due); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-done:
				timer.Stop()
				return
			}
		}
		if _, err := dst.Write(chunk.data); err != nil {
			return
		}
	}
}

func (c *sshCluster) writeClientConfig(path string, aliases []string) error {
	var b strings.Builder
	for _, alias := range aliases {
		w := c.workers[alias]
		fmt.Fprintf(&b, `Host %s
  HostName 127.0.0.1
  Port %d
  User %s
  IdentityFile %q
  IdentitiesOnly yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR

`, w.alias, w.port, c.user, w.keyPath)
	}
	// Safety belt: any unknown host fails immediately.
	b.WriteString("Host *\n  HostName 127.0.0.1\n  Port 9\n  ConnectTimeout 2\n  BatchMode yes\n")
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func (c *sshCluster) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		close(c.done)
		for _, ln := range c.listeners {
			_ = ln.Close()
		}
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
			select {
			case <-c.exited:
			case <-time.After(3 * time.Second):
			}
		}
		if c.logFile != nil {
			_ = c.logFile.Close()
		}
	})
	return nil
}

func findSSHD() (string, error) {
	for _, p := range []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	p, err := exec.LookPath("sshd")
	if err != nil {
		return "", fmt.Errorf("sshd not found in /usr/sbin/sshd or PATH: %w", err)
	}
	return p, nil
}

func keygen(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func allocatePorts(n int) ([]int, error) {
	listeners := make([]net.Listener, 0, n)
	ports := make([]int, 0, n)
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("allocate port: %w", err)
		}
		listeners = append(listeners, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	return ports, nil
}

func waitPorts(ctx context.Context, ports []int, exited <-chan error) error {
	deadline := time.Now().Add(5 * time.Second)
	for _, p := range ports {
		addr := fmt.Sprintf("127.0.0.1:%d", p)
		for {
			select {
			case err := <-exited:
				if err == nil {
					return fmt.Errorf("sshd exited unexpectedly")
				}
				return err
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out waiting for %s", addr)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	return nil
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
