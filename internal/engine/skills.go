package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gregor-tokarev/fam/internal/transport"
)

// skillFile is one file of a skill folder.
type skillFile struct {
	rel  string
	data []byte
	exec bool
}

// SkillSource is one skill on the cockpit.
type SkillSource struct {
	Name   string
	Dir    string
	Origin string
	files  []skillFile
}

func loadSkill(name, dir, origin string) (*SkillSource, error) {
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		return nil, fmt.Errorf("skill %s (%s): %s has no SKILL.md", name, origin, dir)
	}
	s := &SkillSource{Name: name, Dir: dir, Origin: origin}
	if err := s.walk(dir, "", 0); err != nil {
		return nil, fmt.Errorf("skill %s (%s): %w", name, origin, err)
	}
	sort.Slice(s.files, func(i, j int) bool { return s.files[i].rel < s.files[j].rel })
	return s, nil
}

// walk reads the files of a skill folder. It follows symbolic links to files and folders
// (os.Stat), so that linked resources are part of the copy.
func (s *SkillSource) walk(dir, prefix string, depth int) error {
	if depth > 16 {
		return fmt.Errorf("%s: the folders are more than 16 levels deep (a link loop?)", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		rel := path.Join(prefix, e.Name())
		if rel == managedMarker {
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := s.walk(p, rel, depth+1); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			s.files = append(s.files, skillFile{rel: rel, data: data, exec: info.Mode().Perm()&0o100 != 0})
		}
	}
	return nil
}

// digest is the content digest of the skill: path, exec bit, and SHA-256 of each file.
func (s *SkillSource) digest() string {
	var lines []string
	for _, f := range s.files {
		h := sha256.Sum256(f.data)
		lines = append(lines, fmt.Sprintf("%s %s %v", hex.EncodeToString(h[:]), f.rel, f.exec))
	}
	return digestLines(lines)
}

func digestLines(lines []string) string {
	sort.Strings(lines)
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h[:])
}

func (s *SkillSource) tarball() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	for _, f := range s.files {
		for d := path.Dir(f.rel); d != "." && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	var dl []string
	for d := range dirs {
		dl = append(dl, d)
	}
	sort.Strings(dl)
	for _, d := range dl {
		if err := tw.WriteHeader(&tar.Header{Name: d + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			return nil, err
		}
	}
	for _, f := range s.files {
		mode := int64(0o644)
		if f.exec {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.rel, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(f.data))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.data); err != nil {
			return nil, err
		}
	}
	marker := []byte("This skill is managed by FAM. FAM replaces or removes this folder.\n")
	if err := tw.WriteHeader(&tar.Header{Name: managedMarker, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(marker))}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(marker); err != nil {
		return nil, err
	}
	err := tw.Close()
	return buf.Bytes(), err
}

// remoteDigest computes the digest of a skill folder on the machine. The script prints one line
// for each file: the path in hex (any byte is safe, also a backslash, a space, or a newline),
// the SHA-256 of the content (read from stdin, so the hash tool never escapes the name), and
// the exec bit of the owner.
func remoteDigest(ctx context.Context, env *Env, dir string) (string, error) {
	script := fmt.Sprintf(`set -e
cd %s
find . -type f ! -path ./%s -exec sh -c '
set -e
for f do
  n=$(printf "%%s" "${f#./}" | od -An -v -tx1 | tr -d " \n")
  h=$(%s < "$f" | awk "{print \$1}")
  x=false; [ -z "$(find "$f" -prune -perm -u+x)" ] || x=true
  printf "%%s %%s %%s\n" "$n" "$h" "$x"
done' sh {} +`, transport.Quote(dir), managedMarker, env.T.SHA256Cmd())
	r, err := env.ShOK(ctx, script, false, "digest of "+dir)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(r.Out()), "\n") {
		f := strings.Fields(l)
		if len(f) != 3 {
			continue
		}
		name, err := hex.DecodeString(f[0])
		if err != nil || len(f[1]) != 64 {
			return "", fmt.Errorf("digest of %s: unexpected line %q", dir, l)
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", f[1], name, f[2]))
	}
	return digestLines(lines), nil
}

