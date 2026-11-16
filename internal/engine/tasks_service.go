package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/gregor-tokarev/fam/internal/transport"
)

// ServiceTask is a systemd unit or a launchd job.
type ServiceTask struct {
	base
	Unit    string
	Scope   string
	Content *string
	Enabled bool
	Running bool
	Absent  bool
	units   []Unit
}

// Kind implements Task.
func (t *ServiceTask) Kind() string { return "service" }

// Name implements Task.
func (t *ServiceTask) Name() string { return t.Unit + " (" + t.Scope + ")" }

// Restarts implements Restarter.
func (t *ServiceTask) Restarts() []Unit { return t.units }

func needServiceManager(env *Env, what string) error {
	switch env.Facts.Init {
	case "systemd", "launchd":
		return nil
	}
	return fmt.Errorf("%s needs systemd or launchd, but %s (%s %s) has no supported service manager (init: %s)",
		what, env.Machine.Name, env.Facts.Distro, env.Facts.DistroVersion, env.Facts.Init)
}

// systemctl returns the systemctl command for the scope.
func systemctl(scope string) string {
	if scope == "system" {
		return "systemctl"
	}
	return `XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}" DBUS_SESSION_BUS_ADDRESS="unix:path=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/bus" systemctl --user`
}

func (t *ServiceTask) unitPath(env *Env) string {
	if env.Facts.Init == "launchd" {
		if t.Scope == "system" {
			return "/Library/LaunchDaemons/" + t.Unit + ".plist"
		}
		return env.Home("Library/LaunchAgents/" + t.Unit + ".plist")
	}
	if t.Scope == "system" {
		return "/etc/systemd/system/" + t.Unit
	}
	return env.Home(".config/systemd/user/" + t.Unit)
}

// launchdDomain returns the launchd domain target. LaunchAgents live in the GUI domain of
// the user, which exists only while the user is logged in at the screen.
func launchdDomain(scope string) string {
	if scope == "system" {
		return "system"
	}
	return `gui/$(id -u)`
}

// needGUISession stops with a clear error when a LaunchAgent has no GUI session to load into.
func needGUISession(ctx context.Context, env *Env, t *ServiceTask) error {
	if env.Facts.Init != "launchd" || t.Scope == "system" {
		return nil
	}
	r, err := env.Sh(ctx, `launchctl print "gui/$(id -u)" >/dev/null 2>&1`, false, "find the GUI session")
	if err != nil {
		return err
	}
	if r.Code != 0 {
		return fmt.Errorf("the LaunchAgent %s needs a GUI login session of %s on %s, and nobody is logged in at the screen. macOS loads LaunchAgents only in a GUI session: log in once, or turn on automatic login. For a service without a login, use scope = \"system\" (a LaunchDaemon)",
			t.Unit, env.Facts.User, env.Machine.Name)
	}
	return nil
}

type serviceState struct {
	fileExists, fileSame, enabled, active bool
}

func (t *ServiceTask) state(ctx context.Context, env *Env) (serviceState, error) {
	var st serviceState
	p := env.unitPathCached(t)
	fi, err := env.T.Stat(ctx, p, t.sudo)
	if err != nil {
		return st, err
	}
	st.fileExists = fi.Exists && fi.Type == "file"
	if t.Content != nil {
		st.fileSame = st.fileExists && fi.SHA256 == sha([]byte(*t.Content))
	}
	u := transport.Quote(t.Unit)
	var script string
	if env.Facts.Init == "launchd" {
		d := launchdDomain(t.Scope)
		script = fmt.Sprintf(`d=%s
if launchctl print "$d/"%s >/dev/null 2>&1; then echo active=yes; else echo active=no; fi
if launchctl print-disabled "$d" 2>/dev/null | grep -Eq '"'%s'" => (true|disabled)'; then echo enabled=no; else echo enabled=yes; fi`, d, u, u)
	} else {
		// `systemctl show` fails when FAM cannot reach the service manager; is-enabled and
		// is-active return nonzero codes for normal states, so FAM reads their output only after it.
		sc := systemctl(t.Scope)
		script = fmt.Sprintf(`%[1]s show --property=LoadState %[2]s >/dev/null || { echo "FAM cannot reach the service manager" >&2; exit 5; }
echo "enabled=$(%[1]s is-enabled %[2]s 2>/dev/null)"
echo "active=$(%[1]s is-active %[2]s 2>/dev/null)"`, sc, u)
	}
	r, err := env.Sh(ctx, script, t.sudo && t.Scope == "system", "state of "+t.Unit)
	if err != nil {
		return st, err
	}
	if r.Code != 0 {
		return st, sudoHint(&ExitError{Label: "state of " + t.Unit, Code: r.Code, Stderr: r.Err(), Stdout: r.Out()}, t.sudo, "service")
	}
	for _, line := range strings.Split(r.Out(), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "enabled":
			st.enabled = v == "yes" || v == "enabled" || v == "enabled-runtime" || v == "static" || v == "alias"
		case "active":
			st.active = v == "yes" || v == "active" || v == "activating" || v == "reloading"
		}
	}
	if env.Facts.Init == "launchd" && !st.active {
		// launchd has no "enabled but not loaded" state that FAM manages.
		st.enabled = st.enabled && st.fileExists
	}
	return st, nil
}

