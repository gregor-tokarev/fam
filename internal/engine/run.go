package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gregor-tokarev/fam/internal/config"
	"github.com/gregor-tokarev/fam/internal/facts"
	"github.com/gregor-tokarev/fam/internal/output"
	pklconfig "github.com/gregor-tokarev/fam/internal/pklgen/config"
	"github.com/gregor-tokarev/fam/internal/progress"
	"github.com/gregor-tokarev/fam/internal/schema"
	"github.com/gregor-tokarev/fam/internal/secrets"
	"github.com/gregor-tokarev/fam/internal/transport"
	"github.com/gregor-tokarev/fam/internal/version"
)

// Mode is the command.
type Mode string

// Modes.
const (
	ModePlan   Mode = "plan"
	ModeApply  Mode = "apply"
	ModeVerify Mode = "verify"
)

// Options configure a run.
type Options struct {
	Mode         Mode
	Project      *Project
	Machines     []string
	Serial       bool
	SSHConfig    string
	SudoPassword func(ctx context.Context) ([]byte, error)
	Observer     transport.Observer
	// Progress receives short progress lines (for example the pkl download). It can be nil.
	Progress io.Writer
	// Reporter shows the stages and the tasks while the run runs. It can be nil.
	Reporter *progress.Reporter
	RunID    string
	// OnSecrets receives the secret values, so that the run log can hide them.
	OnSecrets func(values []string)
	// RefreshSecrets reads the secrets from Infisical also when the cache is fresh.
	RefreshSecrets bool
}

// machineRun holds the state of one machine during a run.
type machineRun struct {
	m      config.Machine
	p      *progress.Machine
	res    *output.MachineResult
	env    *Env
	spec   *config.Spec
	sects  []Section
	verify []VerifySpec
}

