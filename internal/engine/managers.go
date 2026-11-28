package engine

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/apple/pkl-go/pkl"

	"github.com/gregor-tokarev/fam/internal/facts"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks"
	"github.com/gregor-tokarev/fam/internal/transport"
)

// manager holds the commands of one package manager. %s is the quoted package name.
type manager struct {
	// tool is the program that must exist on the machine.
	tool string
	// installed exits 0 when the package is installed and nonzero when it is not.
	installed string
	install   string
	update    string
	uninstall string
	// root runs install, update, and uninstall with FAM's sudo.
	root bool
}

// The installed queries exit 0 when the package is installed, 1 when it is not, and any other
// code when the query fails. %[1]s is the quoted package name, %[2]s the raw name for awk.
var managers = map[string]manager{
	"uv": {tool: "uv", installed: `out=$(uv tool list 2>&1) || exit 5
printf '%%s\n' "$out" | awk -v p=%[1]s '$1 == p { found = 1 } END { exit !found }'`,
		install: "uv tool install %s", update: "uv tool upgrade %s", uninstall: "uv tool uninstall %s"},
	// Cargo's list command creates metadata even when no tools exist. Read its records directly.
	"cargo": {tool: "cargo", installed: `[ -e "$HOME/.local/.crates.toml" ] || exit 1
out=$(cat "$HOME/.local/.crates.toml") || exit 5
printf '%%s\n' "$out" | awk -F '"' -v p=%[1]s 'index($2, p " ") == 1 { found = 1 } END { exit !found }'`,
		install: `cargo install --locked --root "$HOME/.local" %s`, update: `cargo install --locked --root "$HOME/.local" %s`, uninstall: `cargo uninstall --root "$HOME/.local" %s`},
	// Go inspection must not create telemetry files during plan.
	"go": {tool: "go", installed: `for b in "$HOME/.local/bin/"*; do
[ -f "$b" ] || continue
out=$(TEST_TELEMETRY_DIR=/dev/null go version -m "$b" 2>/dev/null) || continue
printf '%%s\n' "$out" | awk -v p=%[1]s '$1 == "path" && $2 == p { found = 1 } END { exit !found }' && exit 0
done
exit 1`,
		install: `GOBIN="$HOME/.local/bin" go install %s@latest`, update: `GOBIN="$HOME/.local/bin" go install %s@latest`,
		uninstall: `for b in "$HOME/.local/bin/"*; do
[ -f "$b" ] || continue
out=$(TEST_TELEMETRY_DIR=/dev/null go version -m "$b" 2>/dev/null) || continue
if printf '%%s\n' "$out" | awk -v p=%s '$1 == "path" && $2 == p { found = 1 } END { exit !found }'; then rm -f "$b" || exit; fi
done`},
	// npm: `npm ls -g` knows versions (foo@1.2.3), but exits 254 with ENOENT when the global
	// folder does not exist yet (a fresh prefix). That is "not installed", not an error. Only
	// ENOENT on the global folder or a folder above it counts. Any other failure is an error.
	// The options fix the error format against npm settings (loglevel, color, heading, json, parseable).
	"npm": {tool: "npm", installed: `r=$(npm root -g 2>/dev/null) || exit 5
out=$(npm ls -g --depth=0 --loglevel=error --color=false --heading=npm --json=false --parseable=false %[1]s 2>&1); rc=$?
[ $rc -ne 0 ] || exit 0
case "$out" in *"code ENOENT"*) ;; *) exit $rc ;; esac
p=$(printf '%%s\n' "$out" | sed -n -e 's/^npm error path //p' -e 's/^npm ERR! path //p' | head -n 1)
[ -n "$p" ] || exit 5
case "$r/" in "$p"/*) exit 1 ;; esac
exit 5`,
		install: "npm install -g %s", update: "npm install -g %s@latest", uninstall: "npm uninstall -g %s"},
	"bun": {tool: "bun", installed: `out=$(bun pm ls -g 2>&1) || { case "$out" in *"No package.json"*) exit 1 ;; *) exit 5 ;; esac; }
printf '%%s\n' "$out" | awk -v p=%[1]s '{for (i = 1; i <= NF; i++) { n = $i; sub(/@[^@]*$/, "", n); if (n != $i && n == p) f = 1 }} END { exit !f }'`,
		install: "bun add -g %s", update: "bun add -g %s@latest", uninstall: "bun remove -g %s"},
	// Vite+ refuses to install when another global package already owns the same executable
	// unless --force is passed. --force auto-replaces conflicting Vite+ packages and still
	// rejects binaries that Vite+ does not manage.
	"vite": {tool: "vp", installed: `out=$(vp list -g 2>&1) || exit 5
printf '%%s\n' "$out" | awk -v p=%[1]s 'NR > 2 { n = $1; sub(/@[^@]*$/, "", n); if (n == p) f = 1 } END { exit !f }'`,
		install: "vp install -g --force %s", update: "vp update -g %s", uninstall: "vp uninstall -g %s"},
	"brew": {tool: "brew", installed: "brew list %[1]s >/dev/null 2>&1",
		install: "brew install %s", update: "brew upgrade %s", uninstall: "brew uninstall %s"},
	"apt": {tool: "apt-get", installed: `st=$(dpkg-query -W -f='${Status}' %[1]s 2>/dev/null); rc=$?
[ $rc -le 1 ] || exit 5
case "$st" in *"install ok installed"*) exit 0 ;; *) exit 1 ;; esac`,
		install:   "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y %s",
		update:    "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --only-upgrade %s",
		uninstall: "DEBIAN_FRONTEND=noninteractive apt-get remove -y %s", root: true},
	"dnf": {tool: "dnf", installed: "rpm -q %[1]s >/dev/null 2>&1",
		install: "dnf install -y %s", update: "dnf upgrade -y %s", uninstall: "dnf remove -y %s", root: true},
	// pacman and yay: Arch Linux has no safe update of one package (no partial upgrades), so the
	// update only installs a missing package. `pacman -Syu` updates the whole system.
	"pacman": {tool: "pacman", installed: "pacman -Q %[1]s >/dev/null 2>&1",
		install: "pacman -S --needed --noconfirm %s", update: "pacman -S --needed --noconfirm %s",
		uninstall: "pacman -R --noconfirm %s", root: true},
	"yay": {tool: "yay", installed: "pacman -Q %[1]s >/dev/null 2>&1",
		install: "yay -S --needed --noconfirm %s", update: "yay -S --needed --noconfirm %s",
		uninstall: "yay -R --noconfirm %s"},
	"apk": {tool: "apk", installed: "apk info -e %[1]s >/dev/null 2>&1",
		install: "apk add --no-cache %s", update: "apk add --no-cache --upgrade %s", uninstall: "apk del %s", root: true},
}

