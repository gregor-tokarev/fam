// Command bench benchmarks FAM commands comparing a baseline binary
// (typically origin/main) against a candidate binary (typically the PR head)
// and emits a Markdown table with millisecond timings.
//
//	go run ./bench
//	go run ./bench -base-bin /tmp/fam-main -head-bin /tmp/fam-pr -markdown /tmp/bench.md
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const commentMarker = "<!-- fam-command-benchmark -->"

type benchCase struct {
	name         string
	network      bool
	needsProject bool
	needsSteady  bool
	// prepare runs before the timer starts and returns (home, workDir, args, cleanup, error).
	prepare func(sb *sandbox, iter int) (home, dir string, args []string, cleanup func(), err error)
}

type sideResult struct {
	MedianMS   float64   `json:"medianMs,omitempty"`
	SamplesMS  []float64 `json:"samplesMs,omitempty"`
	NotPresent bool      `json:"notPresent,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type caseResult struct {
	Command string     `json:"command"`
	Base    sideResult `json:"base"`
	Head    sideResult `json:"head"`
}

type report struct {
	BaseLabel string       `json:"baseLabel"`
	HeadLabel string       `json:"headLabel"`
	BaseSHA   string       `json:"baseSha,omitempty"`
	HeadSHA   string       `json:"headSha,omitempty"`
	Runs      int          `json:"runs"`
	Warmup    int          `json:"warmup"`
	Results   []caseResult `json:"results"`
}

func allCases() []benchCase {
	noop := func() {}
	return []benchCase{
		{
			name: "fam new",
			prepare: func(sb *sandbox, iter int) (string, string, []string, func(), error) {
				dir := filepath.Join(sb.work, fmt.Sprintf("new-%d", iter))
				_ = os.RemoveAll(dir)
				return sb.home, sb.root, []string{"new", "--base", dir}, func() { _ = os.RemoveAll(dir) }, nil
			},
		},
		{
			name:         "fam migrate",
			needsProject: true,
			prepare: func(sb *sandbox, iter int) (string, string, []string, func(), error) {
				dir := filepath.Join(sb.work, fmt.Sprintf("migrate-%d", iter))
				_ = os.RemoveAll(dir)
				if err := copyTree(sb.project, dir); err != nil {
					return "", "", nil, noop, err
				}
				return sb.home, dir, []string{"migrate", "--base", dir}, func() { _ = os.RemoveAll(dir) }, nil
			},
		},
		{
			name:         "fam check",
			needsProject: true,
			prepare: func(sb *sandbox, _ int) (string, string, []string, func(), error) {
				return sb.home, sb.project, []string{"check", "--base", sb.project}, noop, nil
			},
		},
		{
			name:         "fam doctor --offline",
			needsProject: true,
			prepare: func(sb *sandbox, _ int) (string, string, []string, func(), error) {
				return sb.home, sb.project, []string{"doctor", "--offline", "--base", sb.project}, noop, nil
			},
		},
		{
			name:         "fam plan",
			needsProject: true,
			needsSteady:  true,
			prepare: func(sb *sandbox, _ int) (string, string, []string, func(), error) {
				return sb.home, sb.project, []string{"plan", "--no-progress", "--base", sb.project}, noop, nil
			},
		},
		{
			name:         "fam verify",
			needsProject: true,
			needsSteady:  true,
			prepare: func(sb *sandbox, _ int) (string, string, []string, func(), error) {
				return sb.home, sb.project, []string{"verify", "--no-progress", "--base", sb.project}, noop, nil
			},
		},
		{
			name:    "fam update --check",
			network: true,
			prepare: func(sb *sandbox, _ int) (string, string, []string, func(), error) {
				return sb.home, sb.root, []string{"update", "--check"}, noop, nil
			},
		},
	}
}

func main() {
	baseBin := flag.String("base-bin", "", "path to baseline fam binary (default: build from -base-ref)")
	headBin := flag.String("head-bin", "", "path to candidate fam binary (default: build current checkout)")
	baseRef := flag.String("base-ref", "origin/main", "git ref for baseline when -base-bin is empty")
	baseLabel := flag.String("base-label", "origin/main", "column label for baseline")
	headLabel := flag.String("head-label", "PR", "column label for candidate")
	baseSHA := flag.String("base-sha", "", "short commit SHA for baseline (shown in header)")
	headSHA := flag.String("head-sha", "", "short commit SHA for candidate (shown in header)")
	runs := flag.Int("runs", 5, "measured iterations per command")
	warmup := flag.Int("warmup", 1, "warmup iterations per command")
	offline := flag.Bool("offline", false, "skip network commands such as fam update --check")
	filter := flag.String("filter", "", "run only commands whose name contains this substring")
	markdownOut := flag.String("markdown", "", "write the Markdown report to this file")
	jsonOut := flag.String("json", "", "write the JSON report to this file")
	flag.Parse()

	if *runs < 1 {
		fmt.Fprintln(os.Stderr, "bench: -runs must be >= 1")
		os.Exit(2)
	}
	if *warmup < 0 {
		fmt.Fprintln(os.Stderr, "bench: -warmup must be >= 0")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	tmpRoot, err := os.MkdirTemp("", "fam-bench-*")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(tmpRoot)

	repoDir, err := os.Getwd()
	if err != nil {
		fatal(err)
	}

	resolvedBaseBin := *baseBin
	resolvedBaseSHA := *baseSHA
	if resolvedBaseBin == "" {
		resolvedBaseBin = filepath.Join(tmpRoot, "fam-base")
		fmt.Fprintf(os.Stderr, "bench: building baseline from %s...\n", *baseRef)
		sha, err := buildFromRef(ctx, repoDir, *baseRef, filepath.Join(tmpRoot, "base-worktree"), resolvedBaseBin)
		if err != nil {
			fatal(err)
		}
		if resolvedBaseSHA == "" {
			resolvedBaseSHA = sha
		}
	} else {
		resolvedBaseBin, err = filepath.Abs(resolvedBaseBin)
		if err != nil {
			fatal(err)
		}
	}

	resolvedHeadBin := *headBin
	resolvedHeadSHA := *headSHA
	if resolvedHeadBin == "" {
		resolvedHeadBin = filepath.Join(tmpRoot, "fam-head")
		fmt.Fprintf(os.Stderr, "bench: building candidate from working tree...\n")
		if err := buildFromDir(ctx, repoDir, resolvedHeadBin); err != nil {
			fatal(err)
		}
		if resolvedHeadSHA == "" {
			if out, err := exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output(); err == nil {
				resolvedHeadSHA = strings.TrimSpace(string(out))
			}
		}
	} else {
		resolvedHeadBin, err = filepath.Abs(resolvedHeadBin)
		if err != nil {
			fatal(err)
		}
	}

	var hostPklDirs []string
	if uc, err := os.UserCacheDir(); err == nil {
		hostPklDirs = append(hostPklDirs, filepath.Join(uc, "fam", "pkl"))
	}

	fmt.Fprintf(os.Stderr, "bench: preparing isolated sandboxes...\n")
	baseSB, err := newSandbox(ctx, tmpRoot, "base", resolvedBaseBin, hostPklDirs)
	if err != nil {
		fatal(err)
	}
	// Reuse base's cached pkl binary for head if they share the same Pkl version.
	headSB, err := newSandbox(ctx, tmpRoot, "head", resolvedHeadBin, append([]string{baseSB.pklRoot()}, hostPklDirs...))
	if err != nil {
		fatal(err)
	}

	cases := selectCases(allCases(), *offline, *filter)
	if len(cases) == 0 {
		fatal(fmt.Errorf("no benchmark cases matched"))
	}

	rep := report{
		BaseLabel: *baseLabel,
		HeadLabel: *headLabel,
		BaseSHA:   resolvedBaseSHA,
		HeadSHA:   resolvedHeadSHA,
		Runs:      *runs,
		Warmup:    *warmup,
		Results:   make([]caseResult, 0, len(cases)),
	}

	anyError := false
	for _, bc := range cases {
		fmt.Fprintf(os.Stderr, "bench: measuring %s...\n", bc.name)
		res := measureCase(ctx, bc, baseSB, headSB, *warmup, *runs)
		rep.Results = append(rep.Results, res)
		if res.Base.Error != "" || res.Head.Error != "" {
			anyError = true
		}
	}

	md := renderMarkdown(rep)
	fmt.Print(md)

	if *markdownOut != "" {
		if err := os.WriteFile(*markdownOut, []byte(md), 0o644); err != nil {
			fatal(err)
		}
	}
	if *jsonOut != "" {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			fatal(err)
		}
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o644); err != nil {
			fatal(err)
		}
	}
	if anyError {
		os.Exit(1)
	}
}

func selectCases(cases []benchCase, offline bool, filter string) []benchCase {
	var out []benchCase
	for _, c := range cases {
		if offline && c.network {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(c.name), strings.ToLower(filter)) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func measureCase(ctx context.Context, bc benchCase, baseSB, headSB *sandbox, warmup, runs int) caseResult {
	res := caseResult{Command: bc.name}

	runOne := func(sb *sandbox, side *sideResult, iter int, record, allowNotPresent bool) {
		if side.NotPresent || side.Error != "" {
			return
		}
		if bc.needsProject && sb.projectErr != "" {
			side.Error = sb.projectErr
			return
		}
		if bc.needsSteady && sb.steadyErr != "" {
			side.Error = sb.steadyErr
			return
		}
		home, dir, args, cleanup, err := bc.prepare(sb, iter)
		if err != nil {
			side.Error = "prepare: " + err.Error()
			return
		}
		defer cleanup()

		dur, out, err := sb.timeRun(ctx, home, dir, args...)
		if err != nil {
			if allowNotPresent && isUnknownCommand(out) {
				side.NotPresent = true
				return
			}
			side.Error = compactErr(err, out)
			return
		}
		if record {
			side.SamplesMS = append(side.SamplesMS, float64(dur)/float64(time.Millisecond))
		}
	}

	for w := 0; w < warmup; w++ {
		iter := -(w + 1)
		runOne(baseSB, &res.Base, iter, false, true)
		runOne(headSB, &res.Head, iter, false, false)
	}

	for i := 0; i < runs; i++ {
		if i%2 == 0 {
			runOne(baseSB, &res.Base, i, true, true)
			runOne(headSB, &res.Head, i, true, false)
		} else {
			runOne(headSB, &res.Head, i, true, false)
			runOne(baseSB, &res.Base, i, true, true)
		}
	}

	if len(res.Base.SamplesMS) > 0 {
		res.Base.MedianMS = median(res.Base.SamplesMS)
	}
	if len(res.Head.SamplesMS) > 0 {
		res.Head.MedianMS = median(res.Head.SamplesMS)
	}
	return res
}

func isUnknownCommand(out string) bool {
	return strings.Contains(out, "unknown command") || strings.Contains(out, "unknown flag")
}

func compactErr(err error, out string) string {
	s := strings.TrimSpace(out)
	if s == "" {
		return err.Error()
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 6 {
		lines = append(lines[:3], append([]string{"..."}, lines[len(lines)-2:]...)...)
	}
	return fmt.Sprintf("%v: %s", err, strings.Join(lines, " | "))
}

func median(xs []float64) float64 {
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

func renderMarkdown(r report) string {
	var b strings.Builder
	b.WriteString(commentMarker + "\n")
	b.WriteString("### FAM Command Benchmark\n\n")

	baseDesc := fmt.Sprintf("`%s`", r.BaseLabel)
	if r.BaseSHA != "" {
		baseDesc += fmt.Sprintf(" (`%s`)", r.BaseSHA)
	}
	headDesc := fmt.Sprintf("`%s`", r.HeadLabel)
	if r.HeadSHA != "" {
		headDesc += fmt.Sprintf(" (`%s`)", r.HeadSHA)
	}
	fmt.Fprintf(&b, "Median wall time across %d runs (%d warmup), interleaved on the same machine: %s vs %s.\n\n",
		r.Runs, r.Warmup, baseDesc, headDesc)

	fmt.Fprintf(&b, "| Command | `%s` | `%s` | Diff |\n", r.BaseLabel, r.HeadLabel)
	b.WriteString("|---|---:|---:|---:|\n")

	var failures []string
	for _, row := range r.Results {
		baseCell := formatSide(row.Base)
		headCell := formatSide(row.Head)
		diffCell := formatDiff(row.Base, row.Head)
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", row.Command, baseCell, headCell, diffCell)
		if row.Base.Error != "" {
			failures = append(failures, fmt.Sprintf("- `%s` (%s): %s", row.Command, r.BaseLabel, row.Base.Error))
		}
		if row.Head.Error != "" {
			failures = append(failures, fmt.Sprintf("- `%s` (%s): %s", row.Command, r.HeadLabel, row.Head.Error))
		}
	}

	if len(failures) > 0 {
		b.WriteString("\n<details>\n<summary>Errors</summary>\n\n")
		for _, f := range failures {
			b.WriteString(f + "\n")
		}
		b.WriteString("\n</details>\n")
	}
	return b.String()
}

func formatSide(s sideResult) string {
	switch {
	case s.Error != "":
		return "error"
	case s.NotPresent:
		return "n/a"
	default:
		return fmt.Sprintf("%.1f ms", s.MedianMS)
	}
}

func formatDiff(base, head sideResult) string {
	if base.Error != "" || head.Error != "" || base.NotPresent || head.NotPresent {
		return "—"
	}
	delta := head.MedianMS - base.MedianMS
	if math.Abs(delta) < 0.05 {
		return "0.0 ms (0.0%)"
	}
	if base.MedianMS > 0 {
		pct := (delta / base.MedianMS) * 100
		return fmt.Sprintf("%+.1f ms (%+.1f%%)", delta, pct)
	}
	return fmt.Sprintf("%+.1f ms", delta)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bench:", err)
	os.Exit(1)
}