// Run runs plan, apply, or verify.
func Run(ctx context.Context, opts Options) *output.Run {
	start := time.Now()
	res := &output.Run{Command: string(opts.Mode), RunID: opts.RunID, Version: version.Version, Started: start.UTC().Format(time.RFC3339),
		Base: opts.Project.Base, Config: opts.Project.ConfigPath, Machines: []output.MachineResult{}}
	defer func() {
		res.Duration = time.Since(start).Round(time.Millisecond).String()
		res.OK = res.Error == ""
		for i := range res.Machines {
			m := &res.Machines[i]
			res.Summary.Plus(m.Counts)
			if m.Status == "failed" || m.Status == "error" || m.Status == "conflict" {
				res.OK = false
			}
			if m.Counts.VerifyBad > 0 {
				res.OK = false
			}
		}
	}()
	fail := func(err error) *output.Run { res.Error = err.Error(); return res }

	// 1. Validate the config.
	pkg, err := schema.Current()
	if err != nil {
		return fail(err)
	}
	// Put the fam package of this binary into the Pkl cache, so that Pkl needs no download.
	if err := schema.SeedAll(pkg); err != nil {
		return fail(fmt.Errorf("write the fam package into the Pkl cache: %w", err))
	}
	if err := schema.Check(opts.Project.Base, pkg); err != nil {
		return fail(err)
	}
	if _, err := os.Stat(opts.Project.ConfigPath); err != nil {
		return fail(fmt.Errorf("no config entrypoint: %w", err))
	}
	opts.Reporter.Step("evaluating the config")
	ev, err := config.NewEvaluator(ctx, opts.Project.Base, opts.Project.ConfigPath, opts.Progress)
	if err != nil {
		return fail(err)
	}
	defer ev.Close()
	local := facts.Facts{}
	{
		lt := transport.NewLocal("cockpit", nil, nil)
		if f, err := facts.Collect(ctx, lt, "", true, nil); err == nil {
			local = f
		}
	}
	// Step 1 reads only what does not depend on the facts of a machine: the machines with their
	// tags, and the secrets. Pkl is lazy, so the rest of the config is not evaluated here.
	// FAM evaluates and validates the full config once for each machine, with its own facts.
	vprops := local.Properties()
	machines, err := ev.MachineList(ctx, vprops)
	if err != nil {
		return fail(err)
	}
	if len(machines) == 0 {
		return fail(errors.New("the config has no machines"))
	}
	if err := checkProject(opts.Project); err != nil {
		return fail(err)
	}
	sec, err := ev.Secrets(ctx, vprops)
	if err != nil {
		return fail(err)
	}
	// All secret values that FAM hides: the values of the cockpit file, and the old values that
	// it finds in the secrets file of each machine (after a rotation, scripts still see them).
	var secretMu sync.Mutex
	var hidden []string
	addSecrets := func(values []string) {
		secretMu.Lock()
		defer secretMu.Unlock()
		hidden = append(hidden, values...)
		opts.Reporter.SetSecrets(hidden)
		if opts.OnSecrets != nil {
			opts.OnSecrets(append([]string{}, hidden...))
		}
	}
	defer func() {
		secretMu.Lock()
		defer secretMu.Unlock()
		output.Redact(res, hidden)
	}()
	var secretList []secrets.Secret
	var secretWarnings []string
	if sec != nil {
		opts.Reporter.Step("reading the secrets")
		src, err := loadSecrets(ctx, sec, opts.Project.Base, opts.RefreshSecrets, false)
		// Hide the credentials also when the read failed.
		addSecrets(src.values())
		if err != nil {
			return fail(err)
		}
		secretList, secretWarnings = src.List, src.Warnings
	}
	cockpits := 0
	for _, m := range machines {
		if m.IsCockpit() {
			cockpits++
		}
	}
	if cockpits > 1 {
		return fail(fmt.Errorf("the config has %d machines with `cockpit = true`. Only one machine can be the cockpit", cockpits))
	}
	if len(opts.Machines) > 0 {
		var sel []config.Machine
		for _, name := range opts.Machines {
			found := false
			for _, m := range machines {
				if m.Name == name {
					sel = append(sel, m)
					found = true
				}
			}
			if !found {
				return fail(fmt.Errorf("--machine %s: the config has no machine with this name", name))
			}
		}
		machines = sel
	}
	if len(machines) == 0 {
		return fail(errors.New("the config has no machines"))
	}

	// 2-7 for each machine. Machines run in parallel unless --serial.
	runs := make([]*machineRun, len(machines))
	// Add all machines before the first starts, so that the live lines keep their order.
	machineProgress := make([]*progress.Machine, len(machines))
	for i, m := range machines {
		machineProgress[i] = opts.Reporter.Machine(m.Name)
	}
	work := func(i int) {
		m := machines[i]
		mr := &machineRun{m: m, p: machineProgress[i], res: &output.MachineResult{Name: m.Name, Cockpit: m.Cockpit, Tags: m.Tags, Tasks: []output.TaskResult{}}}
		mr.res.Warnings = append(mr.res.Warnings, secretWarnings...)
		runs[i] = mr
		// pkl-go sends the requests of all evaluators over one channel, so machines can evaluate
		// at the same time.
		runMachine(ctx, opts, mr, secretList, addSecrets, func(props map[string]string) (*pklconfig.Config, error) {
			return ev.Eval(ctx, props)
		})
		mr.p.Done(mr.res.Status)
	}
	if opts.Serial {
		for i := range machines {
			work(i)
		}
	} else {
		var wg sync.WaitGroup
		for i := range machines {
			wg.Add(1)
			go func(i int) { defer wg.Done(); work(i) }(i)
		}
		wg.Wait()
	}
	for _, mr := range runs {
		res.Machines = append(res.Machines, *mr.res)
	}
	sort.SliceStable(res.Machines, func(i, j int) bool { return res.Machines[i].Name < res.Machines[j].Name })
	return res
}