// PackageSpec resolves a package, its manager (the manager of the bundle wins when it is set),
// and the name for the selected manager.
func PackageSpec(v any, override string, f facts.Facts) (name, mgr string, err error) {
	name, mgr, err = packageSpec(v)
	if err != nil {
		return
	}
	if override != "" {
		mgr = override
	}
	if mgr == "system" {
		mgr, err = systemManager(f)
		if err != nil {
			return
		}
	}
	names := map[string]string{}
	switch x := v.(type) {
	case tasks.PackageRef:
		for k, n := range x.Names {
			names[string(k)] = n
		}
	case *tasks.PackageRef:
		for k, n := range x.Names {
			names[string(k)] = n
		}
	case pkl.Object:
		names, err = objectNames(x.Properties["names"])
	case *pkl.Object:
		names, err = objectNames(x.Properties["names"])
	}
	if n, ok := names[mgr]; ok {
		name = n
	}
	return
}

func systemManager(f facts.Facts) (string, error) {
	if f.OS == "darwin" {
		return "brew", nil
	}
	if f.OS == "linux" {
		switch f.Distro {
		case "ubuntu", "debian", "linuxmint", "pop", "raspbian", "kali":
			return "apt", nil
		case "fedora", "rhel", "centos", "rocky", "almalinux", "amzn":
			return "dnf", nil
		case "arch", "manjaro", "endeavouros":
			return "pacman", nil
		case "alpine":
			return "apk", nil
		}
	}
	return "", fmt.Errorf("no system package manager for %s/%s. Select a manager explicitly", f.OS, f.Distro)
}

