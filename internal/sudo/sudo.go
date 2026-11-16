// Package sudo gets the sudo password of the cockpit once and keeps it in memory.
package sudo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"golang.org/x/term"
)

// Source gives the sudo password of the cockpit.
type Source struct {
	// PasswordFile is the --sudo-password-file flag.
	PasswordFile string

	mu       sync.Mutex
	done     bool
	password []byte
	noPass   bool
	err      error
}

// ErrNoPassword means that FAM has no way to get the password.
var ErrNoPassword = errors.New("a task on the cockpit needs sudo, but sudo needs a password and FAM has no terminal. Use --sudo-password-file")

// Password returns the password for `sudo -S`. It asks only once.
// When sudo needs no password, it returns an empty password.
func (s *Source) Password(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return s.password, s.err
	}
	s.done = true
	s.password, s.err = s.get(ctx)
	return s.password, s.err
}

func (s *Source) get(ctx context.Context) ([]byte, error) {
	// No password necessary.
	if exec.CommandContext(ctx, "sudo", "-n", "true").Run() == nil {
		s.noPass = true
		return nil, nil
	}
	if s.PasswordFile != "" {
		b, err := os.ReadFile(s.PasswordFile)
		if err != nil {
			return nil, fmt.Errorf("read the sudo password file: %w", err)
		}
		pw := bytes.TrimRight(b, "\r\n")
		if err := validate(ctx, pw); err != nil {
			return nil, err
		}
		return pw, nil
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil || !term.IsTerminal(int(tty.Fd())) {
		if tty != nil {
			tty.Close()
		}
		return nil, ErrNoPassword
	}
	defer tty.Close()
	fmt.Fprint(tty, "[fam] sudo password for the cockpit: ")
	pw, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return nil, fmt.Errorf("read the sudo password: %w", err)
	}
	if err := validate(ctx, pw); err != nil {
		return nil, err
	}
	return pw, nil
}

// validate runs `sudo -S -v` with the password.
func validate(ctx context.Context, pw []byte) error {
	cmd := exec.CommandContext(ctx, "sudo", "-S", "-p", "", "-v")
	cmd.Stdin = bytes.NewReader(append(append([]byte{}, pw...), '\n'))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("the sudo password of the cockpit is not correct: %s", msg)
	}
	return nil
}
