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
	// ReadFileInfo reads a regular file and returns its FileInfo (including Mode) in one step.
	ReadFileInfo(ctx context.Context, path string, sudo bool) (data []byte, info FileInfo, err error)
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

// SkillState describes a skill directory on a machine.
type SkillState struct {
	Exists  bool
	IsDir   bool
	Managed bool
	Digest  string
}

// SkillOps is implemented by transports that can check and install skill folders natively.
type SkillOps interface {
	HasNativeSkillOps() bool
	SkillCheck(ctx context.Context, dir, marker string) (SkillState, error)
	SkillWrite(ctx context.Context, dir, marker string, tarball []byte) error
}

// ConcurrencyProvider reports how many read-only operations a transport can run concurrently.
type ConcurrencyProvider interface {
	MaxConcurrency() int
}

// PrimOp is one Go-planned dumb remote primitive executed sequentially in a batch.
// All task semantics, content rendering, conflict checks, and error messages stay in Go;
// the remote worker only validates the CAS precondition and applies the primitive.
type PrimOp struct {
	Index int    `json:"index"`
	Op    string `json:"op"` // "write", "symlink", or "skill_write"

	// Common observation metadata (not sent over wire).
	Label  string `json:"-"`
	Secret bool   `json:"-"`

	// Target & payload fields depending on Op.
	Path         string  `json:"path,omitempty"`
	Target       string  `json:"target,omitempty"`
	Dir          string  `json:"dir,omitempty"`
	Marker       string  `json:"marker,omitempty"`
	DataB64      string  `json:"data_b64,omitempty"`
	TarB64       string  `json:"tar_b64,omitempty"`
	Mode         *uint32 `json:"mode,omitempty"`
	Sudo         bool    `json:"sudo,omitempty"`
	Verify       bool    `json:"verify,omitempty"`
	StopAfter    bool    `json:"stop_after,omitempty"`
	IfMissing    bool    `json:"if_missing,omitempty"`
	IfSHA256     string  `json:"if_sha256,omitempty"`
	IfLinkTarget *string `json:"if_link_target,omitempty"`
	IfDigest     string  `json:"if_digest,omitempty"`
}

// PrimResult is the remote outcome of one PrimOp in a sequential primitive batch.
type PrimResult struct {
	Index      int    `json:"index"`
	Stale      bool   `json:"stale,omitempty"`
	Err        string `json:"err,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Exists     bool   `json:"exists,omitempty"`
	Type       string `json:"type,omitempty"`
	Target     string `json:"target,omitempty"`
	Mode       uint32 `json:"mode,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	IsDir      bool   `json:"is_dir,omitempty"`
	Managed    bool   `json:"managed,omitempty"`
	Digest     string `json:"digest,omitempty"`
}

// PrimBatcher executes a sequence of Go-planned dumb primitives in one remote round trip,
// stopping immediately on any CAS mismatch ("stale") or error so Go falls back cleanly.
type PrimBatcher interface {
	SupportsPrimBatch() bool
	BatchPrims(ctx context.Context, ops []PrimOp, onResult func(PrimResult)) error
}

// SessionRefresher restarts a persistent remote session so later commands inherit updated
// login environment files (such as ~/.config/fam/env.sh, secrets.env, or ~/.bashrc).
type SessionRefresher interface {
	RefreshSession(ctx context.Context) error
	// RefreshSessionIfNeeded restarts the session only if watched shell startup files changed.
	RefreshSessionIfNeeded(ctx context.Context) error
}

// SpecReq describes one speculative read-only remote check pipelined into Bootstrap.
type SpecReq struct {
	Key       string `json:"key"`
	Op        string `json:"op"` // stat, read, skill_check, exec
	Path      string `json:"path,omitempty"`
	Dir       string `json:"dir,omitempty"`
	Marker    string `json:"marker,omitempty"`
	Script    string `json:"script,omitempty"`
	Sudo      bool   `json:"sudo,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
}

// Canonical speculative-check lookup keys shared by engine and transport.
func SpecKeyStat(sudo bool, path string) string   { return fmt.Sprintf("stat:%t:%s", sudo, path) }
func SpecKeyRead(sudo bool, path string) string   { return fmt.Sprintf("read:%t:%s", sudo, path) }
func SpecKeySkill(dir, marker string) string      { return "skill:" + dir + "\x00" + marker }
func SpecKeyExec(sudo bool, script string) string { return fmt.Sprintf("exec:%t:%s", sudo, script) }

// BootstrapResult holds the initial facts, secrets, and speculative check status from BootstrapSpec.
type BootstrapResult struct {
	FactsRes      Result
	SecretsData   []byte
	SecretsExists bool
	SpecHit       bool
	SpecResults   map[string]WireResp
	HasWire       bool
	SessionKey    string
	ControlSock   string
}

// IdentityError reports that SSH destination identity could not be verified or drifted.
type IdentityError struct {
	Msg string
}

func (e *IdentityError) Error() string { return e.Msg }

// SpecBootstrapper establishes the persistent session, returns facts + secrets, and optionally
// runs a speculative read-only check batch gated on the SHA-256 of the live facts output.
type SpecBootstrapper interface {
	BootstrapSpec(ctx context.Context, factsScript, secretsRelPath, expectedFactsSHA string, reqs []SpecReq) (BootstrapResult, error)
	ClearSpecCache()
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
