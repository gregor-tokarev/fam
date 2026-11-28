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
	return managers[mgr].bulkScript != nil
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

func managerBulkScript(mgr string, pkgs []string) (script string, complete bool, ok bool) {
	m := managers[mgr]
	if m.bulkScript == nil {
		return "", false, false
	}
	quoted := make([]string, len(pkgs))
	for i, p := range pkgs {
		quoted[i] = transport.Quote(p)
	}
	return m.bulkScript(m.tool, strings.Join(quoted, " ")), m.bulkComplete, true
}

func queryManagerBulk(ctx context.Context, env *Env, mgr string, pkgs []string, sudo bool) (map[string]bool, bool, error) {
	script, complete, ok := managerBulkScript(mgr, pkgs)
	if !ok {
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
	return managers[mgr].bulkParse(strings.Split(string(r.Stdout), "\n")), complete, nil
}
