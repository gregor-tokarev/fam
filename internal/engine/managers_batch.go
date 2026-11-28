package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gregor-tokarev/fam/internal/transport"
)

type mgrCacheKey struct {
	mgr  string
	sudo bool
}

type mgrCacheEntry struct {
	once      sync.Once
	pkgs      []string
	queried   map[string]bool
	installed map[string]bool
	complete  bool
	err       error
}

// managerCache deduplicates read-only package manager queries across a single checkAll pass.
type managerCache struct {
	entries map[mgrCacheKey]*mgrCacheEntry
}

func newManagerCache(ts []Task) *managerCache {
	byKey := map[mgrCacheKey][]string{}
	seen := map[mgrCacheKey]map[string]bool{}
	for _, t := range ts {
		pt, ok := t.(*PackageTask)
		if !ok || pt.Manager == "" {
			continue
		}
		// Architecture-qualified Debian package names (e.g. libc6:amd64) need exact single-package dpkg-query checks.
		if pt.Manager == "apt" && strings.Contains(pt.PackageName, ":") {
			continue
		}
		k := mgrCacheKey{mgr: pt.Manager, sudo: pt.sudo}
		if seen[k] == nil {
			seen[k] = map[string]bool{}
		}
		if !seen[k][pt.PackageName] {
			seen[k][pt.PackageName] = true
			byKey[k] = append(byKey[k], pt.PackageName)
		}
	}
	entries := map[mgrCacheKey]*mgrCacheEntry{}
	for k, pkgs := range byKey {
		if len(pkgs) < 2 || !supportsBulkManager(k.mgr) {
			continue
		}
		entries[k] = &mgrCacheEntry{pkgs: pkgs, queried: seen[k]}
	}
	if len(entries) == 0 {
		return nil
	}
	return &managerCache{entries: entries}
}

func supportsBulkManager(mgr string) bool {
	switch mgr {
	case "vite", "apt", "dnf", "pacman", "yay", "apk", "uv", "bun", "cargo", "go", "brew":
		return true
	}
	return false
}

// lookup checks pkg against a cached bulk manager snapshot. handled is false when the caller
// should run the standard single-package check instead.
func (c *managerCache) lookup(ctx context.Context, env *Env, mgr, pkg string, sudo bool) (installed bool, handled bool, err error) {
	if c == nil {
		return false, false, nil
	}
	e, ok := c.entries[mgrCacheKey{mgr: mgr, sudo: sudo}]
	if !ok || !e.queried[pkg] {
		return false, false, nil
	}
	e.once.Do(func() {
		e.installed, e.complete, e.err = queryManagerBulk(ctx, env, mgr, e.pkgs, sudo)
	})
	if e.err != nil {
		return false, true, e.err
	}
	if e.installed[pkg] {
		return true, true, nil
	}
	if !e.complete {
		return false, false, nil
	}
	return false, true, nil
}

