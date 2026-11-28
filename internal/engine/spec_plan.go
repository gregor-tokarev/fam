package engine

import (
	"time"

	"github.com/gregor-tokarev/fam/internal/transport"
)

// specProvider is implemented by tasks whose Check reads can be prefetched speculatively
// during Bootstrap. Each task keeps its specReqs method right beside Check so scripts and
// paths have a single source of truth.
type specProvider interface {
	specReqs(env *Env) []transport.SpecReq
}

func specStat(sudo bool, p string) transport.SpecReq {
	return transport.SpecReq{Key: transport.SpecKeyStat(sudo, p), Op: "stat", Path: p, Sudo: sudo}
}

func specRead(sudo bool, p string) transport.SpecReq {
	return transport.SpecReq{Key: transport.SpecKeyRead(sudo, p), Op: "read", Path: p, Sudo: sudo}
}

func specExec(sudo bool, fullScript string, timeout time.Duration) transport.SpecReq {
	return transport.SpecReq{Key: transport.SpecKeyExec(sudo, fullScript), Op: "exec", Script: fullScript, Sudo: sudo, TimeoutMS: timeout.Milliseconds()}
}

// buildSpecChecks collects deduplicated read-only wire requests that checkAll will need on
// the initial pass. All operations are strictly read-only.
func buildSpecChecks(env *Env, sects []Section) []transport.SpecReq {
	var all []Task
	for _, s := range sects {
		if s.SkipReason == "" {
			all = append(all, s.Tasks...)
		}
	}
	if len(all) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var reqs []transport.SpecReq
	add := func(r transport.SpecReq) {
		if seen[r.Key] {
			return
		}
		seen[r.Key] = true
		reqs = append(reqs, r)
	}

	mc := newManagerCache(all)
	if mc != nil {
		for k, entry := range mc.entries {
			if script, _, ok := managerBulkScript(k.mgr, entry.pkgs); ok {
				add(specExec(k.sudo, env.userScriptFull(script), 2*time.Minute))
			}
		}
	}

	for _, t := range all {
		if pt, ok := t.(*PackageTask); ok && pt.Manager != "" && mc != nil {
			if e, ok := mc.entries[mgrCacheKey{mgr: pt.Manager, sudo: pt.sudo}]; ok && e.queried[pt.PackageName] {
				continue
			}
		}
		if sp, ok := t.(specProvider); ok {
			for _, req := range sp.specReqs(env) {
				add(req)
			}
		}
	}
	return reqs
}
