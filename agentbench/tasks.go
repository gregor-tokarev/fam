package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Task is one job for the agent and the grader of its result.
type Task struct {
	Name   string
	Prompt string
	// Valid adds the check that the project has no errors after the run.
	Valid bool
	Setup func(e *Env, project string) error
	Grade func(g *Grader) map[string]bool
}

var tasks = []*Task{
	{
		Name:   "add-global",
		Prompt: "Install Microsoft's Playwright CLI for coding agents on every machine of the fleet." + handsOff,
		Valid:  true,
		Grade: func(g *Grader) map[string]bool {
			s := g.mustSummary()
			return map[string]bool{
				"in-every-machine": s.has(s.Bundles, "playwright-cli", "present"),
				"uses-std":         strings.Contains(g.File("config.pkl"), "std.computer.playwrightCli()"),
			}
		},
	},
	{
		Name:   "role-only",
		Prompt: "The worker machines need Tailscale. Do not install it on the cockpit." + handsOff,
		Valid:  true,
		Grade: func(g *Grader) map[string]bool {
			s := g.mustSummary()
			return map[string]bool{
				"on-workers":     s.has(s.Tags["worker"], "tailscale", "present"),
				"not-everywhere": !s.has(s.Bundles, "tailscale", ""),
			}
		},
	},
	{
		Name:   "remove",
		Prompt: "Remove the hello bundle from all machines. FAM installed it on them already." + handsOff,
		Valid:  true,
		Grade: func(g *Grader) map[string]bool {
			s := g.mustSummary()
			return map[string]bool{"absent": s.has(s.Bundles, "hello", "absent")}
		},
	},
	{
		Name:   "add-machine",
		Prompt: "Add a worker machine named w2. Its address is 10.0.0.7 and the SSH user is dev." + handsOff,
		Valid:  true,
		Grade: func(g *Grader) map[string]bool {
			s := g.mustSummary()
			m := s.Machines["w2"]
			return map[string]bool{"machine": m.hasTag("worker") && m.Address == "10.0.0.7" && m.User == "dev"}
		},
	},
	{
		Name:   "local-file",
		Prompt: "Every machine needs the file ~/.config/acme/settings.toml with the line `region = \"eu\"`." + handsOff,
		Valid:  true,
		Grade: func(g *Grader) map[string]bool {
			s := g.mustSummary()
			ok := false
			for _, b := range s.Bundles {
				for _, f := range b.Files {
					if b.State == "present" && f.Path == "~/.config/acme/settings.toml" && regexp.MustCompile(`region"?\s*[=:]\s*"eu"`).MatchString(f.Content) {
						ok = true
					}
				}
			}
			return map[string]bool{"file": ok}
		},
	},
	{
		Name:   "secret",
		Prompt: "Add the Vercel CLI to the worker machines. Which secret must I provide for it, and where do I put it?" + handsOff,
		Grade: func(g *Grader) map[string]bool {
			s, err := g.Summary()
			return map[string]bool{
				"evaluates":   err == nil,
				"on-workers":  err == nil && s.has(s.Tags["worker"], "vercel", "present"),
				"names-token": strings.Contains(g.Answer, "VERCEL_TOKEN"),
				"names-file":  strings.Contains(g.Answer, "fleet.env"),
			}
		},
	},
	{
		Name:   "fix-broken",
		Prompt: "This FAM project has errors. Find and fix them, and keep what the config means.",
		Valid:  true,
		Setup: func(e *Env, p string) error {
			if err := replaceIn(filepath.Join(p, "config.pkl"), "// std.computer.agentBrowser()", "std.computer.agentBrowse()"); err != nil {
				return err
			}
			return replaceIn(filepath.Join(p, "machines.pkl"), `tags { "worker" }`, `tags { "wroker" }`)
		},
		Grade: func(g *Grader) map[string]bool {
			s := g.mustSummary()
			return map[string]bool{
				"browser": s.has(s.Bundles, "agent-browser", "present"),
				"tag":     s.Machines["worker1"].hasTag("worker") && !s.Machines["worker1"].hasTag("wroker"),
			}
		},
	},
	{
		Name:   "editor",
		Prompt: "Zed underlines std.computer.agentBrowser() in config.pkl in red, but the code looks right to me. Fix the cause.",
		Setup: func(e *Env, p string) error {
			if err := replaceIn(filepath.Join(p, "config.pkl"), "// std.computer.agentBrowser()", "std.computer.agentBrowser()"); err != nil {
				return err
			}
			return replaceIn(filepath.Join(p, ".pkl-lsp", "projects.json"), e.Version, "0.12.0")
		},
		Grade: func(g *Grader) map[string]bool {
			st := g.File(".pkl-lsp/projects.json")
			return map[string]bool{
				"state-current": strings.Contains(st, "v"+g.Env.Version) && !strings.Contains(st, "0.12.0"),
				"says-restart":  regexp.MustCompile(`(?i)restart`).MatchString(g.Answer),
				"code-kept":     strings.Contains(g.File("config.pkl"), "\n  std.computer.agentBrowser()"),
			}
		},
	},
	{
		Name:   "discover",
		Prompt: "Which bundles of the FAM standard library let agents control a web browser? List them. Do not change any file.",
		Grade: func(g *Grader) map[string]bool {
			n := 0
			for _, f := range []string{"agentBrowser", "playwrightCli", "browserUse", "browserbaseBrowse", "steel"} {
				if strings.Contains(g.Answer, f) {
					n++
				}
			}
			ch := g.Changed()
			if len(ch) > 0 {
				g.note("changed: %v", ch)
			}
			return map[string]bool{"lists-4": n >= 4, "no-change": len(ch) == 0}
		},
	},
	{
		Name:   "machine-only",
		Prompt: "Install portless only on worker1. Other workers that I add later must not get it." + handsOff,
		Valid:  true,
		Grade: func(g *Grader) map[string]bool {
			s := g.mustSummary()
			return map[string]bool{
				"on-worker1":   s.has(s.Machines["worker1"].Bundles, "portless", "present"),
				"not-on-roles": !s.has(s.Tags["worker"], "portless", "") && !s.has(s.Bundles, "portless", ""),
			}
		},
	},
	{
		Name:   "instructions",
		Prompt: "Every coding agent on every machine must follow this rule: \"Run the tests before each commit.\" Add it." + handsOff,
		Grade: func(g *Grader) map[string]bool {
			before, err := git(g.ctx, g.Project, "show", "HEAD:AGENTS.md")
			if err != nil {
				before = ""
			}
			return map[string]bool{
				"fleet-instructions": strings.Contains(strings.ToLower(g.File("instructions/AGENTS.md")), "tests before"),
				"guide-untouched":    g.File("AGENTS.md") == before,
			}
		},
	},
	{
		Name:   "agent-settings",
		Prompt: "Turn off the auto memory of Claude Code on all machines." + handsOff,
		Valid:  true,
		Grade: func(g *Grader) map[string]bool {
			s := g.mustSummary()
			return map[string]bool{"setting": regexp.MustCompile(`"autoMemoryEnabled":\s*false`).MatchString(s.Agents["claude"])}
		},
	},
	{
		Name:   "env-var",
		Prompt: "Set EDITOR=nvim in every shell on every machine." + handsOff,
		Valid:  true,
		Grade: func(g *Grader) map[string]bool {
			s := g.mustSummary()
			ok := false
			for _, b := range s.Bundles {
				if b.State == "present" && b.Env["EDITOR"] == "nvim" {
					ok = true
				}
			}
			return map[string]bool{"env": ok}
		},
	},
	{
		Name:   "skill",
		Prompt: "Give every agent on every machine a skill named release-checklist. It tells the agent to update CHANGELOG.md and to tag the release." + handsOff,
		Valid:  true,
		Grade: func(g *Grader) map[string]bool {
			// A skill folder that a bundle installs with an AgentSkill task.
			sk := ""
			filepath.WalkDir(g.Project, func(p string, d os.DirEntry, err error) error {
				if err == nil && d.Name() == "SKILL.md" {
					b, _ := os.ReadFile(p)
					if strings.Contains(string(b), "name: release-checklist") {
						sk = string(b)
					}
				}
				return nil
			})
			s := g.mustSummary()
			task := false
			for _, b := range s.Bundles {
				for _, t := range b.Tasks {
					if b.State == "present" && t.Kind == "AgentSkill" {
						task = true
					}
				}
			}
			return map[string]bool{
				"skill-file":  strings.Contains(sk, "CHANGELOG"),
				"description": strings.Contains(sk, "description:"),
				"bundle-task": task,
			}
		},
	},
	{
		Name:   "machine-exception",
		Prompt: "worker1 must not have agent-browser any more. All other machines keep it." + handsOff,
		Valid:  true,
		Setup: func(e *Env, p string) error {
			return replaceIn(filepath.Join(p, "config.pkl"), "// std.computer.agentBrowser()", "std.computer.agentBrowser()")
		},
		Grade: func(g *Grader) map[string]bool {
			s := g.mustSummary()
			return map[string]bool{
				"absent-on-worker1": s.has(s.Machines["worker1"].Bundles, "agent-browser", "absent"),
				"kept-for-others":   s.has(s.Bundles, "agent-browser", "present"),
			}
		},
	},
	{
		Name:   "github-skill",
		Prompt: "Install the skill in the folder skills/frontend-design of the GitHub repository anthropics/skills on every machine." + handsOff,
		Valid:  true,
		Grade: func(g *Grader) map[string]bool {
			s := g.mustSummary()
			ok := false
			for _, b := range s.Bundles {
				for _, t := range b.Tasks {
					if b.State == "present" && t.Kind == "AgentSkill" && strings.Contains(t.JSON, "github.com/anthropics/skills") && strings.Contains(t.JSON, "skills/frontend-design") {
						ok = true
					}
				}
			}
			return map[string]bool{"github-skill": ok}
		},
	},
	{
		Name:   "package",
		Prompt: "Install tmux on every machine with the package manager of the system." + handsOff,
		Valid:  true,
		Grade: func(g *Grader) map[string]bool {
			s := g.mustSummary()
			ok := false
			for _, b := range s.Bundles {
				for _, t := range b.Tasks {
					if b.State == "present" && t.Kind == "Package" && strings.Contains(t.JSON, `"tmux"`) && regexp.MustCompile(`"manager":\s*"(system|apt|dnf|pacman|apk|brew)"`).MatchString(t.JSON) {
						ok = true
					}
				}
			}
			return map[string]bool{"package-task": ok}
		},
	},
}

