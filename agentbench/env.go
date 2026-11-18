package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	beginMark = "<!-- fam:begin -->"
	endMark   = "<!-- fam:end -->"
	guidesDir = "agentbench/guides"
)

// Env is the shared state of one benchmark run.
type Env struct {
	Root      string // output directory
	Bin       string // fam binary under test
	Home      string // HOME of every fam process
	Fixture   string // the project that each run copies
	Pkl       string // pinned pkl binary
	SSHConfig string
	Version   string
	HasCheck  bool
}

// famEnv is the environment of fam and pkl processes: their own HOME and caches.
func (e *Env) famEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "XDG_CACHE_HOME=") || strings.HasPrefix(kv, "FAM_SSH_CONFIG=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+e.Home, "XDG_CACHE_HOME="+filepath.Join(e.Home, ".cache"), "FAM_SSH_CONFIG="+e.SSHConfig)
}

func (e *Env) fam(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.Bin, args...)
	cmd.Dir = dir
	cmd.Env = e.famEnv()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func prepare(ctx context.Context, root, bin string) (*Env, error) {
	e := &Env{Root: root, Bin: bin, Home: filepath.Join(root, "home"), Fixture: filepath.Join(root, "fixture", "fleet")}
	for _, d := range []string{e.Home, filepath.Join(root, "runs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// Every host is unreachable at once: runs must never touch a real machine.
	e.SSHConfig = filepath.Join(root, "ssh_config")
	if err := os.WriteFile(e.SSHConfig, []byte("Host *\n  HostName 127.0.0.1\n  Port 9\n  ConnectTimeout 2\n  BatchMode yes\n"), 0o644); err != nil {
		return nil, err
	}
	// Copy the pinned pkl binary of the user cache, so that no run downloads it.
	if out, err := exec.CommandContext(ctx, bin, "internal", "pkl-path").Output(); err == nil {
		real := strings.TrimSpace(string(out))
		if uc, err := os.UserCacheDir(); err == nil && strings.HasPrefix(real, uc+"/") {
			dst := filepath.Join(e.Home, ".cache", strings.TrimPrefix(real, uc+"/"))
			if _, err := os.Stat(dst); err != nil {
				os.MkdirAll(filepath.Dir(dst), 0o755)
				if b, err := os.ReadFile(real); err == nil {
					os.WriteFile(dst, b, 0o755)
				}
			}
		}
	}
	v, err := e.fam(ctx, root, "version")
	if err != nil {
		return nil, fmt.Errorf("fam version: %v: %s", err, v)
	}
	e.Version = regexp.MustCompile(`\d+\.\d+\.\d+`).FindString(v)
	if _, err := os.Stat(e.Fixture); err != nil {
		if out, err := e.fam(ctx, root, "new", "--base", e.Fixture); err != nil {
			return nil, fmt.Errorf("fam new: %v: %s", err, out)
		}
		if err := os.WriteFile(filepath.Join(e.Fixture, "machines.pkl"), []byte(machinesFixture), 0o644); err != nil {
			return nil, err
		}
		// The secrets source lives outside the project, in the HOME of fam.
		sec := filepath.Join(e.Home, ".secrets", "fleet.env")
		os.MkdirAll(filepath.Dir(sec), 0o700)
		if err := os.WriteFile(sec, []byte("# KEY=value lines. FAM copies them to every machine.\n"), 0o600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(e.Fixture, "agents.pkl"), []byte("amends \"@fam/Agents.pkl\"\n\nagents {\n  [\"claude\"] {}\n  [\"codex\"] {}\n}\n"), 0o644); err != nil {
			return nil, err
		}
		cfg := filepath.Join(e.Fixture, "config.pkl")
		b, _ := os.ReadFile(cfg)
		s := strings.Replace(string(b), "// secrets {\n//   delivery = \"copy-file\"\n//   file = \"~/.config/fam/secrets.env\"\n// }", "secrets {\n  file = \"~/.secrets/fleet.env\"\n}", 1)
		if err := os.WriteFile(cfg, []byte(s), 0o644); err != nil {
			return nil, err
		}
	}
	p, err := e.fam(ctx, root, "internal", "pkl-path")
	if err != nil {
		return nil, fmt.Errorf("fam internal pkl-path: %v: %s", err, p)
	}
	lines := strings.Split(strings.TrimSpace(p), "\n")
	e.Pkl = lines[len(lines)-1]
	if out, _ := e.fam(ctx, e.Fixture, "check"); !strings.Contains(out, "unknown command") {
		e.HasCheck = true
	}
	return e, nil
}

const machinesFixture = `amends "@fam/Machines.pkl"

machines {
  ["cockpit"] {
    cockpit = true
  }
  ["worker1"] {
    tags { "worker" }
    address = "worker1.example"
    user = "me"
  }
}
`

func allGuides() []string {
	out := []string{"none", "embedded"}
	m, _ := filepath.Glob(filepath.Join(guidesDir, "*.md"))
	sort.Strings(m)
	for _, p := range m {
		out = append(out, strings.TrimSuffix(filepath.Base(p), ".md"))
	}
	return out
}

// loadGuide returns the text of a guide. "none" is no guide at all, and "embedded" is the
// guide that the fam binary writes.
func loadGuide(e *Env, name string) (string, error) {
	switch name {
	case "none":
		return "", nil
	case "embedded":
		b, err := os.ReadFile(filepath.Join(e.Fixture, "AGENTS.md"))
		if err != nil {
			return "", fmt.Errorf("the fam binary wrote no AGENTS.md: %w", err)
		}
		s := string(b)
		i, j := strings.Index(s, beginMark), strings.Index(s, endMark)
		if i < 0 || j < i {
			return strings.TrimSpace(s), nil
		}
		return strings.TrimSpace(s[i+len(beginMark) : j]), nil
	}
	b, err := os.ReadFile(filepath.Join(guidesDir, name+".md"))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(strings.TrimSpace(string(b)), "{{version}}", e.Version), nil
}

func putGuide(project, guide string) error {
	agents, claude := filepath.Join(project, "AGENTS.md"), filepath.Join(project, "CLAUDE.md")
	if guide == "" {
		os.Remove(agents)
		os.Remove(claude)
		return nil
	}
	if err := os.WriteFile(agents, []byte(beginMark+"\n"+guide+"\n"+endMark+"\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(claude, []byte("@AGENTS.md\n"), 0o644)
}

func sh(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	return sh(ctx, dir, "git", append([]string{"-c", "user.name=bench", "-c", "user.email=bench@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
}

// writeWrappers puts fam and pkl wrappers in bin. The fam wrapper logs each call and blocks
// `fam apply` and `fam update`.
func writeWrappers(e *Env, bin, log string) error {
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	exports := fmt.Sprintf("export HOME=%q XDG_CACHE_HOME=%q FAM_SSH_CONFIG=%q\n", e.Home, filepath.Join(e.Home, ".cache"), e.SSHConfig)
	fam := "#!/bin/sh\n" +
		fmt.Sprintf("printf '%%s\\n' \"$*\" >> %q\n", log) +
		"case \" $* \" in *\" --help \"*|*\" -h \"*) ;; *) for a in \"$@\"; do case \"$a\" in apply|update) echo \"fam: '$a' is not available in this environment\" >&2; exit 1;; esac; done;; esac\n" +
		exports + fmt.Sprintf("exec %q \"$@\"\n", e.Bin)
	pkl := "#!/bin/sh\n" + exports + fmt.Sprintf("exec %q \"$@\"\n", e.Pkl)
	if err := os.WriteFile(filepath.Join(bin, "fam"), []byte(fam), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(bin, "pkl"), []byte(pkl), 0o755)
}

func runOne(ctx context.Context, e *Env, a Agent, guideName, guide string, t *Task, rep int) (res Result) {
	res = Result{Agent: a.Name, Guide: guideName, GuideBytes: len(guide), Task: t.Name, Rep: rep, Checks: map[string]bool{}}
	dir, err := os.MkdirTemp(filepath.Join(e.Root, "runs"), fmt.Sprintf("%s-%s-%s-%d-attempt-", a.Name, guideName, t.Name, rep))
	if err != nil {
		res.Error = err.Error()
		return
	}
	res.Dir = dir
	project := filepath.Join(dir, "fleet")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		res.Error = err.Error()
		return
	}
	if out, err := sh(ctx, dir, "cp", "-a", e.Fixture, project); err != nil {
		res.Error = "copy: " + out
		return
	}
	if err := putGuide(project, guide); err != nil {
		res.Error = err.Error()
		return
	}
	if t.Setup != nil {
		if err := t.Setup(e, project); err != nil {
			res.Error = "setup: " + err.Error()
			return
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-qm", "fleet"}} {
		if out, err := git(ctx, project, args...); err != nil {
			res.Error = fmt.Sprintf("git: %v: %s", err, out)
			return
		}
	}
	log := filepath.Join(dir, "fam.log")
	if err := os.WriteFile(log, nil, 0o644); err != nil {
		res.Error = err.Error()
		return
	}
	bin := filepath.Join(dir, "bin")
	if err := writeWrappers(e, bin, log); err != nil {
		res.Error = err.Error()
		return
	}

	rctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	start := time.Now()
	ans, usage, err := a.Run(rctx, runSpec{Dir: dir, Project: project, Bin: bin, Home: e.Home, Prompt: t.Prompt})
	res.Seconds = time.Since(start).Seconds()
	res.Answer, res.Usage = ans, usage
	if err != nil {
		res.Error = err.Error()
		return
	}
	if b, err := os.ReadFile(log); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				res.FamCmds = append(res.FamCmds, l)
			}
		}
	}
	g := &Grader{Env: e, Project: project, Answer: ans, Cmds: res.FamCmds, ctx: ctx}
	applied := false
	for _, c := range res.FamCmds {
		fs := strings.Fields(c)
		help := false
		for _, f := range fs {
			help = help || f == "--help" || f == "-h"
		}
		for _, f := range fs {
			if (f == "apply" || f == "update") && !help {
				applied = true
			}
		}
	}
	res.Checks["no-apply"] = !applied
	if t.Valid {
		res.Checks["valid"] = g.Valid()
	}
	for k, v := range t.Grade(g) {
		res.Checks[k] = v
	}
	res.Notes = g.Notes
	res.Pass = res.Error == ""
	for _, v := range res.Checks {
		res.Pass = res.Pass && v
	}
	// Keep the diff of the agent for inspection.
	if d, err := git(ctx, project, "diff", "HEAD"); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "agent.diff"), []byte(d), 0o644); err != nil {
			res.Error = err.Error()
			res.Pass = false
		}
	}
	return
}

// Grader gives the task graders a view of the project after the run.
type Grader struct {
	Env     *Env
	Project string
	Answer  string
	Cmds    []string
	Notes   []string
	ctx     context.Context
	sum     *Summary
	sumErr  error
	loaded  bool
}

func (g *Grader) note(f string, a ...any) { g.Notes = append(g.Notes, fmt.Sprintf(f, a...)) }

// Valid is true when the project has no errors: `fam check` when the binary has it, otherwise
// the config evaluates.
func (g *Grader) Valid() bool {
	if g.Env.HasCheck {
		out, err := g.Env.fam(g.ctx, g.Project, "check")
		if err != nil {
			g.note("fam check: %s", lastLines(out, 6))
			return false
		}
		return true
	}
	_, err := g.Summary()
	return err == nil
}

func (g *Grader) File(name string) string {
	b, _ := os.ReadFile(filepath.Join(g.Project, name))
	return string(b)
}

// Changed lists the files that differ from the commit before the run.
func (g *Grader) Changed() []string {
	out, _ := git(g.ctx, g.Project, "status", "--porcelain", "--untracked-files=all")
	var files []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if len(l) > 3 {
			files = append(files, strings.TrimSpace(l[3:]))
		}
	}
	return files
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " / ")
}
