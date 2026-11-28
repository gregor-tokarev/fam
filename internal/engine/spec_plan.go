package engine

import (
	"fmt"
	"time"

	"github.com/gregor-tokarev/fam/internal/transport"
)

// specKey helpers produce canonical lookup keys shared between speculative pre-checks and SSH.
func specStatKey(sudo bool, p string) string { return fmt.Sprintf("stat:%t:%s", sudo, p) }
func specReadKey(sudo bool, p string) string { return fmt.Sprintf("read:%t:%s", sudo, p) }
func specSkillKey(dir, marker string) string { return "skill:" + dir + "\x00" + marker }
func specExecKey(sudo bool, script string) string {
	return fmt.Sprintf("exec:%t:%s", sudo, script)
}

// buildSpecChecks inspects the speculatively built sections and emits deduplicated read-only
// wire requests that checkAll will need on the initial pass. All operations are strictly read-only.
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
	addStat := func(sudo bool, p string) {
		add(transport.SpecReq{Key: specStatKey(sudo, p), Op: "stat", Path: p, Sudo: sudo})
	}
	addRead := func(sudo bool, p string) {
		add(transport.SpecReq{Key: specReadKey(sudo, p), Op: "read", Path: p, Sudo: sudo})
	}
	addExec := func(sudo bool, fullScript string, timeout time.Duration) {
		add(transport.SpecReq{Key: specExecKey(sudo, fullScript), Op: "exec", Script: fullScript, Sudo: sudo, TimeoutMS: timeout.Milliseconds()})
	}

	mc := newManagerCache(all)
	if mc != nil {
		for k, entry := range mc.entries {
			if script, _, ok := managerBulkScript(k.mgr, entry.pkgs); ok {
				addExec(k.sudo, env.userScriptFull(script), 2*time.Minute)
			}
		}
	}

	for _, t := range all {
		switch x := t.(type) {
		case *FileTask:
			addStat(x.sudo, env.Path(x.Path))
		case *SymlinkTask:
			addStat(x.sudo, env.Path(x.Path))
		case *SkillLinkTask:
			addStat(false, env.Home(".claude/skills/"+x.Name_))
		case *ManagedFileTask:
			addStat(false, env.Path(x.Path))
		case *BlockTask:
			addRead(false, env.Path(x.Path))
		case *MergeTask:
			addRead(x.sudo, env.Path(x.Path))
		case *SkillTask:
			dir := x.dir(env)
			add(transport.SpecReq{Key: specSkillKey(dir, managedMarker), Op: "skill_check", Dir: dir, Marker: managedMarker})
		case *SkillPruneTask:
			script := fmt.Sprintf("d=%s\n[ -d \"$d\" ] || exit 0\nfor m in \"$d\"/*/%s; do [ -f \"$m\" ] && basename \"$(dirname \"$m\")\"; done; exit 0", transport.Quote(env.Home(agentsSkills)), managedMarker)
			addExec(false, script, 10*time.Minute)
		case *SkillRemoveTask:
			addExec(false, x.script(env)+"\n"+`[ -n "$mine$link" ] && echo change; exit 0`, 10*time.Minute)
		case *SshdEnvTask:
			addExec(true, `out=$(sshd -T) || exit 5; printf '%s\n' "$out" | grep -i '^permituserenvironment ' || true`, 10*time.Minute)
		case *OwnedRemoveTask:
			addRead(false, env.Path(x.Path))
		case *ScriptTask:
			if x.CheckSh == "" {
				addStat(x.sudo, env.Path(x.Creates))
			} else {
				addExec(x.sudo, env.userScriptFull(x.CheckSh), scriptTimeout)
			}
		case *PackageTask:
			if x.Manager != "" {
				if mc != nil {
					if e, ok := mc.entries[mgrCacheKey{mgr: x.Manager, sudo: x.sudo}]; ok && e.queried[x.PackageName] {
						continue
					}
				}
				addExec(x.sudo, env.userScriptFull(managerCheck(x.Manager, x.PackageName)), 2*time.Minute)
			} else {
				script := "if command -v " + transport.Quote(x.bin(env)) + " >/dev/null 2>&1; then exit 0; else exit 3; fi"
				addExec(x.sudo, env.userScriptFull(script), 2*time.Minute)
			}
		case *GitHubReleaseTask:
			addStat(false, x.binary(env))
			addStat(false, x.recordPath(env))
		}
	}
	return reqs
}
