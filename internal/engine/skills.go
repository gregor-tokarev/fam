package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gregor-tokarev/fam/internal/config"
	"github.com/gregor-tokarev/fam/internal/pklgen/bundle"
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

// SkillTask copies one skill to ~/.agents/skills/<name>.
type SkillTask struct {
	base
	Skill *SkillSource
}

// Kind implements Task.
func (t *SkillTask) Kind() string { return "skill" }

// Name implements Task.
func (t *SkillTask) Name() string { return t.Skill.Name + " (" + t.Skill.Origin + ")" }

func (t *SkillTask) dir(env *Env) string { return env.Home(agentsSkills + "/" + t.Skill.Name) }

// Check implements Task.
func (t *SkillTask) Check(ctx context.Context, env *Env) (Check, error) {
	d := t.dir(env)
	fi, err := env.T.Stat(ctx, d, false)
	if err != nil {
		return Check{}, err
	}
	if !fi.Exists {
		return Check{Status: Change, Detail: "copy"}, nil
	}
	if fi.Type != "dir" {
		return Check{}, conflict(fmt.Sprintf("%s exists and FAM did not make it. Remove or rename it, or rename the skill", d))
	}
	m, err := env.T.Stat(ctx, d+"/"+managedMarker, false)
	if err != nil {
		return Check{}, err
	}
	if !m.Exists {
		return Check{}, conflict(fmt.Sprintf("the skill folder %s exists and FAM did not make it. Remove or rename it, or rename the skill", d))
	}
	got, err := remoteDigest(ctx, env, d)
	if err != nil {
		return Check{}, err
	}
	if got == t.Skill.digest() {
		return Check{Status: Satisfied}, nil
	}
	return Check{Status: Change, Detail: "update"}, nil
}

// Apply implements Task. FAM extracts the skill into a temporary folder, then swaps the folders.
func (t *SkillTask) Apply(ctx context.Context, env *Env) (string, error) {
	tb, err := t.Skill.tarball()
	if err != nil {
		return "", err
	}
	d := t.dir(env)
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
trap - EXIT`, transport.Quote(d), t.Skill.Name, managedMarker)
	r, err := env.T.Run(ctx, transport.Cmd{Script: script, Stdin: tb, Label: "copy skill " + t.Skill.Name})
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
		return Check{Status: Change, Detail: "link to " + want}, nil
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
}

// Kind implements Task.
func (t *SkillPruneTask) Kind() string { return "skills" }

// Name implements Task.
func (t *SkillPruneTask) Name() string { return "remove old skills" }

func (t *SkillPruneTask) stale(ctx context.Context, env *Env) ([]string, error) {
	script := fmt.Sprintf(`d=%s
[ -d "$d" ] || exit 0
for m in "$d"/*/%s; do [ -f "$m" ] && basename "$(dirname "$m")"; done; exit 0`, transport.Quote(env.Home(agentsSkills)), managedMarker)
	r, err := env.ShOK(ctx, script, false, "list managed skills")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range strings.Split(r.Out(), "\n") {
		n = strings.TrimSpace(n)
		if n != "" && !t.Keep[n] {
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

// skillsSection copies the project skills and the skills of present bundles,
// and removes the skills of absent bundles and old skills.
func skillsSection(ctx context.Context, env *Env, spec *config.Spec) (Section, error) {
	g := base{group: "skills"}
	sec := Section{Name: "skills"}
	var list []*SkillSource
	byName := map[string]*SkillSource{}
	add := func(s *SkillSource) error {
		if prev, ok := byName[s.Name]; ok {
			return fmt.Errorf("two skills have the name %q: %s and %s", s.Name, prev.Dir, s.Dir)
		}
		byName[s.Name] = s
		list = append(list, s)
		return nil
	}
	if env.Project.SkillsDir != "" {
		entries, err := os.ReadDir(env.Project.SkillsDir)
		if err != nil && !os.IsNotExist(err) {
			return sec, err
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			// os.Stat follows a symbolic link to a folder.
			if st, err := os.Stat(filepath.Join(env.Project.SkillsDir, e.Name())); err != nil || !st.IsDir() {
				continue
			}
			s, err := loadSkill(e.Name(), filepath.Join(env.Project.SkillsDir, e.Name()), "skills/")
			if err != nil {
				return sec, err
			}
			if err := add(s); err != nil {
				return sec, err
			}
		}
	}
	for _, u := range spec.Bundles {
		if u.State != "present" || u.Bundle.Skill == nil {
			continue
		}
		var s *SkillSource
		switch sk := (*u.Bundle.Skill).(type) {
		case string:
			dir := filepath.Join(u.Dir, sk)
			name := filepath.Base(filepath.Clean(dir))
			if name == "skill" || name == "." {
				name = u.Bundle.Name
			}
			ls, err := loadSkill(name, dir, "bundle "+u.Bundle.Name)
			if err != nil {
				return sec, err
			}
			s = ls
		case bundle.GitHubSkill, *bundle.GitHubSkill:
			gs, _ := sk.(bundle.GitHubSkill)
			if p, ok := sk.(*bundle.GitHubSkill); ok {
				gs = *p
			}
			gsrc, err := loadGitHubSkill(ctx, gs, "bundle "+u.Bundle.Name)
			if err != nil {
				return sec, err
			}
			s = gsrc
		default:
			return sec, fmt.Errorf("bundle %s: skill has the type %T. Use a folder name or new Bundle.GitHubSkill { … }", u.Bundle.Name, sk)
		}
		if err := add(s); err != nil {
			return sec, err
		}
	}
	keep := map[string]bool{}
	for _, s := range list {
		keep[s.Name] = true
		sec.Tasks = append(sec.Tasks, &SkillTask{base: g, Skill: s}, &SkillLinkTask{base: g, Name_: s.Name})
	}
	sec.Tasks = append(sec.Tasks, &SkillPruneTask{base: g, Keep: keep})
	return sec, nil
}