// checkProject stops when the base does not contain the directories that the config needs.
func checkProject(p *Project) error {
	if st, err := os.Stat(p.InstructionsDir); err != nil || !st.IsDir() {
		return fmt.Errorf("the project has no instructions directory %s. Create it with an AGENTS.md file, or set --instructions", p.InstructionsDir)
	}
	if _, err := os.Stat(p.InstructionsDir + "/AGENTS.md"); err != nil {
		return fmt.Errorf("the project has no %s/AGENTS.md. Create it (it can be empty)", p.InstructionsDir)
	}
	return nil
}

type evalFunc func(props map[string]string) (*pklconfig.Config, error)

func setError(r *output.MachineResult, err error) {
	r.Status = "error"
	r.Error = err.Error()
	switch {
	case transport.IsConnError(err):
		r.ErrorKind = "connection"
	case errors.Is(err, facts.ErrWindows):
		r.ErrorKind = "unsupported"
	default:
		var ee *config.EvalError
		if errors.As(err, &ee) {
			r.ErrorKind = "config"
		} else if r.ErrorKind == "" {
			r.ErrorKind = "task"
		}
	}
}

func runMachine(ctx context.Context, opts Options, mr *machineRun, secretList []secrets.Secret, addSecrets func([]string), eval evalFunc) {
	m := mr.m
	var t transport.Transport
	if m.IsCockpit() {
		l := transport.NewLocal(m.Name, opts.SudoPassword, opts.Observer)
		t = l
		mr.res.Transport = "local"
	} else {
		if m.Address == "" {
			setError(mr.res, fmt.Errorf("machine %s has no address", m.Name))
			return
		}
		t = transport.NewSSH(m.Name, m.Target(), opts.SSHConfig, opts.Observer)
		mr.res.Transport = "ssh"
		mr.res.Target = m.Target()
	}
	defer t.Close()

	// 2. Collect facts.
	mr.p.Stage("collecting facts")
	f, err := facts.Collect(ctx, t, m.Name, m.Cockpit, m.Tags)
	if err != nil {
		setError(mr.res, err)
		return
	}
	t.SetOS(f.OS)
	if l, ok := t.(*transport.Local); ok {
		l.Shell = f.Shell
	}
	mr.res.Facts = &f
	mr.env = &Env{T: t, Facts: f, Machine: m, Project: opts.Project, Secrets: secretList}
	// Hide the old values of the secrets file on the machine before any script runs.
	if data, ok, err := t.ReadFile(ctx, mr.env.Home(secretsFile), false); err == nil && ok {
		if old, err := secrets.Parse(data); err == nil {
			values := make([]string, 0, len(old))
			for _, s := range old {
				values = append(values, s.Value)
			}
			addSecrets(values)
		}
	}

	// 3. Evaluate Pkl for this machine.
	mr.p.Stage("evaluating the config")
	cfg, err := eval(f.Properties())
	if err != nil {
		setError(mr.res, err)
		return
	}
	spec, err := config.Resolve(cfg, m.Name)
	if err != nil {
		mr.res.ErrorKind = "config"
		setError(mr.res, err)
		return
	}
	if opts.Mode == ModePlan || opts.Mode == ModeApply {
		mr.res.Warnings = append(mr.res.Warnings, missingSecrets(spec, secretList)...)
	}
	mr.spec = spec
	mr.p.Stage("preparing the tasks")
	sects, verify, err := buildSections(ctx, mr.env, spec, false)
	if err != nil {
		mr.res.ErrorKind = "config"
		setError(mr.res, err)
		return
	}
	mr.sects, mr.verify = sects, verify

	switch opts.Mode {
	case ModePlan:
		planMachine(ctx, mr)
	case ModeApply:
		applyMachine(ctx, mr)
	case ModeVerify:
		mr.res.Status = "ok"
		for _, s := range mr.sects {
			recordSkippedSection(mr.res, s)
		}
		verifyMachine(ctx, mr)
	}
}

func taskResult(t Task, st Status, detail string, err error) output.TaskResult {
	tr := output.TaskResult{Group: t.Group(), Kind: t.Kind(), Name: t.Name(), Status: string(st), Detail: detail}
	if err != nil {
		tr.Error = err.Error()
	}
	return tr
}

