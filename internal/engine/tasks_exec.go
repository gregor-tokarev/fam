package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/gregor-tokarev/fam/internal/transport"
)

const scriptTimeout = 30 * time.Minute

// ScriptTask is a user script with a check.
type ScriptTask struct {
	base
	Title   string
	CheckSh string
	Creates string
	ApplySh string
	units   []Unit
}

// Kind implements Task.
func (t *ScriptTask) Kind() string { return "script" }

// Name implements Task.
func (t *ScriptTask) Name() string { return t.Title }

// Restarts implements Restarter.
func (t *ScriptTask) Restarts() []Unit { return t.units }

// Check implements Task. Exit code 0 is satisfied, 100 is a change, any other code is an error.
func (t *ScriptTask) Check(ctx context.Context, env *Env) (Check, error) {
	script := t.CheckSh
	if script == "" {
		// Stat reports a folder that FAM cannot look into as an error, not as a missing path.
		fi, err := env.T.Stat(ctx, env.Path(t.Creates), t.sudo)
		if err != nil {
			return Check{}, err
		}
		if fi.Exists {
			return Check{Status: Satisfied}, nil
		}
		return Check{Status: Change, Detail: t.Creates + " does not exist"}, nil
	}
	r, err := env.UserScript(ctx, script, t.sudo, "check "+t.Title, scriptTimeout)
	if err != nil {
		return Check{}, err
	}
	switch r.Code {
	case 0:
		return Check{Status: Satisfied}, nil
	case 100:
		detail := lastLine(r.Out())
		if detail == "" {
			detail = "check returned 100"
		}
		return Check{Status: Change, Detail: detail}, nil
	default:
		return Check{}, sudoHint(&ExitError{Label: "check", Code: r.Code, Stderr: r.Err(), Stdout: r.Out()}, t.sudo, "script")
	}
}

// Apply implements Task.
func (t *ScriptTask) Apply(ctx context.Context, env *Env) (string, error) {
	r, err := env.UserScript(ctx, t.ApplySh, t.sudo, "apply "+t.Title, scriptTimeout)
	if err != nil {
		return "", err
	}
	if r.Code != 0 {
		return "", sudoHint(&ExitError{Label: "apply", Code: r.Code, Stderr: r.Err(), Stdout: r.Out()}, t.sudo, "script")
	}
	return "applied", nil
}

// lastLine returns the last line in full: FAM hides secret values later, and a cut value
// would not match.
func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// PackageTask is a command line program with install, update, and uninstall commands.
type PackageTask struct {
	base
	Binary     string
	Install    string
	UpdateSh   string
	Uninstall  string
	VersionCmd string
	Absent     bool
	// Label replaces the binary name in output.
	Label string
	// Pin is a pinned version. A different installed version is a change; apply runs Install,
	// which installs the pinned version.
	Pin string
	// Manager and PackageName are set for a package that a package manager installs.
	// FAM then asks the package manager, not PATH, whether the package is installed.
	Manager     string
	PackageName string
	// ApplySudo runs install, update, and uninstall with sudo (OS package managers).
	ApplySudo bool
	units     []Unit
}

func (t *PackageTask) applySudo() bool { return t.sudo || t.ApplySudo }

// errNoManager means that the package manager of a package is not on the machine (yet).
var errNoManager = fmt.Errorf("the package manager is not installed")

// Kind implements Task.
func (t *PackageTask) Kind() string { return "package" }

// Name implements Task.
func (t *PackageTask) Name() string {
	if t.Label != "" {
		return t.Label
	}
	if t.Manager != "" {
		return t.PackageName + " (" + t.Manager + ")"
	}
	return t.Binary
}

// Restarts implements Restarter.
func (t *PackageTask) Restarts() []Unit { return t.units }

