package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Local runs commands on the cockpit.
type Local struct {
	Machine string
	// Shell is the login shell of the user. FAM uses it for LoginShell commands.
	Shell string
	// SudoPassword returns the sudo password of the cockpit. FAM gives it to `sudo -S`.
	SudoPassword func(ctx context.Context) ([]byte, error)
	Observer     Observer
	files
}

// NewLocal returns the local transport of the cockpit.
func NewLocal(machine string, sudoPassword func(ctx context.Context) ([]byte, error), obs Observer) *Local {
	l := &Local{Machine: machine, SudoPassword: sudoPassword, Observer: obs}
	l.files.run = l.Run
	return l
}

// Name implements Transport.
func (l *Local) Name() string { return l.Machine }

// Kind implements Transport.
func (l *Local) Kind() string { return "local" }

// Run implements Transport.
func (l *Local) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Sudo && len(c.Stdin) > 0 {
		return Result{}, errors.New("internal error: a sudo command cannot read stdin")
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	var cmd *exec.Cmd
	stdin := c.Stdin
	switch {
	case c.LoginShell:
		shell := l.Shell
		if shell == "" {
			shell = "/bin/sh"
		}
		// A fresh shell on the cockpit is a login shell (like a new terminal or a session that
		// an agent starts): it reads ~/.profile or ~/.zprofile, where FAM loads its environment.
		args := []string{"-l", "-c", c.Script}
		if c.Interactive {
			args = []string{"-i", "-c", c.Script}
		}
		cmd = exec.CommandContext(ctx, shell, args...)
		// A fresh shell: only the variables that a login gives.
		home, _ := os.UserHomeDir()
		user := os.Getenv("USER")
		cmd.Env = []string{
			"HOME=" + home, "USER=" + user, "LOGNAME=" + user, "SHELL=" + shell,
			"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "TERM=dumb",
		}
	case c.Sudo:
		if l.SudoPassword == nil {
			return Result{}, errors.New("sudo on the cockpit needs a password, and no password source is set")
		}
		pw, err := l.SudoPassword(ctx)
		if err != nil {
			return Result{}, err
		}
		if pw == nil {
			// sudo needs no password here.
			cmd = exec.CommandContext(ctx, "sudo", "-n", "sh", "-c", c.Script)
			break
		}
		// -k ignores a cached sudo timestamp, so sudo always reads the password from stdin and
		// never passes it on to the command.
		stdin = append(append([]byte{}, pw...), '\n')
		cmd = exec.CommandContext(ctx, "sudo", "-k", "-S", "-p", "", "sh", "-c", c.Script)
	default:
		cmd = exec.CommandContext(ctx, "sh", "-c", c.Script)
	}
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Duration: time.Since(start)}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			err = fmt.Errorf("run command: %w", err)
			l.observe(c, res, err)
			return res, err
		}
		res.Code = ee.ExitCode()
		if res.Code == -1 && ctx.Err() != nil {
			err = fmt.Errorf("command stopped after the timeout: %w", ctx.Err())
			l.observe(c, res, err)
			return res, err
		}
	}
	l.observe(c, res, nil)
	return res, nil
}

func (l *Local) observe(c Cmd, r Result, err error) {
	if l.Observer != nil {
		l.Observer(l.Machine, c, r, err)
	}
}

// Reconnect implements Transport. A local transport has no connection.
func (l *Local) Reconnect(context.Context) error { return nil }

// Close implements Transport.
func (l *Local) Close() error { return nil }
