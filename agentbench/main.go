// Command agentbench measures how well coding agents work in a FAM project with each
// version of the project guide (the FAM part of AGENTS.md).
//
//	go run ./agentbench run [-agents haiku,sol] [-guides none,g6-lean] [-tasks add-global] [-repeat 2] [-jobs 4]
//	go run ./agentbench report <results.jsonl>...
//
// Each run copies a fresh project, puts one guide into AGENTS.md, starts one agent with one
// task prompt, and grades the files, the fam commands, and the answer of the agent.
// `fam apply` and `fam update` are blocked. fam runs with its own HOME, so the runs do not
// change the Pkl caches or the machine.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agentbench run|grade|report ...")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	switch os.Args[1] {
	case "run":
		if err := run(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "agentbench:", err)
			os.Exit(1)
		}
	case "grade":
		if err := grade(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "agentbench:", err)
			os.Exit(1)
		}
	case "report":
		if err := report(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "agentbench:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: agentbench run|grade|report ...")
		os.Exit(2)
	}
}

func grade(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("grade", flag.ContinueOnError)
	out := fs.String("out", "", "output directory of the run")
	famBin := fs.String("fam", "./fam", "fam binary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || fs.NArg() != 2 {
		return fmt.Errorf("usage: agentbench grade -out <root> <task> <run dir>")
	}
	t, ok := taskByName[fs.Arg(0)]
	if !ok {
		return fmt.Errorf("unknown task %q", fs.Arg(0))
	}
	project, err := filepath.Abs(filepath.Join(fs.Arg(1), "fleet"))
	if err != nil {
		return err
	}
	if st, err := os.Stat(project); err != nil {
		return err
	} else if !st.IsDir() {
		return fmt.Errorf("not a directory: %s", project)
	}
	bin, err := filepath.Abs(*famBin)
	if err != nil {
		return err
	}
	e, err := prepare(ctx, *out, bin)
	if err != nil {
		return err
	}
	records, err := readResults(filepath.Join(e.Root, "results.jsonl"))
	if err != nil {
		return err
	}
	g := &Grader{Env: e, Project: project, ctx: ctx}
	found := false
	for _, r := range records {
		if filepath.Clean(r.Dir) == filepath.Dir(project) && r.Task == t.Name {
			g.Answer, g.Cmds = r.Answer, r.FamCmds
			found = true
		}
	}
	if !found {
		return fmt.Errorf("no matching run in %s/results.jsonl", e.Root)
	}
	checks := t.Grade(g)
	if t.Valid {
		checks["valid"] = g.Valid()
	}
	fmt.Println(checks, g.Notes)
	for _, ok := range checks {
		if !ok {
			return fmt.Errorf("grade failed")
		}
	}
	return nil
}

// readResults rejects partial records rather than overwriting existing data.
func readResults(path string) ([]Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rs []Result
	for i, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r Result
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		rs = append(rs, r)
	}
	return rs, nil
}