// bin returns the binary name, or its absolute path when the config gives a path.
func (t *PackageTask) bin(env *Env) string {
	if strings.HasPrefix(t.Binary, "~/") || strings.HasPrefix(t.Binary, "/") {
		return env.Path(t.Binary)
	}
	return t.Binary
}

// installedScript exits 0 when the program is found, 3 when it is not. Any other code is an
// error (for example sudo without a password).
func (t *PackageTask) installedScript(env *Env) (script, label string) {
	if t.Manager != "" {
		return managerCheck(t.Manager, t.PackageName), "ask " + t.Manager + " for " + t.PackageName
	}
	return "if command -v " + transport.Quote(t.bin(env)) + " >/dev/null 2>&1; then exit 0; else exit 3; fi", "find " + t.Binary
}

// installedResult reads the exit code of installedScript.
func (t *PackageTask) installedResult(r transport.Result) (bool, error) {
	switch r.Code {
	case 0:
		return true, nil
	case 3:
		return false, nil
	case 4:
		if t.Manager != "" {
			return false, errNoManager
		}
	}
	return false, sudoHint(&ExitError{Label: "find " + t.Binary, Code: r.Code, Stderr: r.Err(), Stdout: r.Out()}, t.sudo, "package")
}

func (t *PackageTask) installed(ctx context.Context, env *Env) (bool, error) {
	script, label := t.installedScript(env)
	r, err := env.UserScript(ctx, script, t.sudo, label, 2*time.Minute)
	if err != nil {
		return false, err
	}
	return t.installedResult(r)
}

// versionScript is the version command, with the absolute path when the config gives a path.
func (t *PackageTask) versionScript(env *Env) string {
	cmd := t.VersionCmd
	if b := t.bin(env); b != t.Binary && strings.HasPrefix(cmd, t.Binary+" ") {
		cmd = transport.Quote(b) + cmd[len(t.Binary):]
	}
	return cmd
}

func (t *PackageTask) version(ctx context.Context, env *Env) string {
	if t.VersionCmd == "" {
		return ""
	}
	r, err := env.UserScript(ctx, t.versionScript(env), t.sudo, "version of "+t.Binary, 2*time.Minute)
	if err != nil || r.Code != 0 {
		return ""
	}
	return firstLine(r.Out())
}

// runMarker returns a separator line that no program prints by chance.
func runMarker(kind string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "FAM-" + kind + "-" + hex.EncodeToString(b)
}

// probeDriver runs the installed script ("$<inst>") and, when the program is found, prints mark
// and the output of the version script ("$<ver>"). Its exit code is the code of the installed
// script. Both scripts run as their own top-level scripts (see UserScripts).
func probeDriver(inst, ver int, mark string) string {
	return fmt.Sprintf(`"$FAM_SH" -c "$%[1]d"
rc=$?
[ "$rc" -eq 0 ] || exit "$rc"
printf '\n%%s\n' '%[3]s'
if v=$("$FAM_SH" -c "$%[2]d"); then printf '%%s\n' "$v"; fi
exit 0
`, inst, ver, mark)
}

// probeResult reads the result of probeDriver. version is "" when the program is not installed
// or the version command fails.
func (t *PackageTask) probeResult(r transport.Result, mark string) (ok bool, version string, err error) {
	if ok, err = t.installedResult(r); !ok || err != nil {
		return ok, "", err
	}
	if _, after, found := strings.Cut(string(r.Stdout), "\n"+mark+"\n"); found {
		version = firstLine(after)
	}
	return true, version, nil
}

