package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// SSH runs commands on a worker with the system `ssh` command and ControlMaster connection reuse.
type SSH struct {
	Machine string
	// Target is `user@address` or `address`.
	Target string
	// ConfigFile is given to `ssh -F` when it is not empty.
	ConfigFile string
	Observer   Observer
	files
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
	args = append(args,
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+filepath.Join(dir, "%C"),
		"-o", "ControlPersist=120",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=20",
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

// Reconnect implements Transport. It closes the ControlMaster connection.
func (s *SSH) Reconnect(ctx context.Context) error { return s.control(ctx, "exit") }

// Close implements Transport.
func (s *SSH) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.control(ctx, "exit")
}