// check runs the check of one task and classifies its error.
func check(ctx context.Context, env *Env, t Task) (Check, error) {
	c, err := t.Check(ctx, env)
	if err != nil {
		var ce *CheckError
		if errors.As(err, &ce) && ce.Conflict {
			return Check{Status: Conflict}, err
		}
		return Check{Status: Error}, err
	}
	return c, nil
}

// parallelCommands is the number of checks or verify shells that run at the same time on one
// machine. Checks change nothing, so their order does not matter. The number stays below the
// default MaxSessions (10) of sshd, so that all of them share the one SSH connection. When sshd
// refuses a session, ssh opens its own connection for it, so a lower limit is slower, not an error.
const parallelCommands = 8

// checked is the result of one check of checkAll.
type checked struct {
	c   Check
	err error
	// ran is false when FAM did not start the check after a connection error of another check.
	ran bool
}

// checkAll runs the checks of tasks at the same time and returns the results in the order of
// tasks. After a connection error it starts no more checks, and lets the running checks end:
// a check can wait for a download that other machines share. FAM starts the checks in order, so
// a check that did not start comes after a check with a connection error. connErr is the first
// connection error. done runs after each check.
func checkAll(ctx context.Context, env *Env, ts []Task, done func()) (out []checked, connErr error) {
	out = make([]checked, len(ts))
	var mu sync.Mutex
	var stop atomic.Bool
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallelCommands)
	for i, t := range ts {
		sem <- struct{}{}
		if stop.Load() {
			<-sem
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			c, err := check(ctx, env, t)
			mu.Lock()
			out[i] = checked{c: c, err: err, ran: true}
			if transport.IsConnError(err) {
				stop.Store(true)
				if connErr == nil {
					connErr = err
				}
			}
			mu.Unlock()
			if done != nil {
				done()
			}
		}()
	}
	wg.Wait()
	return out, connErr
}

// progressDone returns a done function for checkAll that counts the finished checks. It gives
// no task name: several checks run, and the name of a finished check is not the slow one.
func progressDone(mr *machineRun) func() {
	var mu sync.Mutex
	n := 0
	return func() {
		mu.Lock()
		defer mu.Unlock()
		n++
		mr.p.Task(n, "")
	}
}

// recordSkippedSection reports an unsupported bundle in every output format.
func recordSkippedSection(r *output.MachineResult, s Section) bool {
	if s.SkipReason == "" {
		return false
	}
	r.Tasks = append(r.Tasks, output.TaskResult{Group: s.Name, Kind: "bundle", Name: strings.TrimPrefix(s.Name, "bundle "), Status: string(Skipped), Detail: s.SkipReason})
	r.Counts.Add(string(Skipped))
	return true
}

// 4. Plan: run every check. The checks run at the same time.
func planMachine(ctx context.Context, mr *machineRun) {
	r := mr.res
	r.Status = "ok"
	mr.p.Count("checking", countTasks(mr.sects, true))
	var ts []Task
	for _, s := range mr.sects {
		if s.SkipReason == "" {
			ts = append(ts, s.Tasks...)
		}
	}
	results, connErr := checkAll(ctx, mr.env, ts, progressDone(mr))
	i := 0
	for _, s := range mr.sects {
		if recordSkippedSection(r, s) {
			continue
		}
		for _, t := range s.Tasks {
			res := results[i]
			i++
			if !res.ran {
				setError(r, connErr)
				return
			}
			r.Tasks = append(r.Tasks, taskResult(t, res.c.Status, res.c.Detail, res.err))
			r.Counts.Add(string(res.c.Status))
			if transport.IsConnError(res.err) {
				setError(r, res.err)
				return
			}
		}
	}
	switch {
	case r.Counts.Conflict > 0:
		r.Status = "conflict"
	case r.Counts.Error > 0:
		r.Status = "error"
		r.ErrorKind = "task"
		r.Error = fmt.Sprintf("%d checks failed", r.Counts.Error)
	case r.Counts.Change > 0:
		r.Status = "changes"
	}
}