// probe finds the program and reads its version in one command.
func (t *PackageTask) probe(ctx context.Context, env *Env) (ok bool, version string, err error) {
	inst, label := t.installedScript(env)
	if t.VersionCmd == "" {
		ok, err = t.installed(ctx, env)
		return ok, "", err
	}
	mark := runMarker("VERSION")
	r, err := env.UserScripts(ctx, probeDriver(1, 2, mark), []string{inst, t.versionScript(env)}, t.sudo, label+" and its version", 4*time.Minute)
	if err != nil {
		return false, "", err
	}
	return t.probeResult(r, mark)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// Check implements Task.
func (t *PackageTask) Check(ctx context.Context, env *Env) (Check, error) {
	var ok bool
	var v string
	var err error
	// A pin needs the version, and an update compares it with the version after the update. One
	// command finds the program and reads the version.
	probed := !t.Absent && t.VersionCmd != "" && (t.Pin != "" || t.UpdateSh != "" && env.probeVersions)
	if probed {
		ok, v, err = t.probe(ctx, env)
	} else {
		ok, err = t.installed(ctx, env)
	}
	if err == errNoManager {
		// An earlier task can install the package manager (for example Node.js for npm).
		if t.Absent {
			return Check{Status: Satisfied, Detail: t.Manager + " is not installed"}, nil
		}
		return Check{Status: Change, Detail: "install (" + managers[t.Manager].tool + " is not installed yet)"}, nil
	}
	if err != nil {
		return Check{}, err
	}
	if t.Absent {
		if !ok {
			return Check{Status: Satisfied}, nil
		}
		if t.Uninstall == "" {
			return Check{}, fmt.Errorf("package %s has no uninstall command, so FAM cannot remove it", t.Binary)
		}
		return Check{Status: Change, Detail: "uninstall"}, nil
	}
	if !ok {
		return Check{Status: Change, Detail: "install"}, nil
	}
	if t.Pin != "" && !versionIs(v, t.Pin) {
		return Check{Status: Change, Detail: fmt.Sprintf("installed %q, pinned %s", v, t.Pin)}, nil
	}
	return Check{Status: Satisfied, version: v, versionKnown: probed}, nil
}

// Apply implements Task.
func (t *PackageTask) Apply(ctx context.Context, env *Env) (string, error) {
	script, verb := t.Install, "installed"
	if t.Absent {
		script, verb = t.Uninstall, "uninstalled"
	}
	if t.Manager != "" {
		if _, err := t.installed(ctx, env); err == errNoManager {
			return "", fmt.Errorf("FAM cannot install %s: %s is not on the machine. Install it first, for example with an earlier bundle", t.PackageName, managers[t.Manager].tool)
		}
	}
	r, err := env.UserScript(ctx, script, t.applySudo(), verb+" "+t.Name(), scriptTimeout)
	if err != nil {
		return "", err
	}
	if r.Code != 0 {
		return "", sudoHint(&ExitError{Label: strings.TrimSuffix(verb, "ed"), Code: r.Code, Stderr: r.Err(), Stdout: r.Out()}, t.applySudo(), "package")
	}
	if v := t.version(ctx, env); v != "" && !t.Absent {
		return verb + " " + v, nil
	}
	return verb, nil
}

// Update implements Updater. FAM compares the version before and after the update. c is the
// satisfied check before the update, with the version when the check read it.
func (t *PackageTask) Update(ctx context.Context, env *Env, c Check) (Update, error) {
	if t.Absent || t.UpdateSh == "" {
		return Update{}, nil
	}
	before := c.version
	if !c.versionKnown {
		before = t.version(ctx, env)
	}
	// When the update and the probe run as the same user, one command runs the update and then
	// finds the program and its version again. updMark shows that the update succeeded.
	together := t.applySudo() == t.sudo && t.VersionCmd != ""
	updMark, verMark := runMarker("UPDATE"), runMarker("VERSION")
	var r transport.Result
	var err error
	if together {
		inst, _ := t.installedScript(env)
		driver := `"$FAM_SH" -c "$1"
rc=$?
[ "$rc" -eq 0 ] || exit "$rc"
printf '\n%s\n' '` + updMark + `'
` + probeDriver(2, 3, verMark)
		r, err = env.UserScripts(ctx, driver, []string{t.UpdateSh, inst, t.versionScript(env)}, t.sudo, "update "+t.Name(), scriptTimeout+4*time.Minute)
	} else {
		r, err = env.UserScript(ctx, t.UpdateSh, t.applySudo(), "update "+t.Name(), scriptTimeout)
	}
	if err != nil {
		return Update{Ran: true}, err
	}
	var probed *transport.Result
	if together {
		if out, after, found := bytes.Cut(r.Stdout, []byte("\n"+updMark+"\n")); found {
			// The update succeeded. The exit code and the rest of stdout are of the probe.
			probed = &transport.Result{Stdout: after, Stderr: r.Stderr, Code: r.Code}
			r = transport.Result{Stdout: out, Stderr: r.Stderr}
		}
	}
	// Exit code 42 with "FAM-ROOT: <command>": the update needs root (an OS package). FAM runs
	// the command with its own sudo (sudo -n on workers, the password on the cockpit).
	if r.Code == 42 {
		cmd := ""
		for _, l := range strings.Split(r.Out(), "\n") {
			if c, ok := strings.CutPrefix(strings.TrimSpace(l), "FAM-ROOT: "); ok {
				cmd = c
			}
		}
		if cmd == "" {
			return Update{Ran: true}, fmt.Errorf("update %s: exit code 42 without a FAM-ROOT command", t.Name())
		}
		r, err = env.UserScript(ctx, cmd, true, "update "+t.Name()+" as root", scriptTimeout)
		if err != nil {
			return Update{Ran: true}, err
		}
	}
	if r.Code != 0 {
		return Update{Ran: true}, sudoHint(&ExitError{Label: "update", Code: r.Code, Stderr: r.Err(), Stdout: r.Out()}, t.applySudo(), "package")
	}
	// Check again: an update must leave the program installed. The probe also gives the version
	// for the pin, so this is the full check of the task.
	var ok bool
	var after string
	if probed != nil {
		ok, after, err = t.probeResult(*probed, verMark)
		if err == nil && !ok {
			// The update can change the shell startup files (for example a new PATH entry) that
			// only a new SSH command reads. Look again in a new command before FAM reports an error.
			ok, after, err = t.probe(ctx, env)
		}
	} else {
		ok, after, err = t.probe(ctx, env)
	}
	if err != nil {
		return Update{Ran: true}, err
	} else if !ok {
		return Update{Ran: true, Changed: true}, fmt.Errorf("the update of %s ran, but FAM cannot find %s after it", t.Binary, t.bin(env))
	}
	u := Update{Ran: true, Checked: true}
	switch {
	case before != after:
		u.Changed, u.Detail = true, fmt.Sprintf("updated %s -> %s", before, after)
	case after == "":
		u.Detail = "update ran"
	default:
		u.Detail = "up to date (" + after + ")"
	}
	if t.Pin != "" && !versionIs(after, t.Pin) {
		u.Diff = fmt.Sprintf("installed %q, pinned %s", after, t.Pin)
	}
	return u, nil
}

// releaseVersion removes a tag's path prefix and leading v.
func releaseVersion(tag string) string {
	return strings.TrimPrefix(tag[strings.LastIndexByte(tag, '/')+1:], "v")
}

// Allow build metadata and common release suffixes, but keep prereleases distinct.
var releaseBuildSuffix = regexp.MustCompile(`^(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*|-(release|stable|final|build)([.-][0-9A-Za-z-]+)*)$`)

// versionIs matches a whole version, including after a slash, without matching a longer version.
func versionIs(out, version string) bool {
	version = releaseVersion(version)
	for _, f := range strings.FieldsFunc(out, func(r rune) bool {
		return unicode.IsSpace(r) || r == '(' || r == ')' || r == ',' || r == '/'
	}) {
		f = strings.TrimPrefix(f, "v")
		if f == version || strings.HasPrefix(f, version) && releaseBuildSuffix.MatchString(strings.TrimPrefix(f, version)) {
			return true
		}
	}
	return false
}
