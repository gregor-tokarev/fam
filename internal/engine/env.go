// Package engine plans, applies, and verifies the tasks of each machine.
package engine

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/gregor-tokarev/fam/internal/config"
	"github.com/gregor-tokarev/fam/internal/facts"
	"github.com/gregor-tokarev/fam/internal/secrets"
	"github.com/gregor-tokarev/fam/internal/transport"
)

// Env is the context of the tasks of one machine.
type Env struct {
	T       transport.Transport
	Facts   facts.Facts
	Machine config.Machine
	Project *Project
	// Secrets are the secrets of the cockpit. FAM shows only their names.
	Secrets []secrets.Secret
	// restarted holds the units that the last Service task restarted. The apply loop removes
	// them from the restart requests made before that task.
	restarted map[Unit]bool
}

func (e *Env) markRestarted(u Unit) {
	if e.restarted == nil {
		e.restarted = map[Unit]bool{}
	}
	e.restarted[u] = true
}

// Project holds the directories of the project.
type Project struct {
	Base            string
	ConfigPath      string
	InstructionsDir string
	SkillsDir       string
}

// FAM-owned paths on each machine, relative to the home directory.
const (
	famConfigDir  = ".config/fam"
	envFile       = ".config/fam/env.sh"
	secretsFile   = ".config/fam/secrets.env"
	agentsDir     = ".agents"
	agentsSkills  = ".agents/skills"
	managedMarker = ".fam-managed"
)

// Path expands `~/` with the home directory of the SSH user.
func (e *Env) Path(p string) string {
	switch {
	case p == "~":
		return e.Facts.Home
	case strings.HasPrefix(p, "~/"):
		return path.Join(e.Facts.Home, p[2:])
	case strings.HasPrefix(p, "/"):
		return path.Clean(p)
	default:
		return path.Join(e.Facts.Home, p)
	}
}

// Home returns a path relative to the home directory.
func (e *Env) Home(rel string) string { return path.Join(e.Facts.Home, rel) }

// prelude loads the FAM environment, so that scripts see the bundle variables, the secrets,
// and the user programs in ~/.local/bin.
func (e *Env) prelude() string {
	home := transport.Quote(e.Facts.Home)
	return fmt.Sprintf(`HOME_FAM=%[1]s
[ -r "$HOME_FAM/%[2]s" ] && . "$HOME_FAM/%[2]s"
case ":$PATH:" in *":$HOME_FAM/.local/bin:"*) ;; *) PATH="$HOME_FAM/.local/bin:$PATH" ;; esac
export PATH
unset HOME_FAM
`, home, envFile)
}

// UserScript runs a user script (Bash when the machine has it, else sh) with the FAM environment.
func (e *Env) UserScript(ctx context.Context, script string, sudo bool, label string, timeout time.Duration) (transport.Result, error) {
	s := e.prelude() + `if command -v bash >/dev/null 2>&1; then exec bash -c "$0"; else exec sh -c "$0"; fi`
	full := "sh -c " + transport.Quote(s) + " " + transport.Quote(script)
	return e.T.Run(ctx, transport.Cmd{Script: full, Sudo: sudo, Label: label, Timeout: timeout})
}

// Sh runs a FAM-internal POSIX script.
func (e *Env) Sh(ctx context.Context, script string, sudo bool, label string) (transport.Result, error) {
	return e.T.Run(ctx, transport.Cmd{Script: script, Sudo: sudo, Label: label, Timeout: 10 * time.Minute})
}

// ShOK runs a FAM-internal POSIX script and returns an error on a nonzero exit code.
func (e *Env) ShOK(ctx context.Context, script string, sudo bool, label string) (transport.Result, error) {
	r, err := e.Sh(ctx, script, sudo, label)
	if err != nil {
		return r, err
	}
	if r.Code != 0 {
		return r, &ExitError{Label: label, Code: r.Code, Stderr: r.Err(), Stdout: r.Out()}
	}
	return r, nil
}

// ExitError is a nonzero exit code of a command.
type ExitError struct {
	Label  string
	Code   int
	Stderr string
	Stdout string
}

func (e *ExitError) Error() string {
	msg := tail(e.Stderr, 6)
	if msg == "" {
		msg = tail(e.Stdout, 6)
	}
	if msg != "" {
		return fmt.Sprintf("%s: exit code %d: %s", e.Label, e.Code, msg)
	}
	return fmt.Sprintf("%s: exit code %d", e.Label, e.Code)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func sudoHint(err error, sudo bool, kind string) error {
	if err == nil || !sudo {
		return err
	}
	if strings.Contains(err.Error(), "a password is required") || strings.Contains(err.Error(), "a terminal is required") {
		return fmt.Errorf("%w (FAM runs `sudo -n` on workers: give the SSH user passwordless sudo, or remove `sudo = true`)", err)
	}
	return err
}