var taskByName = func() map[string]*Task {
	m := map[string]*Task{}
	for _, t := range tasks {
		m[t.Name] = t
	}
	return m
}()

func taskNames() []string {
	var out []string
	for _, t := range tasks {
		out = append(out, t.Name)
	}
	return out
}

func replaceIn(path, old, new string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !strings.Contains(string(b), old) {
		return fmt.Errorf("%s has no %q", path, old)
	}
	return os.WriteFile(path, []byte(strings.Replace(string(b), old, new, 1)), 0o644)
}

// Summary is the evaluated config, reduced to what the graders need.
type Summary struct {
	Bundles  []SBundle            `json:"bundles"`
	Tags     map[string][]SBundle `json:"tags"`
	Machines map[string]SMachine  `json:"machines"`
	Agents   map[string]string    `json:"agents"`
}

type SMachine struct {
	Cockpit bool      `json:"cockpit"`
	Tags    []string  `json:"tags"`
	Address string    `json:"address"`
	User    string    `json:"user"`
	Bundles []SBundle `json:"bundles"`
}

func (m SMachine) hasTag(tag string) bool {
	for _, t := range m.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

type SBundle struct {
	Name  string            `json:"name"`
	State string            `json:"state"`
	Env   map[string]string `json:"env"`
	Tasks []struct {
		Kind string `json:"kind"`
		JSON string `json:"json"`
	} `json:"tasks"`
	Files []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"files"`
}

func (s *Summary) has(bs []SBundle, name, state string) bool {
	if s == nil {
		return false
	}
	for _, b := range bs {
		if b.Name == name && (state == "" || b.State == state) {
			return true
		}
	}
	return false
}

const summaryPkl = `import "config.pkl" as c
local function files(b) = b.tasks.toList()
  .filter((t) -> List("File", "Merge").contains(t.getClass().simpleName))
  .map((t) -> new Dynamic {
    path = t.path
    content = if (t.getClass().simpleName == "File") t.content ?? "" else new JsonRenderer {}.renderValue(t.values)
  })
local function tasks(b) = b.tasks.toList().map((t) -> new Dynamic {
  kind = t.getClass().simpleName
  json = new JsonRenderer { omitNullProperties = true }.renderValue(t)
})
local function norm(e) =
  if (e.getClass().simpleName == "BundleRef") new Dynamic { name = e.bundle.name; state = e.state; files = files(e.bundle); env = e.bundle.env.toMap(); tasks = tasks(e.bundle) }
  else new Dynamic { name = e.name; state = "present"; files = files(e); env = e.env.toMap(); tasks = tasks(e) }
bundles = c.bundles.toList().map((e) -> norm(e))
tags = c.tags.toMap().mapValues((_, r) -> r.bundles.toList().map((e) -> norm(e)))
machines = c.machines.toMap().mapValues((_, m) -> new Dynamic { cockpit = m.cockpit; tags = m.tags.toList(); address = m.address ?? ""; user = m.user ?? ""; bundles = m.bundles.toList().map((e) -> norm(e)) })
agents = c.agents.toMap().mapValues((_, a) -> new JsonRenderer {}.renderValue(a.settings))
output { renderer = new JsonRenderer {} }
`

// Summary evaluates the config of the project once.
func (g *Grader) Summary() (*Summary, error) {
	if g.loaded {
		return g.sum, g.sumErr
	}
	g.loaded = true
	tmp := filepath.Join(g.Project, ".agentbench-summary.pkl")
	if err := os.WriteFile(tmp, []byte(summaryPkl), 0o644); err != nil {
		g.sumErr = err
		return nil, err
	}
	defer os.Remove(tmp)
	args := []string{"eval", "--project-dir", g.Project}
	for k, v := range map[string]string{"os": "linux", "arch": "amd64", "cpus": "4", "distro": "ubuntu", "distroVersion": "26.04", "home": "/home/me", "hostname": "worker1", "init": "systemd", "libc": "glibc", "machine": "worker1", "cockpit": "false", "tags": "worker", "shell": "/bin/bash", "user": "me"} {
		args = append(args, "-p", "fam."+k+"="+v)
	}
	cmd := exec.CommandContext(g.ctx, g.Env.Pkl, append(args, tmp)...)
	cmd.Dir = g.Project
	cmd.Env = g.Env.famEnv()
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok {
			msg = firstLines(string(ee.Stderr), 6)
		}
		g.note("eval: %s", msg)
		g.sumErr = fmt.Errorf("eval: %s", msg)
		return nil, g.sumErr
	}
	var s Summary
	if err := json.Unmarshal(out, &s); err != nil {
		g.sumErr = err
		return nil, err
	}
	g.sum = &s
	return &s, nil
}

func (g *Grader) mustSummary() *Summary {
	s, _ := g.Summary()
	if s == nil {
		return &Summary{}
	}
	return s
}

func firstLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[:n]
	}
	return strings.Join(ls, " / ")
}

// handsOff ends the prompts of tasks that change the project.
const handsOff = " Change the project files. Do not run `fam apply`: I run it myself."