func queryManagerBulk(ctx context.Context, env *Env, mgr string, pkgs []string, sudo bool) (map[string]bool, bool, error) {
	m := managers[mgr]
	var quoted []string
	for _, p := range pkgs {
		quoted = append(quoted, transport.Quote(p))
	}
	qList := strings.Join(quoted, " ")

	var script string
	complete := true
	switch mgr {
	case "vite":
		script = fmt.Sprintf(`command -v %s >/dev/null 2>&1 || exit 4
out=$(vp list -g 2>&1) || { echo "the query of %s failed with exit code $?" >&2; exit 5; }
printf '%%s\n' "$out"`, m.tool, m.tool)
	case "apt":
		script = fmt.Sprintf(`command -v %s >/dev/null 2>&1 || exit 4
out=$(dpkg-query -W -f='${Package}\t${Status}\n' %s 2>/dev/null); rc=$?
[ $rc -le 1 ] || { echo "the query of %s failed with exit code $rc" >&2; exit 5; }
printf '%%s\n' "$out"`, m.tool, qList, m.tool)
	case "dnf":
		script = fmt.Sprintf(`command -v %s >/dev/null 2>&1 || exit 4
for p in %s; do rpm -q "$p" >/dev/null 2>&1 && printf '%%s\n' "$p"; done
exit 0`, m.tool, qList)
	case "pacman", "yay":
		script = fmt.Sprintf(`command -v %s >/dev/null 2>&1 || exit 4
for p in %s; do pacman -Q "$p" >/dev/null 2>&1 && printf '%%s\n' "$p"; done
exit 0`, m.tool, qList)
	case "apk":
		script = fmt.Sprintf(`command -v %s >/dev/null 2>&1 || exit 4
for p in %s; do apk info -e "$p" >/dev/null 2>&1 && printf '%%s\n' "$p"; done
exit 0`, m.tool, qList)
	case "uv":
		script = fmt.Sprintf(`command -v %s >/dev/null 2>&1 || exit 4
out=$(uv tool list 2>&1) || { echo "the query of %s failed with exit code $?" >&2; exit 5; }
printf '%%s\n' "$out"`, m.tool, m.tool)
	case "bun":
		script = fmt.Sprintf(`command -v %s >/dev/null 2>&1 || exit 4
out=$(bun pm ls -g 2>&1) || { case "$out" in *"No package.json"*) exit 0 ;; *) echo "the query of %s failed with exit code $?" >&2; exit 5 ;; esac; }
printf '%%s\n' "$out"`, m.tool, m.tool)
	case "cargo":
		script = fmt.Sprintf(`command -v %s >/dev/null 2>&1 || exit 4
[ -e "$HOME/.local/.crates.toml" ] || exit 0
cat "$HOME/.local/.crates.toml" || exit 5`, m.tool)
	case "go":
		script = fmt.Sprintf(`command -v %s >/dev/null 2>&1 || exit 4
for b in "$HOME/.local/bin/"*; do
  [ -f "$b" ] || continue
  TEST_TELEMETRY_DIR=/dev/null go version -m "$b" 2>/dev/null || true
done`, m.tool)
	case "brew":
		complete = false
		script = fmt.Sprintf(`command -v %s >/dev/null 2>&1 || exit 4
brew list --formula -1 2>/dev/null || true
brew list --cask -1 2>/dev/null || true`, m.tool)
	default:
		return nil, false, nil
	}

	r, err := env.UserScript(ctx, script, sudo, fmt.Sprintf("ask %s for %d packages", mgr, len(pkgs)), 2*time.Minute)
	if err != nil {
		return nil, false, err
	}
	if r.Code == 4 {
		return nil, false, errNoManager
	}
	if r.Code != 0 {
		return nil, false, sudoHint(&ExitError{Label: "ask " + mgr, Code: r.Code, Stderr: r.Err(), Stdout: r.Out()}, sudo, "package")
	}

	out := map[string]bool{}
	lines := strings.Split(string(r.Stdout), "\n")
	switch mgr {
	case "vite":
		for i, line := range lines {
			if i < 2 {
				continue
			}
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			n := f[0]
			if at := strings.LastIndexByte(n, '@'); at > 0 {
				n = n[:at]
			}
			if n != "" {
				out[n] = true
			}
		}
	case "apt":
		for _, line := range lines {
			pkg, status, ok := strings.Cut(line, "\t")
			if ok && strings.Contains(status, "install ok installed") {
				out[strings.TrimSpace(pkg)] = true
			}
		}
	case "dnf", "pacman", "yay", "apk", "brew":
		for _, line := range lines {
			if n := strings.TrimSpace(line); n != "" {
				out[n] = true
			}
		}
	case "uv":
		for _, line := range lines {
			if f := strings.Fields(line); len(f) > 0 {
				out[f[0]] = true
			}
		}
	case "bun":
		for _, line := range lines {
			for _, tok := range strings.Fields(line) {
				if at := strings.LastIndexByte(tok, '@'); at > 0 {
					out[tok[:at]] = true
				}
			}
		}
	case "cargo":
		for _, line := range lines {
			parts := strings.Split(line, "\"")
			if len(parts) >= 2 {
				if name, _, ok := strings.Cut(parts[1], " "); ok && name != "" {
					out[name] = true
				}
			}
		}
	case "go":
		for _, line := range lines {
			if f := strings.Fields(line); len(f) >= 2 && f[0] == "path" {
				out[f[1]] = true
			}
		}
	}
	return out, complete, nil
}