// 5-6. Apply: plan first, stop on a conflict, then check, apply, and check again for each task.
func applyMachine(ctx context.Context, mr *machineRun) {
	r := mr.res
	env := mr.env
	// The plan stops with a conflict before any change. The checks run at the same time.
	mr.p.Count("checking", countTasks(mr.sects, false))
	var all []Task
	for _, s := range mr.sects {
		all = append(all, s.Tasks...)
	}
	results, connErr := checkAll(ctx, env, all, progressDone(mr))
	// pre holds the result of this check for each task. The apply loop uses it while no task
	// before has changed the machine, so that a task that needs nothing costs one check, not two.
	pre := make(map[Task]checked, len(all))
	for i, t := range all {
		res := results[i]
		if !res.ran {
			setError(r, connErr)
			return
		}
		if transport.IsConnError(res.err) {
			setError(r, res.err)
			return
		}
		pre[t] = res
		if res.c.Status == Conflict {
			r.Tasks = append(r.Tasks, taskResult(t, Conflict, "", res.err))
			r.Counts.Add(string(Conflict))
		}
	}
	if r.Counts.Conflict > 0 {
		r.Status = "conflict"
		r.Error = "the plan has conflicts. FAM changed nothing on this machine"
		r.ErrorKind = "task"
		return
	}
	r.Status = "ok"
	stopped := false
	mr.p.Count("applying", countTasks(mr.sects, true))
	n := 0
	// From here, the checks of packages with an update command also read the version.
	env.probeVersions = true
	// order is the run order of the tasks of the apply loop. pos is the position of each task.
	var order []Task
	pos := map[Task]int{}
	for _, s := range mr.sects {
		if s.SkipReason == "" {
			for _, t := range s.Tasks {
				pos[t] = len(order)
				order = append(order, t)
			}
		}
	}
	// pre holds check results that are still current. A task with a satisfied result in pre
	// needs no new check. A task that changes the machine (an apply or an update) always checks
	// right before it. After a task changes the machine, FAM clears pre:
	//   - after an apply or a restart, each next task checks again before it runs, as before.
	//   - after an update, FAM checks the next tasks again at the same time, up to the next task
	//     with an update. An update can change files of other tasks, so the results of before
	//     the update are not current.
	isUpdater := func(t Task) bool { _, ok := t.(Updater); return ok }
	refresh := false
	refreshFrom := func(t Task) {
		refresh = false
		var seg []Task
		for _, x := range order[pos[t]:] {
			if isUpdater(x) {
				break
			}
			seg = append(seg, x)
		}
		results, _ := checkAll(ctx, env, seg, nil)
		pre = make(map[Task]checked, len(seg))
		for i, x := range seg {
			res := results[i]
			// A connection error is the result of the task, as when the task checks itself. Any
			// other error or a difference: the task checks again right before it runs.
			if res.ran && (res.err == nil && res.c.Status == Satisfied || transport.IsConnError(res.err)) {
				pre[x] = res
			}
		}
	}
	// The position of each task in r.Tasks.
	index := map[Task]int{}
	// A change of the sshd configuration applies only to new connections.
	reconnect := false
	for _, s := range mr.sects {
		if recordSkippedSection(r, s) {
			continue
		}
		var units []Unit
		failedInSection := false
		env.restarted = nil
		for _, t := range s.Tasks {
			if stopped {
				r.Tasks = append(r.Tasks, taskResult(t, Skipped, "an earlier task failed", nil))
				r.Counts.Add(string(Skipped))
				continue
			}
			n++
			mr.p.Task(n, taskLabel(t))
			if _, ok := pre[t]; !ok && refresh && !isUpdater(t) {
				refreshFrom(t)
			}
			// Only a satisfied result (or a connection error of the refresh) skips the check. A
			// task that changes the machine always checks right before, for example a removal
			// that checks the owner, or an update that needs the program.
			var p *checked
			if res, ok := pre[t]; ok && !isUpdater(t) && (res.err == nil && res.c.Status == Satisfied || transport.IsConnError(res.err)) {
				p = &res
			}
			st, detail, err, touch := applyTask(ctx, env, t, p)
			switch touch {
			case touchApply:
				pre, refresh = nil, false
			case touchUpdate:
				pre, refresh = nil, true
			}
			index[t] = len(r.Tasks)
			r.Tasks = append(r.Tasks, taskResult(t, st, detail, err))
			r.Counts.Add(string(st))
			if transport.IsConnError(err) {
				setError(r, err)
				stopped = true
				failedInSection = true
				continue
			}
			if st == Failed || st == Conflict {
				stopped = true
				failedInSection = true
				continue
			}
			if st == Changed {
				if _, ok := t.(*SshdEnvTask); ok {
					reconnect = true
				}
				// A Service task that restarted its unit satisfies the restart requests before it.
				// Requests after it still restart the unit at the end of the bundle.
				if len(env.restarted) > 0 {
					kept := units[:0]
					for _, u := range units {
						if !env.restarted[u] {
							kept = append(kept, u)
						}
					}
					units = kept
					env.restarted = nil
				}
				if rs, ok := t.(Restarter); ok {
					units = append(units, rs.Restarts()...)
				}
			}
		}
		// Restart each changed unit once, at the end of the bundle. No restart after a failure.
		if s.RestartUnits && !failedInSection && !stopped {
			seen := map[Unit]bool{}
			for _, u := range units {
				if stopped {
					break
				}
				if seen[u] {
					continue
				}
				seen[u] = true
				mr.p.Stage("restarting " + u.Name)
				pre, refresh = nil, false
				rr := output.RestartResult{Unit: u.Name, Scope: u.Scope, Status: "restarted"}
				if err := restartUnit(ctx, env, u); err != nil {
					rr.Status = "failed"
					rr.Error = err.Error()
					stopped = true
					if transport.IsConnError(err) {
						setError(r, err)
					}
				}
				r.Restarts = append(r.Restarts, rr)
			}
		}
	}
	if r.Status == "error" {
		return
	}
	if !stopped {
		mr.p.Stage("checking the shell blocks")
		stopped = restoreBlocks(ctx, env, mr, index)
		if r.Status == "error" {
			return
		}
	}
	if r.Counts.Failed > 0 || stopped {
		r.Status = "failed"
		r.ErrorKind = "task"
		if r.Error == "" {
			r.Error = "a task failed. FAM stopped the tasks after it"
		}
		return
	}
	if r.Counts.Changed > 0 {
		r.Status = "changed"
	}
	// 7. Verify.
	if reconnect {
		mr.p.Stage("reconnecting")
		if err := env.T.Reconnect(ctx); err != nil {
			setError(r, err)
			return
		}
	}
	verifyMachine(ctx, mr)
}