func objectNames(v any) (map[string]string, error) {
	out := map[string]string{}
	if v == nil {
		return out, nil
	}
	switch x := v.(type) {
	case pkl.Object:
		for k, v := range x.Entries {
			n, ok := v.(string)
			key, keyOK := k.(string)
			if !ok || !keyOK || n == "" {
				return nil, fmt.Errorf("package names must map managers to nonempty strings")
			}
			out[key] = n
		}
	case map[any]any:
		for k, v := range x {
			n, ok := v.(string)
			key, keyOK := k.(string)
			if !ok || !keyOK || n == "" {
				return nil, fmt.Errorf("package names must map managers to nonempty strings")
			}
			out[key] = n
		}
	default:
		return nil, fmt.Errorf("package names must be a mapping")
	}
	return out, nil
}

// packageSpec reads a package string, PackageRef, or Dynamic object.
func packageSpec(v any) (name, mgr string, err error) {
	switch x := v.(type) {
	case string:
		return x, "npm", nil
	case tasks.PackageRef:
		return x.Package, string(x.Manager), nil
	case *tasks.PackageRef:
		return x.Package, string(x.Manager), nil
	case pkl.Object:
		return packageFromObject(&x)
	case *pkl.Object:
		return packageFromObject(x)
	}
	return "", "", fmt.Errorf("package has the type %T. Use a string or new { package = …; manager = … }", v)
}

func packageFromObject(o *pkl.Object) (string, string, error) {
	for k := range o.Properties {
		if k != "package" && k != "manager" && k != "names" {
			return "", "", fmt.Errorf("package has the unknown property %q. Use package, manager, and names", k)
		}
	}
	name, ok := o.Properties["package"].(string)
	if !ok || name == "" {
		return "", "", fmt.Errorf("package needs a `package` name")
	}
	mgr := "npm"
	if m, ok := o.Properties["manager"]; ok {
		s, ok := m.(string)
		if !ok {
			return "", "", fmt.Errorf("the manager of package %s is not a string", name)
		}
		mgr = s
	}
	if _, ok := managers[mgr]; !ok && mgr != "system" {
		return "", "", fmt.Errorf("package %s: unknown manager %q. Use one of npm, bun, vite, uv, cargo, go, system, brew, apt, dnf, pacman, yay, apk", name, mgr)
	}
	return name, mgr, nil
}

// defaultBinary is the package name without an npm scope and without a version.
func defaultBinary(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexByte(name, '@'); i > 0 {
		name = name[:i]
	}
	return name
}

// managedPackage fills a PackageTask from a package and its manager.
func managedPackage(pt *PackageTask, name, mgr string) error {
	m, ok := managers[mgr]
	if !ok {
		return fmt.Errorf("package %s: unknown manager %q", name, mgr)
	}
	q := transport.Quote(name)
	pt.Manager = mgr
	pt.PackageName = name
	pt.Install = fmt.Sprintf(m.install, q)
	pt.UpdateSh = fmt.Sprintf(m.update, q)
	pt.Uninstall = fmt.Sprintf(m.uninstall, q)
	pt.ApplySudo = m.root
	if pt.Binary == "" {
		binaryName := name
		if mgr == "go" {
			// Go omits a major version suffix from the executable name.
			suffix := path.Base(name)
			if strings.HasPrefix(suffix, "v") {
				if major, err := strconv.Atoi(suffix[1:]); err == nil && major >= 2 {
					binaryName = path.Dir(name)
				}
			}
		}
		pt.Binary = defaultBinary(binaryName)
	}
	return nil
}

// managerCheck returns the script that tells whether the package manager has the package:
// exit 0 installed, 3 not installed, 4 the manager is missing. A failed query exits with
// another code, which FAM reports as an error.
func managerCheck(mgr, name string) string {
	m := managers[mgr]
	return fmt.Sprintf(`command -v %s >/dev/null 2>&1 || exit 4
( %s )
rc=$?
case $rc in 0) exit 0 ;; 1) exit 3 ;; *) echo "the query of %s failed with exit code $rc" >&2; exit 5 ;; esac`, m.tool, fmt.Sprintf(m.installed, transport.Quote(name)), m.tool)
}