// Result is one line of results.jsonl.
type Result struct {
	Agent      string            `json:"agent"`
	Guide      string            `json:"guide"`
	GuideBytes int               `json:"guideBytes"`
	Task       string            `json:"task"`
	Rep        int               `json:"rep"`
	Pass       bool              `json:"pass"`
	Checks     map[string]bool   `json:"checks"`
	Notes      []string          `json:"notes,omitempty"`
	Usage      Usage             `json:"usage"`
	Seconds    float64           `json:"seconds"`
	FamCmds    []string          `json:"famCmds"`
	Answer     string            `json:"answer"`
	Error      string            `json:"error,omitempty"`
	Dir        string            `json:"dir"`
	Extra      map[string]string `json:"extra,omitempty"`
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	agentsF := fs.String("agents", "haiku,sol", "agents: "+strings.Join(agentNames(), ","))
	guidesF := fs.String("guides", "", "guides in agentbench/guides (without .md), plus none and embedded (default: all)")
	tasksF := fs.String("tasks", "", "tasks (default: all): "+strings.Join(taskNames(), ","))
	repeat := fs.Int("repeat", 1, "runs of each combination")
	jobs := fs.Int("jobs", 4, "parallel runs")
	famBin := fs.String("fam", "./fam", "fam binary to test")
	out := fs.String("out", "", "output directory (default ~/.cache/fam-agentbench/<time>)")
	retryErrors := fs.Bool("retry-errors", false, "resume this output directory, retry errors and missing runs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *jobs < 1 || *repeat < 1 {
		return fmt.Errorf("-jobs and -repeat must be positive")
	}
	if *retryErrors && *out == "" {
		return fmt.Errorf("-retry-errors requires -out")
	}

	bin, err := filepath.Abs(*famBin)
	if err != nil {
		return err
	}
	if *out == "" {
		c, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		*out = filepath.Join(c, "fam-agentbench", time.Now().Format("20060102-150405"))
	}
	root, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	env, err := prepare(ctx, root, bin)
	if err != nil {
		return err
	}

	guides := split(*guidesF)
	if len(guides) == 0 {
		guides = allGuides()
	}
	tasks := split(*tasksF)
	if len(tasks) == 0 {
		tasks = taskNames()
	}
	agents := split(*agentsF)
	for _, a := range agents {
		if _, ok := agentByName[a]; !ok {
			return fmt.Errorf("unknown agent %q", a)
		}
	}
	for _, t := range tasks {
		if _, ok := taskByName[t]; !ok {
			return fmt.Errorf("unknown task %q", t)
		}
	}
	guideText := map[string]string{}
	for _, g := range guides {
		s, err := loadGuide(env, g)
		if err != nil {
			return err
		}
		guideText[g] = s
	}

	type job struct {
		agent, guide, task string
		rep                int
	}
	var all []job
	for r := 1; r <= *repeat; r++ {
		for _, t := range tasks {
			for _, g := range guides {
				for _, a := range agents {
					all = append(all, job{a, g, t, r})
				}
			}
		}
	}
	resPath := filepath.Join(root, "results.jsonl")
	if *retryErrors {
		previous, err := readResults(resPath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		completed := map[job]bool{}
		for _, r := range previous {
			if r.Error == "" {
				completed[job{r.Agent, r.Guide, r.Task, r.Rep}] = true
			}
		}
		pending := all[:0]
		for _, j := range all {
			if !completed[j] {
				pending = append(pending, j)
			}
		}
		all = pending
	} else if st, err := os.Stat(resPath); err == nil && st.Size() > 0 {
		return fmt.Errorf("results already exist: use -retry-errors to resume %s", resPath)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(resPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if b, err := os.ReadFile(resPath); err != nil {
		return err
	} else if len(b) > 0 && b[len(b)-1] != '\n' {
		if _, err := f.Write([]byte("\n")); err != nil {
			return err
		}
	}
	fmt.Printf("%d runs, results in %s\n", len(all), resPath)

	var mu sync.Mutex
	ch := make(chan job)
	var wg sync.WaitGroup
	done := 0
	var writeErr error
	for i := 0; i < *jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				if ctx.Err() != nil {
					continue
				}
				res := runOne(ctx, env, agentByName[j.agent], j.guide, guideText[j.guide], taskByName[j.task], j.rep)
				b, err := json.Marshal(res)
				mu.Lock()
				if err == nil {
					_, err = f.Write(append(b, '\n'))
				}
				if err != nil && writeErr == nil {
					writeErr = err
				}
				done++
				mark := "FAIL"
				if res.Error != "" {
					mark = "ERROR"
				}
				if res.Pass {
					mark = "pass"
				}
				fmt.Printf("[%d/%d] %s %-6s %-10s %-14s %3.0fs %s\n", done, len(all), mark, j.agent, j.guide, j.task, res.Seconds, failed(res))
				mu.Unlock()
			}
		}()
	}
	for _, j := range all {
		ch <- j
	}
	close(ch)
	wg.Wait()
	if writeErr != nil {
		return writeErr
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return report([]string{resPath})
}

func failed(r Result) string {
	if r.Error != "" {
		return "error: " + r.Error
	}
	var bad []string
	for k, v := range r.Checks {
		if !v {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	return strings.Join(bad, ",")
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func report(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("usage: agentbench report <results.jsonl>...")
	}
	var rs []Result
	for _, p := range paths {
		r, err := readResults(p)
		if err != nil {
			return err
		}
		// Latest attempt per cell within each file. Original attempts remain in JSONL.
		latest := map[string]Result{}
		for _, v := range r {
			latest[fmt.Sprintf("%s/%s/%s/%d", v.Agent, v.Guide, v.Task, v.Rep)] = v
		}
		for _, v := range latest {
			rs = append(rs, v)
		}
	}
	type key struct{ agent, guide string }
	type agg struct {
		n, pass, bytes, errors int
		in, out, cached        int64
		secs, cost             float64
		famCmds                int
		taskPass, taskTotal    map[string]int
	}
	m := map[key]*agg{}
	taskSet := map[string]bool{}
	for _, r := range rs {
		k := key{r.Agent, r.Guide}
		a := m[k]
		if a == nil {
			a = &agg{taskPass: map[string]int{}, taskTotal: map[string]int{}}
			m[k] = a
		}
		a.bytes = r.GuideBytes
		if r.Error != "" {
			a.errors++
			continue
		}
		a.n++
		a.bytes = r.GuideBytes
		if r.Pass {
			a.pass++
			a.taskPass[r.Task]++
		}
		a.taskTotal[r.Task]++
		a.in += r.Usage.Input
		a.cached += r.Usage.Cached
		a.out += r.Usage.Output
		a.cost += r.Usage.CostUSD
		a.secs += r.Seconds
		a.famCmds += len(r.FamCmds)
		taskSet[r.Task] = true
	}
	keys := make([]key, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].agent != keys[j].agent {
			return keys[i].agent < keys[j].agent
		}
		if m[keys[i]].bytes == m[keys[j]].bytes {
			return keys[i].guide < keys[j].guide
		}
		return m[keys[i]].bytes < m[keys[j]].bytes
	})
	var tasks []string
	for t := range taskSet {
		tasks = append(tasks, t)
	}
	sort.Strings(tasks)
	fmt.Printf("\n| agent | guide | guide tokens≈ | pass | scored runs | errors | rate | in tok/run | out tok/run | fam cmds/run | s/run | $/run |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, k := range keys {
		a := m[k]
		n := float64(a.n)
		if n == 0 {
			n = 1
		}
		fmt.Printf("| %s | %s | %d | %d | %d | %d | %.0f%% | %.0f | %.0f | %.1f | %.0f | %.3f |\n", k.agent, k.guide, a.bytes/4, a.pass, a.n, a.errors, 100*float64(a.pass)/n, float64(a.in+a.cached)/n, float64(a.out)/n, float64(a.famCmds)/n, a.secs/n, a.cost/n)
	}
	fmt.Printf("\n| agent | guide |")
	for _, t := range tasks {
		fmt.Printf(" %s |", t)
	}
	fmt.Printf("\n|---|---|%s\n", strings.Repeat("---|", len(tasks)))
	for _, k := range keys {
		a := m[k]
		fmt.Printf("| %s | %s |", k.agent, k.guide)
		for _, t := range tasks {
			if a.taskTotal[t] == 0 {
				fmt.Printf(" |")
				continue
			}
			fmt.Printf(" %d/%d |", a.taskPass[t], a.taskTotal[t])
		}
		fmt.Println()
	}
	return nil
}