// restoreBlocks checks the FAM blocks of the shell startup files again after all other tasks.
// An installer that runs later in the same apply (for example the Vite+ installer) can append
// its own lines after a block that must stay at the end of the file. FAM puts the block back,
// so that the next plan is satisfied. It returns true when a block fails.
func restoreBlocks(ctx context.Context, env *Env, mr *machineRun, index map[Task]int) bool {
	r := mr.res
	var blocks []Task
	for _, s := range mr.sects {
		for _, t := range s.Tasks {
			if _, ok := t.(*BlockTask); !ok {
				continue
			}
			if _, ok := index[t]; ok {
				blocks = append(blocks, t)
			}
		}
	}
	// The checks of the blocks run at the same time. Most blocks are still in place.
	results, connErr := checkAll(ctx, env, blocks, nil)
	if connErr != nil {
		// Change no file after a connection error. Report the first block that lost it.
		for k, t := range blocks {
			if res := results[k]; transport.IsConnError(res.err) {
				i := index[t]
				prev := r.Tasks[i].Status
				r.Tasks[i] = taskResult(t, Failed, "check failed", res.err)
				r.Counts.Move(prev, string(Failed))
				break
			}
		}
		setError(r, connErr)
		return true
	}
	// After FAM writes one block, the results of the other blocks can be old: two startup files
	// can be one file (a link). Then each next block checks again before it runs, as before.
	wrote := false
	for k, t := range blocks {
		res := results[k]
		var p *checked
		if !wrote {
			if res.err == nil && res.c.Status == Satisfied {
				continue
			}
			if res.err == nil {
				p = &res
			}
		}
		st, detail, err, _ := applyTask(ctx, env, t, p)
		if st == Unchanged {
			continue
		}
		wrote = true
		if detail == "" && p != nil {
			detail = res.c.Detail
		}
		if st == Changed {
			detail += " (again: a later task changed the file)"
		}
		i := index[t]
		prev := r.Tasks[i].Status
		r.Tasks[i] = taskResult(t, st, detail, err)
		r.Counts.Move(prev, string(st))
		if transport.IsConnError(err) {
			setError(r, err)
		}
		if st == Failed || err != nil {
			return true
		}
	}
	return false
}

