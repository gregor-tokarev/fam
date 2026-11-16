// Package transport runs commands on a machine: over SSH for workers, locally on the cockpit.
//
// Both implementations run the same POSIX shell scripts, so the task logic is the same.
package transport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Cmd is one command for a machine.
type Cmd struct {
	// Script is a POSIX shell script. FAM runs it with `sh -c`.
	Script string
	// Stdin is data for the script. FAM never logs it. Stdin must be empty when Sudo is true.
	Stdin []byte
	// Sudo runs the script as root: `sudo -n` on workers, `sudo -S` on the cockpit.
	Sudo bool
	// LoginShell runs Script with the login shell of the user, like a new SSH session does.
	// FAM does not wrap the script in `sh -c`.
	LoginShell bool
	// Interactive runs the login shell as an interactive shell (`-i`) with a terminal.
	Interactive bool
	// Timeout stops the command. Zero means no timeout.
	Timeout time.Duration
	// Label describes the command in the run log.
	Label string
	// Secret keeps stdout out of the run log, for example the content of a file.
	Secret bool
}

// Result is the result of one command.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	Code     int
	Duration time.Duration
}

// Out returns stdout as a trimmed string.
func (r Result) Out() string { return strings.TrimSpace(string(r.Stdout)) }

// Err returns stderr as a trimmed string.
func (r Result) Err() string { return strings.TrimSpace(string(r.Stderr)) }

// Transport runs commands on one machine.
type Transport interface {
	// Name is the machine name.
	Name() string
	// Kind is "ssh" or "local".
	Kind() string
	// Run runs a command. A connection error returns a *ConnError. A nonzero exit code is not an error.
	Run(ctx context.Context, c Cmd) (Result, error)
	// ReadFile reads a regular file. exists is false when the path does not exist.
	ReadFile(ctx context.Context, path string, sudo bool) (data []byte, exists bool, err error)
	// WriteFile writes a file atomically: a temporary file, then a rename.
	WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode, sudo bool) error
	// Stat returns information about a path. It does not follow a symbolic link.
	Stat(ctx context.Context, path string, sudo bool) (FileInfo, error)
	// SetOS selects OS-specific commands, for example `sha256sum` or `shasum -a 256`.
	SetOS(goos string)
	// SHA256Cmd returns the command that prints the SHA-256 of stdin on this machine.
	SHA256Cmd() string
	// Reconnect closes the shared connection, so that the next command opens a new one.
	Reconnect(ctx context.Context) error
	// Close closes the shared connection.
	Close() error
}

// ConnError is a connection error: `ssh` exited with code 255.
type ConnError struct {
	Machine string
	Target  string
	Stderr  string
}

func (e *ConnError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = "no message from ssh"
	}
	return fmt.Sprintf("connection error to %s (%s): ssh exit code 255: %s", e.Machine, e.Target, msg)
}

// IsConnError reports whether err is a connection error.
func IsConnError(err error) bool {
	var ce *ConnError
	return errors.As(err, &ce)
}

// Observer receives every command for the run log.
type Observer func(machine string, c Cmd, r Result, err error)

// Quote quotes s for a POSIX shell.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
