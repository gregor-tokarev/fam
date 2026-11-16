package engine

import (
	"fmt"
	"strings"

	"github.com/apple/pkl-go/pkl"

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
	"vite": {tool: "vp", installed: `out=$(vp list -g 2>&1) || exit 5
printf '%%s\n' "$out" | awk -v p=%[1]s 'NR > 2 { n = $1; sub(/@[^@]*$/, "", n); if (n == p) f = 1 } END { exit !f }'`,
		install: "vp install -g %s", update: "vp update -g %s", uninstall: "vp uninstall -g %s"},
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

// packageSpec reads the `package` field: a string (npm), a PackageRef, or a Dynamic object
// with `package` and `manager`.
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
		if k != "package" && k != "manager" {
			return "", "", fmt.Errorf("package has the unknown property %q. Use package and manager", k)
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
	if _, ok := managers[mgr]; !ok {
		return "", "", fmt.Errorf("package %s: unknown manager %q. Use one of npm, bun, vite, brew, apt, dnf, pacman, yay, apk", name, mgr)
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
		pt.Binary = defaultBinary(name)
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
