package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gregor-tokarev/fam/internal/transport"
)

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func parseMode(s string, def os.FileMode) os.FileMode {
	if s == "" {
		return def
	}
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return def
	}
	return os.FileMode(n)
}

// FileTask is a file with a fixed content.
type FileTask struct {
	base
	Path    string
	Content []byte
	Mode    os.FileMode
	Absent  bool
	// Label replaces the path in output.
	Label string
	// Secret hides the content from output.
	Secret bool
	units  []Unit
}

// Kind implements Task.
func (t *FileTask) Kind() string { return "file" }

// Name implements Task.
func (t *FileTask) Name() string {
	if t.Label != "" {
		return t.Label
	}
	return t.Path
}

// Restarts implements Restarter.
func (t *FileTask) Restarts() []Unit { return t.units }

func (t *FileTask) specReqs(env *Env) []transport.SpecReq {
	return []transport.SpecReq{specStat(t.sudo, env.Path(t.Path))}
}

// Check implements Task.
func (t *FileTask) Check(ctx context.Context, env *Env) (Check, error) {
	p := env.Path(t.Path)
	fi, err := env.T.Stat(ctx, p, t.sudo)
	if err != nil {
		return Check{}, err
	}
	if t.Absent {
		switch {
		case !fi.Exists:
			return Check{Status: Satisfied}, nil
		case fi.Type == "file" || fi.Type == "link":
			return Check{Status: Change, Detail: "remove"}, nil
		default:
			return Check{}, fmt.Errorf("%s is a %s, not a file. FAM does not remove it", p, fi.Type)
		}
	}
	switch {
	case !fi.Exists:
		return Check{
			Status: Change,
			Detail: "create",
			Plan:   writePrimPlan(p, t.Content, t.Mode, t.sudo, t.Secret, true, "", "file"),
		}, nil
	case fi.Type != "file":
		return Check{}, fmt.Errorf("%s is a %s, not a regular file", p, fi.Type)
	}
	var diffs []string
	if fi.SHA256 != sha(t.Content) {
		diffs = append(diffs, "content differs")
	}
	if fi.Mode.Perm() != t.Mode.Perm() {
		diffs = append(diffs, fmt.Sprintf("mode %04o -> %04o", fi.Mode.Perm(), t.Mode.Perm()))
	}
	if len(diffs) == 0 {
		return Check{Status: Satisfied}, nil
	}
	return Check{
		Status: Change,
		Detail: strings.Join(diffs, ", "),
		Plan:   writePrimPlan(p, t.Content, t.Mode, t.sudo, t.Secret, false, fi.SHA256, "file"),
	}, nil
}

// Apply implements Task.
func (t *FileTask) Apply(ctx context.Context, env *Env) (string, error) {
	p := env.Path(t.Path)
	if t.Absent {
		_, err := env.ShOK(ctx, "rm -f -- "+transport.Quote(p), t.sudo, "remove "+p)
		return "removed", sudoHint(err, t.sudo, "file")
	}
	if err := env.T.WriteFile(ctx, p, t.Content, t.Mode, t.sudo); err != nil {
		return "", sudoHint(err, t.sudo, "file")
	}
	return "written", nil
}

// SymlinkTask is a symbolic link.
type SymlinkTask struct {
	base
	Path   string
	Target string
	Absent bool
	// Backup saves a regular file at Path next to it before FAM replaces it with the link.
	Backup bool
	units  []Unit
}

// Kind implements Task.
func (t *SymlinkTask) Kind() string { return "symlink" }

// Name implements Task.
func (t *SymlinkTask) Name() string { return t.Path + " -> " + t.Target }

// Restarts implements Restarter.
func (t *SymlinkTask) Restarts() []Unit { return t.units }

func (t *SymlinkTask) specReqs(env *Env) []transport.SpecReq {
	return []transport.SpecReq{specStat(t.sudo, env.Path(t.Path))}
}

// Check implements Task.
func (t *SymlinkTask) Check(ctx context.Context, env *Env) (Check, error) {
	p, target := env.Path(t.Path), env.Path(t.Target)
	fi, err := env.T.Stat(ctx, p, t.sudo)
	if err != nil {
		return Check{}, err
	}
	if t.Absent {
		switch {
		case !fi.Exists:
			return Check{Status: Satisfied}, nil
		case fi.Type == "link":
			return Check{Status: Change, Detail: "remove link"}, nil
		default:
			return Check{}, fmt.Errorf("%s is a %s, not a link. FAM does not remove it", p, fi.Type)
		}
	}
	switch {
	case !fi.Exists:
		return Check{
			Status: Change,
			Detail: "create link",
			Plan: &PrimPlan{
				Op:         transport.PrimOp{Op: "symlink", Label: "link " + p, Path: p, Target: target, Sudo: t.sudo, IfMissing: true},
				TargetPath: p,
				WantTarget: target,
				SudoKind:   "symlink",
			},
		}, nil
	case fi.Type == "link" && fi.Target == target:
		return Check{Status: Satisfied}, nil
	case fi.Type == "link":
		oldTarget := fi.Target
		return Check{
			Status: Change,
			Detail: "link points to " + fi.Target,
			Plan: &PrimPlan{
				Op:         transport.PrimOp{Op: "symlink", Label: "link " + p, Path: p, Target: target, Sudo: t.sudo, IfLinkTarget: &oldTarget},
				TargetPath: p,
				WantTarget: target,
				SudoKind:   "symlink",
			},
		}, nil
	case fi.Type == "file" && t.Backup:
		return Check{Status: Change, Detail: "replace the file with a link (FAM saves a copy)"}, nil
	default:
		return Check{}, fmt.Errorf("%s is a %s, not a link. FAM does not replace it", p, fi.Type)
	}
}

// Apply implements Task.
func (t *SymlinkTask) Apply(ctx context.Context, env *Env) (string, error) {
	p, target := env.Path(t.Path), env.Path(t.Target)
	qp := transport.Quote(p)
	if t.Absent {
		_, err := env.ShOK(ctx, fmt.Sprintf("[ -L %[1]s ] && rm -f -- %[1]s || true", qp), t.sudo, "remove link "+p)
		return "removed", sudoHint(err, t.sudo, "symlink")
	}
	script := fmt.Sprintf(`set -e
p=%[1]s
mkdir -p -- "$(dirname -- "$p")"
if [ -f "$p" ] && [ ! -L "$p" ]; then
  if [ %[3]s = yes ]; then cp -p -- "$p" "$p.fam-backup-$(date +%%Y%%m%%d%%H%%M%%S)"; else echo "$p is a file" >&2; exit 1; fi
fi
if [ -L "$p" ] || [ -f "$p" ]; then rm -f -- "$p"; fi
ln -s -- %[2]s "$p"`, qp, transport.Quote(target), yesNo(t.Backup))
	_, err := env.ShOK(ctx, script, t.sudo, "link "+p)
	return "linked", sudoHint(err, t.sudo, "symlink")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