// skillLoadError marks a failure while downloading or unpacking a remote skill source.
// Plan and apply treat it identically to a config-stage build error.
type skillLoadError struct{ err error }

func (e *skillLoadError) Error() string { return e.err.Error() }
func (e *skillLoadError) Unwrap() error { return e.err }

// asyncSkill resolves a remote skill source in the background while remote checks run.
type asyncSkill struct {
	name   string
	dir    string
	origin string
	done   chan struct{}
	src    *SkillSource
	err    error
}

var skillFetchSem = make(chan struct{}, 8)

func startAsyncSkill(ctx context.Context, spec skillSpec, name, origin string) *asyncSkill {
	a := &asyncSkill{name: name, dir: spec.String(), origin: origin, done: make(chan struct{})}
	go func() {
		defer close(a.done)
		select {
		case skillFetchSem <- struct{}{}:
			defer func() { <-skillFetchSem }()
		case <-ctx.Done():
			a.err = ctx.Err()
			return
		}
		a.src, a.err = spec.load(ctx, name, origin)
	}()
	return a
}

func resolvedSkill(src *SkillSource) *asyncSkill {
	ch := make(chan struct{})
	close(ch)
	return &asyncSkill{name: src.Name, dir: src.Dir, origin: src.Origin, done: ch, src: src}
}