// touch tells how a task may have changed the machine.
type touch int

const (
	touchNone   touch = iota
	touchApply        // an apply, or a failure
	touchUpdate       // an update script ran
)

// applyTask runs check, apply, and check again. pre is a current, satisfied check result of
// this task; FAM uses it instead of a new check when it is not nil.
func applyTask(ctx context.Context, env *Env, t Task, pre *checked) (st Status, detail string, err error, tc touch) {
	var c Check
	if pre != nil {
		c, err = pre.c, pre.err
	} else {
		c, err = check(ctx, env, t)
	}
	if err != nil {
		if c.Status == Conflict {
			return Conflict, "", err, touchNone
		}
		// Never run apply after an error.
		return Failed, "check failed", err, touchNone
	}
	if c.Status == Satisfied {
		u, ok := t.(Updater)
		if !ok {
			return Unchanged, "", nil, touchNone
		}
		up, err := u.Update(ctx, env, c)
		if err != nil {
			return Failed, "update failed", err, touchApply
		}
		if !up.Ran {
			return Unchanged, "", nil, touchNone
		}
		diff := up.Diff
		if !up.Checked {
			if c2, err := check(ctx, env, t); err != nil {
				return Failed, "check after update failed", err, touchApply
			} else if c2.Status != Satisfied {
				diff = c2.Detail
			}
		}
		if diff != "" {
			return Failed, up.Detail, fmt.Errorf("the check finds a difference after the update: %s", diff), touchApply
		}
		if up.Changed {
			return Changed, up.Detail, nil, touchUpdate
		}
		return Unchanged, up.Detail, nil, touchUpdate
	}
	detail, err = t.Apply(ctx, env)
	if err != nil {
		return Failed, c.Detail, err, touchApply
	}
	c2, err := check(ctx, env, t)
	if err != nil {
		return Failed, "check after apply failed", err, touchApply
	}
	if c2.Status != Satisfied {
		msg := "the check still finds a difference after apply"
		if c2.Detail != "" {
			msg += ": " + c2.Detail
		}
		return Failed, c.Detail, errors.New(msg), touchApply
	}
	switch detail {
	case "", "written", "linked", "copied", "applied", "configured", "removed", "merged", "no change":
		detail = c.Detail
	default:
		if c.Detail != "" && !strings.Contains(detail, c.Detail) {
			detail = c.Detail + ": " + detail
		}
	}
	return Changed, detail, nil, touchApply
}

// countTasks counts the tasks of the sections. withoutSkipped leaves out the skipped sections.
func countTasks(sects []Section, withoutSkipped bool) int {
	n := 0
	for _, s := range sects {
		if withoutSkipped && s.SkipReason != "" {
			continue
		}
		n += len(s.Tasks)
	}
	return n
}

// taskLabel is the name of a task in the progress lines.
func taskLabel(t Task) string {
	return t.Group() + " › " + t.Kind() + " " + t.Name()
}