func (e *Env) unitPathCached(t *ServiceTask) string { return t.unitPath(e) }

// Check implements Task.
func (t *ServiceTask) Check(ctx context.Context, env *Env) (Check, error) {
	if err := needServiceManager(env, "the Service task "+t.Unit); err != nil {
		return Check{}, err
	}
	if t.Scope == "system" && !t.sudo {
		return Check{}, fmt.Errorf("the system service %s needs `sudo = true`", t.Unit)
	}
	if err := needGUISession(ctx, env, t); err != nil {
		return Check{}, err
	}
	st, err := t.state(ctx, env)
	if err != nil {
		return Check{}, err
	}
	var diffs []string
	if t.Absent {
		if st.active {
			diffs = append(diffs, "stop")
		}
		if st.enabled && (st.fileExists || t.Content == nil) {
			diffs = append(diffs, "disable")
		}
		if t.Content != nil && st.fileExists {
			diffs = append(diffs, "remove unit file")
		}
	} else {
		if t.Content != nil && !st.fileSame {
			if st.fileExists {
				diffs = append(diffs, "update unit file")
			} else {
				diffs = append(diffs, "create unit file")
			}
		}
		if t.Enabled != st.enabled {
			diffs = append(diffs, map[bool]string{true: "enable", false: "disable"}[t.Enabled])
		}
		if t.Running != st.active {
			diffs = append(diffs, map[bool]string{true: "start", false: "stop"}[t.Running])
		}
	}
	if len(diffs) == 0 {
		return Check{Status: Satisfied}, nil
	}
	return Check{Status: Change, Detail: strings.Join(diffs, ", ")}, nil
}

// Apply implements Task.
func (t *ServiceTask) Apply(ctx context.Context, env *Env) (string, error) {
	st, err := t.state(ctx, env)
	if err != nil {
		return "", err
	}
	p := t.unitPath(env)
	sysSudo := t.sudo && t.Scope == "system"
	u := transport.Quote(t.Unit)
	var steps []string
	if t.Absent {
		if env.Facts.Init == "launchd" {
			d := launchdDomain(t.Scope)
			steps = append(steps, fmt.Sprintf(`d=%s; launchctl bootout "$d/"%s 2>/dev/null || true`, d, u))
			if t.Content != nil {
				steps = append(steps, "rm -f -- "+transport.Quote(p))
			}
		} else {
			sc := systemctl(t.Scope)
			steps = append(steps, fmt.Sprintf("%s disable --now %s 2>/dev/null || %s stop %s 2>/dev/null || true", sc, u, sc, u))
			if t.Content != nil {
				steps = append(steps, "rm -f -- "+transport.Quote(p), sc+" daemon-reload")
			}
		}
		_, err := env.ShOK(ctx, strings.Join(steps, "\n"), sysSudo, "remove service "+t.Unit)
		return "removed", sudoHint(err, t.sudo, "service")
	}
	fileChanged := false
	if t.Content != nil && !st.fileSame {
		if err := env.T.WriteFile(ctx, p, []byte(*t.Content), 0o644, sysSudo); err != nil {
			return "", sudoHint(err, t.sudo, "service")
		}
		fileChanged = true
	}
	if env.Facts.Init == "launchd" {
		d := launchdDomain(t.Scope)
		s := fmt.Sprintf("set -e\nd=%s\n", d)
		if t.Enabled {
			s += fmt.Sprintf(`launchctl enable "$d/"%s`+"\n", u)
		} else {
			s += fmt.Sprintf(`launchctl disable "$d/"%s`+"\n", u)
		}
		if t.Running {
			if fileChanged || !st.active {
				// bootstrap loads the plist again. A job without RunAtLoad or KeepAlive does not
				// start on load, so kickstart (without -k) starts it when it does not run yet.
				s += fmt.Sprintf(`launchctl bootout "$d/"%[1]s 2>/dev/null || true
launchctl bootstrap "$d" %[2]s
launchctl kickstart "$d/"%[1]s
`, u, transport.Quote(p))
				// The job now runs in a new process with the new plist, so the restart requests
				// before this task are done.
				env.markRestarted(Unit{Name: t.Unit, Scope: t.Scope})
			}
		} else if st.active {
			s += fmt.Sprintf(`launchctl bootout "$d/"%s 2>/dev/null || true`+"\n", u)
		}
		_, err := env.ShOK(ctx, s, sysSudo, "launchctl "+t.Unit)
		return "configured", sudoHint(err, t.sudo, "service")
	}
	sc := systemctl(t.Scope)
	s := "set -e\n"
	if fileChanged {
		s += sc + " daemon-reload\n"
	}
	if t.Enabled && !st.enabled {
		s += sc + " enable " + u + "\n"
	} else if !t.Enabled && st.enabled {
		s += sc + " disable " + u + "\n"
	}
	switch {
	case t.Running && (fileChanged && st.active):
		s += sc + " restart " + u + "\n"
		env.markRestarted(Unit{Name: t.Unit, Scope: t.Scope})
	case t.Running && !st.active:
		s += sc + " start " + u + "\n"
	case !t.Running && st.active:
		s += sc + " stop " + u + "\n"
	}
	_, err = env.ShOK(ctx, s, sysSudo, "systemctl "+t.Unit)
	return "configured", sudoHint(err, t.sudo, "service")
}