func (a *asyncSkill) wait(ctx context.Context) (*SkillSource, error) {
	select {
	case <-a.done:
		return a.src, a.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SkillTask copies one skill to ~/.agents/skills/<name>.
// Always uses loader (already completed for local/static skills) so there is only one code path.
type SkillTask struct {
	base
	loader *asyncSkill
	units  []Unit
}

// Restarts implements Restarter.
func (t *SkillTask) Restarts() []Unit { return t.units }

// Kind implements Task.
func (t *SkillTask) Kind() string { return "skill" }

func (t *SkillTask) skillName() string   { return t.loader.name }
func (t *SkillTask) skillDir() string    { return t.loader.dir }
func (t *SkillTask) skillOrigin() string { return t.loader.origin }

func (t *SkillTask) resolve(ctx context.Context) (*SkillSource, error) {
	src, err := t.loader.wait(ctx)
	if err != nil {
		return nil, &skillLoadError{err: err}
	}
	return src, nil
}

// resolveAllSkills waits for every async remote skill loader in sects and returns the first load
// error (unwrapped) so plan, apply, and verify always report broken remote skill sources before
// any target mutation or verification.
func resolveAllSkills(ctx context.Context, sects []Section) error {
	for _, s := range sects {
		if s.SkipReason != "" {
			continue
		}
		for _, t := range s.Tasks {
			st, ok := t.(*SkillTask)
			if !ok {
				continue
			}
			if _, err := st.resolve(ctx); err != nil {
				var sle *skillLoadError
				if errors.As(err, &sle) {
					return sle.err
				}
				return err
			}
		}
	}
	return nil
}

// Name implements Task.
func (t *SkillTask) Name() string { return t.skillName() + " (" + t.skillOrigin() + ")" }

func (t *SkillTask) dir(env *Env) string { return env.Home(agentsSkills + "/" + t.skillName()) }

func (t *SkillTask) specReqs(env *Env) []transport.SpecReq {
	dir := t.dir(env)
	return []transport.SpecReq{{
		Key:    transport.SpecKeySkill(dir, managedMarker),
		Op:     "skill_check",
		Dir:    dir,
		Marker: managedMarker,
	}}
}

// Check implements Task.
func (t *SkillTask) Check(ctx context.Context, env *Env) (Check, error) {
	d := t.dir(env)
	var st transport.SkillState
	if ops, ok := env.T.(transport.SkillOps); ok && ops.HasNativeSkillOps() {
		var err error
		st, err = ops.SkillCheck(ctx, d, managedMarker)
		if err != nil {
			return Check{}, err
		}
	} else {
		fi, err := env.T.Stat(ctx, d, false)
		if err != nil {
			return Check{}, err
		}
		if fi.Exists {
			st.Exists = true
			st.IsDir = fi.Type == "dir"
			if st.IsDir {
				m, err := env.T.Stat(ctx, d+"/"+managedMarker, false)
				if err != nil {
					return Check{}, err
				}
				st.Managed = m.Exists
				if st.Managed {
					got, err := remoteDigest(ctx, env, d)
					if err != nil {
						return Check{}, err
					}
					st.Digest = got
				}
			}
		}
	}
	src, err := t.resolve(ctx)
	if err != nil {
		return Check{}, err
	}
	if !st.Exists {
		return Check{
			Status: Change,
			Detail: "copy",
			Plan: &PrimPlan{
				Op:         transport.PrimOp{Op: "skill_write", Label: "copy skill " + src.Name, Dir: d, Marker: managedMarker, Verify: true, IfMissing: true},
				TargetPath: d,
				WantDigest: src.digest(),
				Tarball:    src.tarball,
			},
		}, nil
	}
	if !st.IsDir {
		return Check{}, conflict(fmt.Sprintf("%s exists and FAM did not make it. Remove or rename it, or rename the skill", d))
	}
	if !st.Managed {
		return Check{}, conflict(fmt.Sprintf("the skill folder %s exists and FAM did not make it. Remove or rename it, or rename the skill", d))
	}
	if st.Digest == src.digest() {
		return Check{Status: Satisfied}, nil
	}
	return Check{
		Status: Change,
		Detail: "update",
		Plan: &PrimPlan{
			Op:         transport.PrimOp{Op: "skill_write", Label: "copy skill " + src.Name, Dir: d, Marker: managedMarker, Verify: true, IfDigest: st.Digest},
			TargetPath: d,
			WantDigest: src.digest(),
			Tarball:    src.tarball,
		},
	}, nil
}

// Apply implements Task. FAM extracts the skill into a temporary folder, then swaps the folders.
func (t *SkillTask) Apply(ctx context.Context, env *Env) (string, error) {
	src, err := t.resolve(ctx)
	if err != nil {
		return "", err
	}
	tb, err := src.tarball()
	if err != nil {
		return "", err
	}
	d := t.dir(env)
	if ops, ok := env.T.(transport.SkillOps); ok && ops.HasNativeSkillOps() {
		if err := ops.SkillWrite(ctx, d, managedMarker, tb); err != nil {
			return "", err
		}
		return "copied", nil
	}
	script := fmt.Sprintf(`set -e
d=%[1]s
mkdir -p -- "$(dirname -- "$d")"
t=$(mktemp -d "$(dirname -- "$d")/.fam-tmp-%[2]s.XXXXXX")
trap 'rm -rf -- "$t"' EXIT
tar -xf - -C "$t"
chmod 755 "$t"
if [ -d "$d" ]; then
  [ -f "$d/%[3]s" ] || { echo "$d exists and FAM did not make it" >&2; exit 3; }
  rm -rf -- "$d"
fi
mv -- "$t" "$d"
trap - EXIT`, transport.Quote(d), src.Name, managedMarker)
	r, err := env.T.Run(ctx, transport.Cmd{Script: script, Stdin: tb, Label: "copy skill " + src.Name})
	if err != nil {
		return "", err
	}
	if r.Code != 0 {
		return "", &ExitError{Label: "copy skill", Code: r.Code, Stderr: r.Err()}
	}
	return "copied", nil
}

// SkillLinkTask links ~/.claude/skills/<name> to the skill.
type SkillLinkTask struct {
	base
	Name_ string
}

// Kind implements Task.
func (t *SkillLinkTask) Kind() string { return "symlink" }

// Name implements Task.
func (t *SkillLinkTask) Name() string { return "~/.claude/skills/" + t.Name_ }

func (t *SkillLinkTask) specReqs(env *Env) []transport.SpecReq {
	return []transport.SpecReq{specStat(false, env.Home(".claude/skills/"+t.Name_))}
}

// Check implements Task.
func (t *SkillLinkTask) Check(ctx context.Context, env *Env) (Check, error) {
	p := env.Home(".claude/skills/" + t.Name_)
	want := env.Home(agentsSkills + "/" + t.Name_)
	fi, err := env.T.Stat(ctx, p, false)
	if err != nil {
		return Check{}, err
	}
	switch {
	case !fi.Exists:
		return Check{
			Status: Change,
			Detail: "link to " + want,
			Plan: &PrimPlan{
				Op:         transport.PrimOp{Op: "symlink", Label: "link skill " + t.Name_, Path: p, Target: want, IfMissing: true},
				TargetPath: p,
				WantTarget: want,
			},
		}, nil
	case fi.Type == "link" && fi.Target == want:
		return Check{Status: Satisfied}, nil
	default:
		return Check{}, conflict(fmt.Sprintf("%s exists and FAM did not make it. Remove or rename it, or rename the skill", p))
	}
}

// Apply implements Task.
func (t *SkillLinkTask) Apply(ctx context.Context, env *Env) (string, error) {
	p := env.Home(".claude/skills/" + t.Name_)
	want := env.Home(agentsSkills + "/" + t.Name_)
	_, err := env.ShOK(ctx, fmt.Sprintf(`mkdir -p -- "$(dirname -- %[1]s)" && ln -s -- %[2]s %[1]s`, transport.Quote(p), transport.Quote(want)), false, "link skill "+t.Name_)
	return "linked", err
}

// SkillPruneTask removes skills that FAM manages and that the config does not list any more.
type SkillPruneTask struct {
	base
	Keep map[string]bool
	// Legacy holds the folders of skills/ in the project that no bundle of the machine lists.
	// Earlier FAM versions copied them to every machine, so FAM does not remove them silently.
	Legacy map[string]bool
}

// Kind implements Task.
func (t *SkillPruneTask) Kind() string { return "skills" }

// Name implements Task.
func (t *SkillPruneTask) Name() string { return "remove old skills" }

func (t *SkillPruneTask) listScript(env *Env) string {
	return fmt.Sprintf(`d=%s
[ -d "$d" ] || exit 0
for m in "$d"/*/%s; do [ -f "$m" ] && basename "$(dirname "$m")"; done; exit 0`, transport.Quote(env.Home(agentsSkills)), managedMarker)
}

func (t *SkillPruneTask) specReqs(env *Env) []transport.SpecReq {
	return []transport.SpecReq{specExec(false, t.listScript(env), 10*time.Minute)}
}

func (t *SkillPruneTask) stale(ctx context.Context, env *Env) ([]string, error) {
	r, err := env.ShOK(ctx, t.listScript(env), false, "list managed skills")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range strings.Split(r.Out(), "\n") {
		n = strings.TrimSpace(n)
		if n != "" && !t.Keep[n] {
			if t.Legacy[n] {
				return nil, conflict(fmt.Sprintf("FAM no longer copies the folders of skills/ to every machine, and no bundle of this machine lists the skill %[1]q. Add `new fam.AgentSkill { source = \"../skills/%[1]s\" }` to a bundle in bundles/ to keep it, or add it with `state = \"absent\"` to remove it", n))
			}
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Check implements Task.
func (t *SkillPruneTask) Check(ctx context.Context, env *Env) (Check, error) {
	s, err := t.stale(ctx, env)
	if err != nil {
		return Check{}, err
	}
	if len(s) == 0 {
		return Check{Status: Satisfied}, nil
	}
	return Check{Status: Change, Detail: "remove " + strings.Join(s, ", ")}, nil
}

// Apply implements Task. It also removes the Claude link of each removed skill.
func (t *SkillPruneTask) Apply(ctx context.Context, env *Env) (string, error) {
	s, err := t.stale(ctx, env)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, n := range s {
		d := env.Home(agentsSkills + "/" + n)
		l := env.Home(".claude/skills/" + n)
		fmt.Fprintf(&b, "if [ -L %[1]s ] && [ \"$(readlink -- %[1]s)\" = %[2]s ]; then rm -f -- %[1]s; fi\n", transport.Quote(l), transport.Quote(d))
		fmt.Fprintf(&b, "[ -f %[1]s/%[2]s ] && rm -rf -- %[1]s\n", transport.Quote(d), managedMarker)
	}
	_, err = env.ShOK(ctx, b.String(), false, "remove old skills")
	return "removed " + strings.Join(s, ", "), err
}

// skillsSection removes the managed skills that the config no longer lists. keep holds the
// skills of the bundles; listed holds every skill name that a bundle of the machine lists, also
// an absent or skipped one.
func skillsSection(env *Env, keep, listed map[string]bool) (Section, error) {
	g := base{group: "skills"}
	sec := Section{Name: "skills"}
	legacy := map[string]bool{}
	dir := filepath.Join(env.Project.Base, "skills")
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return sec, err
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "SKILL.md")); err == nil && !listed[e.Name()] {
			legacy[e.Name()] = true
		}
	}
	sec.Tasks = append(sec.Tasks, &SkillPruneTask{base: g, Keep: keep, Legacy: legacy})
	return sec, nil
}

// SkillRemoveTask removes one skill that FAM manages, and its Claude link: a skill task with
// the state "absent", or a skill of an absent bundle. A folder that FAM did not make stays.
type SkillRemoveTask struct {
	base
	Name_ string
	units []Unit
}

// Restarts implements Restarter.
func (t *SkillRemoveTask) Restarts() []Unit { return t.units }

// Kind implements Task.
func (t *SkillRemoveTask) Kind() string { return "skill" }

// Name implements Task.
func (t *SkillRemoveTask) Name() string { return t.Name_ }

func (t *SkillRemoveTask) script(env *Env) string {
	d := transport.Quote(env.Home(agentsSkills + "/" + t.Name_))
	l := transport.Quote(env.Home(".claude/skills/" + t.Name_))
	return fmt.Sprintf(`d=%[1]s; l=%[2]s
mine=; [ -f "$d/%[3]s" ] && mine=1
link=; [ -L "$l" ] && [ "$(readlink -- "$l")" = "$d" ] && link=1`, d, l, managedMarker)
}

func (t *SkillRemoveTask) checkScript(env *Env) string {
	return t.script(env) + "\n" + `[ -n "$mine$link" ] && echo change; exit 0`
}

func (t *SkillRemoveTask) specReqs(env *Env) []transport.SpecReq {
	return []transport.SpecReq{specExec(false, t.checkScript(env), 10*time.Minute)}
}

// Check implements Task.
func (t *SkillRemoveTask) Check(ctx context.Context, env *Env) (Check, error) {
	r, err := env.ShOK(ctx, t.checkScript(env), false, "check skill "+t.Name_)
	if err != nil {
		return Check{}, err
	}
	if strings.TrimSpace(r.Out()) == "change" {
		return Check{Status: Change, Detail: "remove"}, nil
	}
	return Check{Status: Satisfied}, nil
}

// Apply implements Task.
func (t *SkillRemoveTask) Apply(ctx context.Context, env *Env) (string, error) {
	_, err := env.ShOK(ctx, t.script(env)+"\n"+`set -e
[ -z "$link" ] || rm -f -- "$l"
[ -z "$mine" ] || rm -rf -- "$d"`, false, "remove skill "+t.Name_)
	return "removed", err
}
