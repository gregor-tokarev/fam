package engine

import (
	"context"
	"encoding/base64"
	"errors"

	"github.com/gregor-tokarev/fam/internal/transport"
)

// encodeApplyOp converts a task into a remote sequential apply op when the task can be checked,
// applied, and re-checked directly on the remote worker with exact FAM semantics.
func encodeApplyOp(ctx context.Context, env *Env, idx int, t Task) (transport.ApplyOp, bool) {
	stopAfter := t.Group() != "environment" && mayChangeShellEnv(t)
	switch x := t.(type) {
	case *FileTask:
		if x.Absent {
			return transport.ApplyOp{}, false
		}
		p := env.Path(x.Path)
		m := uint32(x.Mode.Perm())
		return transport.ApplyOp{
			Index:           idx,
			Kind:            "file",
			Label:           "write " + p,
			Secret:          x.Secret,
			Path:            p,
			DataB64:         base64.StdEncoding.EncodeToString(x.Content),
			SHA256:          sha(x.Content),
			Mode:            &m,
			Sudo:            x.sudo,
			StopAfterChange: stopAfter,
		}, true
	case *SymlinkTask:
		if x.Absent {
			return transport.ApplyOp{}, false
		}
		p := env.Path(x.Path)
		return transport.ApplyOp{
			Index:           idx,
			Kind:            "symlink",
			Label:           "link " + p,
			Path:            p,
			Target:          env.Path(x.Target),
			Backup:          x.Backup,
			Sudo:            x.sudo,
			StopAfterChange: stopAfter,
		}, true
	case *SkillLinkTask:
		p := env.Home(".claude/skills/" + x.Name_)
		return transport.ApplyOp{
			Index:          idx,
			Kind:           "symlink",
			Label:          "link skill " + x.Name_,
			Path:           p,
			Target:         env.Home(agentsSkills + "/" + x.Name_),
			StrictConflict: true,
		}, true
	case *BlockTask:
		p := env.Path(x.Path)
		m := uint32(x.Mode.Perm())
		return transport.ApplyOp{
			Index:           idx,
			Kind:            "block",
			Label:           "write " + p,
			Path:            p,
			Block:           x.Block,
			BlockBegin:      blockBegin,
			BlockEnd:        blockEnd,
			Top:             x.Top,
			OnlyIfExists:    x.OnlyIfExists,
			Mode:            &m,
			StopAfterChange: stopAfter,
		}, true
	case *SkillTask:
		src, err := x.resolve(ctx)
		if err != nil {
			return transport.ApplyOp{}, false
		}
		tb, err := src.tarball()
		if err != nil {
			return transport.ApplyOp{}, false
		}
		return transport.ApplyOp{
			Index:          idx,
			Kind:           "skill",
			Label:          "copy skill " + src.Name,
			Dir:            x.dir(env),
			Marker:         managedMarker,
			ExpectedDigest: src.digest(),
			TarB64:         base64.StdEncoding.EncodeToString(tb),
		}, true
	case *ManagedFileTask:
		p := env.Path(x.Path)
		return transport.ApplyOp{
			Index:         idx,
			Kind:          "managed_file",
			Label:         "write " + p,
			Path:          p,
			DataB64:       base64.StdEncoding.EncodeToString(x.Content),
			SHA256:        sha(x.Content),
			ManagedPrefix: managedPrefix,
		}, true
	case *ScriptTask:
		// Only scripts with a file-existence (`creates`) check run inside apply_seq; shell-checked
		// scripts run via applyTask so the worker session refreshes before the post-apply check.
		if x.CheckSh != "" {
			return transport.ApplyOp{}, false
		}
		return transport.ApplyOp{
			Index:           idx,
			Kind:            "script",
			Label:           "apply " + x.Title,
			Creates:         env.Path(x.Creates),
			CreatesDisplay:  x.Creates,
			ApplyScript:     x.ApplySh,
			Prelude:         env.prelude(),
			Sudo:            x.sudo,
			StopAfterChange: stopAfter,
			TimeoutMS:       scriptTimeout.Milliseconds(),
		}, true
	}
	return transport.ApplyOp{}, false
}

func decodeApplyOpResult(res transport.ApplyOpResult) (st Status, detail string, err error, tc touch) {
	switch res.Status {
	case "unchanged":
		return Unchanged, res.Detail, nil, touchNone
	case "changed":
		return Changed, res.Detail, nil, touchApply
	case "conflict":
		return Conflict, "", conflict(res.Err), touchNone
	default:
		if res.Detail == "check failed" {
			return Failed, "check failed", errors.New(res.Err), touchNone
		}
		return Failed, res.Detail, errors.New(res.Err), touchApply
	}
}