// restartUnit restarts one unit.
func restartUnit(ctx context.Context, env *Env, u Unit) error {
	if err := needServiceManager(env, "restarting "+u.Name); err != nil {
		return err
	}
	q := transport.Quote(u.Name)
	var script string
	if env.Facts.Init == "launchd" {
		script = fmt.Sprintf(`d=%s; launchctl kickstart -k "$d/"%s`, launchdDomain(u.Scope), q)
	} else {
		// A changed unit file or drop-in takes effect only after daemon-reload.
		script = systemctl(u.Scope) + " daemon-reload && " + systemctl(u.Scope) + " restart " + q
	}
	_, err := env.ShOK(ctx, script, u.Scope == "system", "restart "+u.Name)
	return err
}

// LingerTask enables lingering for a user.
type LingerTask struct {
	base
	User   string
	Absent bool
	units  []Unit
}

// Restarts implements Restarter.
func (t *LingerTask) Restarts() []Unit { return t.units }

// Kind implements Task.
func (t *LingerTask) Kind() string { return "linger" }

// Name implements Task.
func (t *LingerTask) Name() string { return t.user(nil) }

func (t *LingerTask) user(env *Env) string {
	if t.User != "" || env == nil {
		if t.User == "" {
			return "SSH user"
		}
		return t.User
	}
	return env.Facts.User
}

// Check implements Task.
func (t *LingerTask) Check(ctx context.Context, env *Env) (Check, error) {
	if env.Facts.Init != "systemd" {
		return Check{}, fmt.Errorf("Linger needs systemd, but %s (%s %s) has init %q", env.Machine.Name, env.Facts.Distro, env.Facts.DistroVersion, env.Facts.Init)
	}
	fi, err := env.T.Stat(ctx, "/var/lib/systemd/linger/"+t.user(env), false)
	if err != nil {
		return Check{}, err
	}
	switch {
	case fi.Exists && t.Absent:
		return Check{Status: Change, Detail: "disable lingering for " + t.user(env)}, nil
	case !fi.Exists && !t.Absent:
		return Check{Status: Change, Detail: "enable lingering for " + t.user(env)}, nil
	}
	return Check{Status: Satisfied}, nil
}

// Apply implements Task.
func (t *LingerTask) Apply(ctx context.Context, env *Env) (string, error) {
	verb := "enable-linger"
	if t.Absent {
		verb = "disable-linger"
	}
	_, err := env.ShOK(ctx, "loginctl "+verb+" "+transport.Quote(t.user(env)), t.sudo, "loginctl "+verb)
	return verb, sudoHint(err, t.sudo, "linger")
}
