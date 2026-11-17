package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Usage is the token use of one run.
type Usage struct {
	Input   int64   `json:"input"`
	Cached  int64   `json:"cached"`
	Output  int64   `json:"output"`
	CostUSD float64 `json:"costUSD"`
	Turns   int     `json:"turns"`
}

type runSpec struct {
	Dir, Project, Bin, Home, Prompt string
}

// Agent starts one headless coding agent in the project.
type Agent struct {
	Name string
	Run  func(ctx context.Context, s runSpec) (string, Usage, error)
}

var agentByName = map[string]Agent{
	"haiku":  {Name: "haiku", Run: claudeRun("claude-haiku-5-5")},
	"sonnet": {Name: "sonnet", Run: claudeRun("claude-sonnet-5-5")},
	"sol":    {Name: "sol", Run: codexRun("gpt-6.1-sol", "low")},
	"solmed": {Name: "solmed", Run: codexRun("gpt-6.1-sol", "medium")},
}

func agentNames() []string {
	var out []string
	for k := range agentByName {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func agentEnv(s runSpec) []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PATH=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PATH="+s.Bin+":"+os.Getenv("PATH"))
}

// Read-only shell commands, fam, pkl, and the file tools. Everything else is denied.
var claudeTools = []string{
	"Read", "Edit", "Write", "Glob", "Grep",
	"Bash(fam *)", "Bash(fam)", "Bash(pkl *)", "Bash(ls *)", "Bash(ls)", "Bash(cat *)", "Bash(grep *)",
	"Bash(find *)", "Bash(head *)", "Bash(tail *)", "Bash(git diff*)", "Bash(git status*)", "Bash(git log*)",
}

func claudeRun(model string) func(context.Context, runSpec) (string, Usage, error) {
	return func(ctx context.Context, s runSpec) (string, Usage, error) {
		args := []string{"-p", s.Prompt, "--model", model, "--output-format", "json",
			"--setting-sources", "project", "--strict-mcp-config",
			"--permission-mode", "acceptEdits", "--disallowedTools", "WebFetch,WebSearch,Agent,Task"}
		args = append(args, "--allowedTools", strings.Join(claudeTools, ","))
		cmd := exec.CommandContext(ctx, "claude", args...)
		cmd.Dir = s.Project
		cmd.Env = agentEnv(s)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if werr := os.WriteFile(filepath.Join(s.Dir, "agent.stderr"), stderr.Bytes(), 0o644); werr != nil {
			return "", Usage{}, werr
		}
		if werr := os.WriteFile(filepath.Join(s.Dir, "agent.json"), out, 0o644); werr != nil {
			return "", Usage{}, werr
		}
		var r struct {
			Result  string  `json:"result"`
			IsError bool    `json:"is_error"`
			Cost    float64 `json:"total_cost_usd"`
			Turns   int     `json:"num_turns"`
			Usage   struct {
				Input   int64 `json:"input_tokens"`
				CRead   int64 `json:"cache_read_input_tokens"`
				CCreate int64 `json:"cache_creation_input_tokens"`
				Output  int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if jerr := json.Unmarshal(out, &r); jerr != nil {
			return "", Usage{}, fmt.Errorf("claude JSON: %v (process: %v) %s", jerr, err, lastLines(stderr.String(), 3))
		}
		u := Usage{Input: r.Usage.Input + r.Usage.CCreate, Cached: r.Usage.CRead, Output: r.Usage.Output, CostUSD: r.Cost, Turns: r.Turns}
		if err != nil {
			return r.Result, u, fmt.Errorf("claude: %v %s %s", err, lastLines(stderr.String(), 3), lastLines(r.Result, 2))
		}
		if r.IsError {
			return r.Result, u, fmt.Errorf("claude error: %s", lastLines(r.Result, 2))
		}
		return r.Result, u, nil
	}
}

func codexRun(model, effort string) func(context.Context, runSpec) (string, Usage, error) {
	return func(ctx context.Context, s runSpec) (string, Usage, error) {
		last := filepath.Join(s.Dir, "answer.txt")
		args := []string{"exec", "--ephemeral", "--skip-git-repo-check", "-m", model,
			"-c", "model_reasoning_effort=" + effort, "-s", "workspace-write",
			"--add-dir", s.Home, "--add-dir", s.Dir, "-C", s.Project, "--json", "-o", last, s.Prompt}
		cmd := exec.CommandContext(ctx, "codex", args...)
		cmd.Dir = s.Project
		cmd.Env = agentEnv(s)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if werr := os.WriteFile(filepath.Join(s.Dir, "agent.stderr"), stderr.Bytes(), 0o644); werr != nil {
			return "", Usage{}, werr
		}
		if werr := os.WriteFile(filepath.Join(s.Dir, "agent.jsonl"), out, 0o644); werr != nil {
			return "", Usage{}, werr
		}
		var u Usage
		var eventError string
		sc := bufio.NewScanner(bytes.NewReader(out))
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			var ev struct {
				Type    string `json:"type"`
				Message string `json:"message"`
				Error   struct {
					Message string `json:"message"`
				} `json:"error"`
				Usage struct {
					Input  int64 `json:"input_tokens"`
					Cached int64 `json:"cached_input_tokens"`
					Output int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			if jerr := json.Unmarshal(sc.Bytes(), &ev); jerr != nil {
				return "", u, fmt.Errorf("codex event JSON: %w", jerr)
			}
			if ev.Type == "turn.failed" || ev.Type == "error" {
				eventError = string(sc.Bytes())
			}
			if ev.Type == "turn.completed" {
				u.Input += ev.Usage.Input - ev.Usage.Cached
				u.Cached += ev.Usage.Cached
				u.Output += ev.Usage.Output
				u.Turns++
			}
		}
		if serr := sc.Err(); serr != nil {
			return "", u, serr
		}
		ans, readErr := os.ReadFile(last)
		if err != nil {
			return string(ans), u, fmt.Errorf("codex: %v %s", err, lastLines(stderr.String(), 3))
		}
		if eventError != "" {
			return string(ans), u, fmt.Errorf("codex: %s", eventError)
		}
		if u.Turns == 0 {
			return string(ans), u, fmt.Errorf("codex: no completed turn")
		}
		if readErr != nil {
			return "", u, readErr
		}
		return string(ans), u, nil
	}
}
