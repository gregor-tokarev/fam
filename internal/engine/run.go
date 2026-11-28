package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gregor-tokarev/fam/internal/config"
	"github.com/gregor-tokarev/fam/internal/daemon"
	"github.com/gregor-tokarev/fam/internal/facts"
	"github.com/gregor-tokarev/fam/internal/output"
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
	// Update runs the update command of each package. Without it, apply skips the updates of a
	// bundle or agent whose verify scripts pass before the apply loop.
	Update bool
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
	// update runs all updates (--update).
	update bool
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
	ev := config.NewLazyEvaluator(opts.Project.Base, opts.Project.ConfigPath, pkg.Checksum, opts.Progress)
	defer ev.Close()
	local := facts.Facts{}
	{
		lt := transport.NewLocal("cockpit", nil, nil)
		if f, err := facts.CollectLocalCached(ctx, lt, "", true, nil); err == nil {
			local = f
		}
	}
	// Step 1 reads only what does not depend on the facts of a machine: the machines with their
	// tags, and the secrets. Pkl is lazy, so the rest of the config is not evaluated here.
	// FAM evaluates and validates the full config once for each machine, with its own facts.
	vprops := local.Properties()
	machines, sec, err := ev.Init(ctx, vprops)
	if err != nil {
		return fail(err)
	}
	if len(machines) == 0 {
		return fail(errors.New("the config has no machines"))
	}
	if err := checkProject(opts.Project); err != nil {
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
		mr := &machineRun{m: m, p: machineProgress[i], update: opts.Update, res: &output.MachineResult{Name: m.Name, Cockpit: m.Cockpit, Tags: m.Tags, Tasks: []output.TaskResult{}}}
		mr.res.Warnings = append(mr.res.Warnings, secretWarnings...)
		runs[i] = mr
		// pkl-go sends the requests of all evaluators over one channel, so machines can evaluate
		// at the same time.
		runMachine(ctx, opts, mr, local, secretList, addSecrets, func(props map[string]string) (*config.Spec, error) {
			return ev.EvalSpec(ctx, m.Name, props)
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

type evalSpecFunc func(props map[string]string) (*config.Spec, error)

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
		var ce *config.ConfigError
		if errors.As(err, &ee) || errors.As(err, &ce) {
			r.ErrorKind = "config"
		} else if r.ErrorKind == "" {
			r.ErrorKind = "task"
		}
	}
}

func runMachine(ctx context.Context, opts Options, mr *machineRun, cockpitFacts facts.Facts, secretList []secrets.Secret, addSecrets func([]string), evalSpec evalSpecFunc) {
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
		if dt := daemon.NewTransport(ctx, m.Name, m.Target(), opts.SSHConfig, opts.Observer); dt != nil {
			t = dt
		} else {
			t = transport.NewSSH(m.Name, m.Target(), opts.SSHConfig, opts.Observer)
		}
		mr.res.Transport = "ssh"
		mr.res.Target = m.Target()
	}
	defer t.Close()

	// 2. Collect facts (and read existing secrets file + run speculative checks in the same
	// initial SSH connection when cached facts are available).
	mr.p.Stage("collecting facts")
	var f facts.Facts
	var err error
	var oldSecrets []byte
	var oldSecretsKnown bool

	var specSpec *config.Spec
	var specEnv *Env
	var specSects []Section
	var specVerify []VerifySpec
	var specHit bool

	if m.IsCockpit() && cockpitFacts.OS != "" {
		f = cockpitFacts
		f.Machine = m.Name
		f.Cockpit = m.Cockpit
		f.Tags = append([]string{}, m.Tags...)
	} else if sbs, ok := t.(transport.SpecBootstrapper); ok {
		var expectedSHA string
		var specReqs []transport.SpecReq
		cf, hasCF := facts.LoadCache(m.Target(), opts.SSHConfig)
		if hasCF && (opts.Mode == ModePlan || opts.Mode == ModeApply) {
			cf.Facts.Machine = m.Name
			cf.Facts.Cockpit = m.Cockpit
			cf.Facts.Tags = append([]string{}, m.Tags...)
			if sp, serr := evalSpec(cf.Facts.Properties()); serr == nil {
				se := &Env{T: t, Facts: cf.Facts, Machine: m, Project: opts.Project, Secrets: secretList}
				if ss, sv, berr := buildSections(ctx, se, sp, false); berr == nil {
					specSpec, specEnv, specSects, specVerify = sp, se, ss, sv
					expectedSHA = cf.RawSHA256
					specReqs = buildSpecChecks(se, ss)
				}
			}
		}
		var factsRes transport.Result
		var hasSecrets bool
		factsRes, oldSecrets, hasSecrets, specHit, err = sbs.BootstrapSpec(ctx, facts.Script(), secretsFile, expectedSHA, specReqs)
		if err == nil {
			f, err = facts.Parse(ctx, t, factsRes, m.Name, m.Cockpit, m.Tags)
			oldSecretsKnown = true
			if !hasSecrets {
				oldSecrets = nil
			}
			if err == nil {
				liveSHA := facts.RawHash(factsRes.Stdout)
				if !hasCF || liveSHA != cf.RawSHA256 {
					facts.SaveCache(m.Target(), opts.SSHConfig, facts.CachedFacts{Facts: f, RawSHA256: liveSHA})
				}
				if liveSHA != expectedSHA {
					specHit = false
					sbs.ClearSpecCache()
				}
			}
		}
	} else if bs, ok := t.(transport.Bootstrapper); ok {
		var factsRes transport.Result
		var hasSecrets bool
		factsRes, oldSecrets, hasSecrets, err = bs.Bootstrap(ctx, facts.Script(), secretsFile)
		if err == nil {
			f, err = facts.Parse(ctx, t, factsRes, m.Name, m.Cockpit, m.Tags)
			oldSecretsKnown = true
			if !hasSecrets {
				oldSecrets = nil
			}
		}
	} else {
		f, err = facts.Collect(ctx, t, m.Name, m.Cockpit, m.Tags)
	}
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
	if !oldSecretsKnown {
		if data, ok, err := t.ReadFile(ctx, mr.env.Home(secretsFile), false); err == nil && ok {
			oldSecrets = data
		}
	}
	if len(oldSecrets) > 0 {
		if old, err := secrets.Parse(oldSecrets); err == nil {
			values := make([]string, 0, len(old))
			for _, s := range old {
				values = append(values, s.Value)
			}
			addSecrets(values)
		}
	}

	// 3. Evaluate Pkl for this machine (or reuse the verified speculative evaluation).
	var spec *config.Spec
	var sects []Section
	var verify []VerifySpec
	if specHit && specSpec != nil && specSects != nil {
		spec = specSpec
		specEnv.Facts = f
		mr.env = specEnv
		sects, verify = specSects, specVerify
		if opts.Mode == ModePlan || opts.Mode == ModeApply {
			mr.res.Warnings = append(mr.res.Warnings, missingSecrets(spec, secretList)...)
		}
		mr.spec = spec
		mr.sects, mr.verify = sects, verify
	} else {
		mr.p.Stage("evaluating the config")
		spec, err = evalSpec(f.Properties())
		if err != nil {
			setError(mr.res, err)
			return
		}
		if opts.Mode == ModePlan || opts.Mode == ModeApply {
			mr.res.Warnings = append(mr.res.Warnings, missingSecrets(spec, secretList)...)
		}
		mr.spec = spec
		mr.p.Stage("preparing the tasks")
		sects, verify, err = buildSections(ctx, mr.env, spec, false)
		if err != nil {
			mr.res.ErrorKind = "config"
			setError(mr.res, err)
			return
		}
		mr.sects, mr.verify = sects, verify
	}

	switch opts.Mode {
	case ModePlan:
		planMachine(ctx, mr)
	case ModeApply:
		applyMachine(ctx, mr)
	case ModeVerify:
		if err := resolveAllSkills(ctx, mr.sects); err != nil {
			mr.res.ErrorKind = "config"
			setError(mr.res, err)
			return
		}
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
	// before is the result of Updater.Before after a satisfied check, when hasBefore is true.
	before    string
	hasBefore bool
}

// checkAll runs the checks of tasks at the same time and returns the results in the order of
// tasks. After a connection error it starts no new checks, and lets the running checks end:
// a check can wait for a download that other machines share. FAM starts the checks in order, so
// a check that did not start comes after a check with a connection error. connErr is the first
// connection error. rp shows the progress; it can be nil. With before, an Updater with an
// update command also runs its Before at the same time as its check. FAM keeps that value only
// when the check is satisfied. The caller sets before only for tasks that an earlier check found
// satisfied, so the program is installed, and Before only reads.
func maxCheckConcurrency(t transport.Transport) int {
	if cp, ok := t.(transport.ConcurrencyProvider); ok {
		if n := cp.MaxConcurrency(); n > 0 {
			return n
		}
	}
	return parallelCommands
}

func checkAll(ctx context.Context, env *Env, ts []Task, rp *running, before bool) (out []checked, connErr error) {
	if sbs, ok := env.T.(transport.SpecBootstrapper); ok {
		defer sbs.ClearSpecCache()
	}
	out = make([]checked, len(ts))
	prevCache := env.mgrCache
	env.mgrCache = newManagerCache(ts)
	defer func() { env.mgrCache = prevCache }()
	var mu sync.Mutex
	var stop atomic.Bool
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxCheckConcurrency(env.T))
	for i, t := range ts {
		sem <- struct{}{}
		if stop.Load() {
			<-sem
			break
		}
		wg.Add(1)
		rp.begin(i, taskLabel(t))
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			var b string
			var berr error
			var bwg sync.WaitGroup
			u, isUpdater := t.(Updater)
			withBefore := before && isUpdater && u.Updates()
			if withBefore {
				bwg.Add(1)
				go func() {
					defer bwg.Done()
					b, berr = u.Before(ctx, env)
				}()
			}
			c, err := check(ctx, env, t)
			bwg.Wait()
			// A detail can be a part of the full output of a script: keep only a copy of the
			// detail, so that the output does not stay in memory.
			c.Detail = strings.Clone(c.Detail)
			var sle *skillLoadError
			if err != nil && !transport.IsConnError(err) && !errors.As(err, &sle) {
				// Keep only the message (an ExitError holds all output of the script): all
				// results stay in memory until the last check ends.
				err = &CheckError{Conflict: c.Status == Conflict, Msg: err.Error()}
			}
			res := checked{c: c, err: err, ran: true}
			// A lost connection in Before leaves hasBefore false: the update then reads the value
			// itself, right before it runs.
			if withBefore && err == nil && c.Status == Satisfied && berr == nil {
				res.before, res.hasBefore = b, true
			}
			mu.Lock()
			out[i] = res
			if transport.IsConnError(err) {
				stop.Store(true)
				if connErr == nil {
					connErr = err
				}
			}
			mu.Unlock()
			rp.end(i)
		}()
	}
	wg.Wait()
	return out, connErr
}

// running shows the progress of steps that run at the same time: the number of finished steps,
// and the step that runs for the longest time, so that a slow step has its own name.
type running struct {
	mu    sync.Mutex
	p     *progress.Machine
	done  int
	steps map[int]runningStep
	// index, when it is not zero, is the step number to show, in place of the count.
	index int
}

type runningStep struct {
	label string
	at    time.Time
}

func newRunning(p *progress.Machine) *running {
	return &running{p: p, steps: map[int]runningStep{}}
}

func (r *running) begin(id int, label string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps[id] = runningStep{label: label, at: time.Now()}
	r.show()
}

func (r *running) end(id int) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.steps, id)
	r.done++
	r.show()
}

// show names the oldest running step. The caller holds the lock.
func (r *running) show() {
	var oldest runningStep
	for _, st := range r.steps {
		if oldest.at.IsZero() || st.at.Before(oldest.at) {
			oldest = st
		}
	}
	i := r.done
	if r.index > 0 {
		i = r.index
	}
	r.p.TaskSince(i, oldest.label, oldest.at)
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
	results, connErr := checkAll(ctx, mr.env, ts, newRunning(mr.p), false)
	if err := resolveAllSkills(ctx, mr.sects); err != nil {
		r.ErrorKind = "config"
		setError(r, err)
		return
	}
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
	results, connErr := checkAll(ctx, env, all, newRunning(mr.p), false)
	if err := resolveAllSkills(ctx, mr.sects); err != nil {
		r.ErrorKind = "config"
		setError(r, err)
		return
	}
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
	// satisfied holds the tasks that the first check pass found satisfied. A refresh checks only
	// these: a task that was not satisfied probably changes the machine, and then the results of
	// the tasks after it are old.
	satisfied := make(map[Task]bool, len(all))
	for t, res := range pre {
		satisfied[t] = res.err == nil && res.c.Status == Satisfied
	}
	if r.Counts.Conflict > 0 {
		r.Status = "conflict"
		r.Error = "the plan has conflicts. FAM changed nothing on this machine"
		r.ErrorKind = "task"
		return
	}
	// Start downloading any unsatisfied GitHubRelease assets in parallel with update gates and earlier apply tasks.
	prefetchReleases(ctx, env, all, pre)
	var preVerify map[int][2]shellRun
	if !mr.update {
		var err error
		preVerify, err = gateUpdates(ctx, mr)
		if err != nil {
			setError(r, err)
			return
		}
	}
	r.Status = "ok"
	stopped := false
	mr.p.Count("applying", countTasks(mr.sects, true))
	n := 0
	// order is the run order of the tasks of the apply loop. pos is the position of each task.
	var order []Task
	pos := map[Task]int{}
	// secEnd is the position after the last task of the section of each task.
	secEnd := map[Task]int{}
	sectionHasRestarts := map[Task]bool{}
	for _, s := range mr.sects {
		if s.SkipReason == "" {
			hasR := false
			for _, t := range s.Tasks {
				pos[t] = len(order)
				order = append(order, t)
				if rs, ok := t.(Restarter); ok && len(rs.Restarts()) > 0 {
					hasR = true
				}
			}
			for _, t := range s.Tasks {
				secEnd[t] = len(order)
				sectionHasRestarts[t] = s.RestartUnits && hasR
			}
		}
	}
	batchedDone := map[*PackageTask]batchPkgResult{}
	// pre holds check results that are still current. A task with a satisfied result in pre
	// needs no new check, also a package that then runs its update. A task with a result that is
	// not satisfied checks again right before its apply. After a task changes the machine, FAM
	// clears pre:
	//   - after an apply or a restart, each next task checks again before it runs, as before.
	//   - after an update, FAM checks the next tasks again at the same time, up to and including
	//     the next task with an update, and reads its version before the update. An update can
	//     change files of other tasks, so the results of before the update are not current.
	updates := func(t Task) bool { u, ok := t.(Updater); return ok && u.Updates() }
	refresh := false
	// fresh is true while pre holds results of a refresh: each result there is the check of its
	// task, as if the task checked itself at its turn, because no task changed the machine
	// after the refresh. FAM uses it also when it is an error. A result of the first check pass
	// is used only when it is satisfied: there, as before, an error makes the task check again.
	fresh := false
	// inSection limits the refresh to the section of t: a restart at the end of the section
	// changes the machine, so results after it would be old.
	refreshFrom := func(t Task, n int, inSection bool) {
		refresh = false
		end := len(order)
		if inSection {
			end = secEnd[t]
		}
		var seg []Task
		for _, x := range order[pos[t]:end] {
			if !satisfied[x] {
				break
			}
			seg = append(seg, x)
			// The update of x can change the machine, so the results after it would be old.
			if updates(x) {
				break
			}
		}
		// The progress keeps the number of the apply step and names the slowest check.
		rp := newRunning(mr.p)
		rp.index = n
		results, _ := checkAll(ctx, env, seg, rp, true)
		pre = make(map[Task]checked, len(seg))
		for i, x := range seg {
			if results[i].ran {
				pre[x] = results[i]
			}
		}
		fresh = true
	}
	// The position of each task in r.Tasks.
	index := map[Task]int{}
	// A change of the sshd configuration applies only to new connections.
	reconnect := false
	// mutated tracks whether any task applied, updated, or restarted a unit on this machine.
	mutated := false
	// envDirty tracks whether a prior task may have changed shell startup files, env.sh, or
	// secrets.env, requiring a fresh persistent remote session before later commands run.
	envDirty := false
	sa, canSeq := env.T.(transport.SeqApplier)
	if canSeq && !sa.SupportsApplySeq() {
		canSeq = false
	}
	flushEnv := func() bool {
		if !envDirty {
			return true
		}
		envDirty = false
		if reconnect {
			mr.p.Stage("reconnecting")
			if err := env.T.Reconnect(ctx); err != nil {
				setError(r, err)
				stopped = true
				return false
			}
			reconnect = false
		}
		if sr, ok := env.T.(transport.SessionRefresher); ok {
			if err := sr.RefreshSession(ctx); err != nil {
				setError(r, err)
				stopped = true
				return false
			}
		}
		if sa != nil {
			canSeq = sa.SupportsApplySeq()
		}
		return true
	}
	for _, s := range mr.sects {
		if recordSkippedSection(r, s) {
			continue
		}
		var units []Unit
		failedInSection := false
		env.restarted = nil
		recordOutcome := func(t Task, st Status, detail string, err error, tc touch, fromSeq bool) {
			switch tc {
			case touchApply:
				mutated = true
				pre, refresh, fresh = nil, true, false
				if _, isPkg := t.(*PackageTask); !isPkg {
					env.aptUpdated = false
				}
			case touchUpdate:
				mutated = true
				pre, refresh, fresh = nil, true, false
			}
			index[t] = len(r.Tasks)
			r.Tasks = append(r.Tasks, taskResult(t, st, detail, err))
			r.Counts.Add(string(st))
			if transport.IsConnError(err) {
				setError(r, err)
				stopped = true
				failedInSection = true
				return
			}
			if st == Failed || st == Conflict {
				stopped = true
				failedInSection = true
				return
			}
			if st == Changed {
				if s.Name == "environment" || (fromSeq && mayChangeShellEnv(t)) {
					envDirty = true
				}
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
		for ti := 0; ti < len(s.Tasks); ti++ {
			t := s.Tasks[ti]
			if stopped {
				r.Tasks = append(r.Tasks, taskResult(t, Skipped, "an earlier task failed", nil))
				r.Counts.Add(string(Skipped))
				continue
			}
			if envDirty && s.Name != "environment" {
				if !flushEnv() {
					failedInSection = true
					r.Tasks = append(r.Tasks, taskResult(t, Skipped, "an earlier task failed", nil))
					r.Counts.Add(string(Skipped))
					continue
				}
			}
			// After an apply, update, or restart, a task that the first pass found satisfied
			// starts a parallel/batched refresh up to the next non-satisfied task or updater.
			if _, ok := pre[t]; !ok && refresh && satisfied[t] {
				refreshFrom(t, n+1, s.RestartUnits && len(units) > 0)
			}
			// A satisfied result skips the check. A task that applies a change always checks right
			// before, for example a removal that checks the owner.
			var p *checked
			if res, ok := pre[t]; ok {
				switch {
				case res.err == nil && res.c.Status == Satisfied:
					p = &res
				case res.err != nil && fresh:
					p = &res
				}
			}
			if pt, ok := t.(*PackageTask); ok {
				if br, done := batchedDone[pt]; done {
					delete(batchedDone, pt)
					n++
					mr.p.Task(n, taskLabel(t))
					recordOutcome(t, Changed, br.detail, nil, touchApply, false)
					continue
				}
			}
			// When a task must check/apply and is the start of a batchable same-manager package run,
			// install all compatible packages in a single manager invocation.
			if p == nil {
				if batch := collectPackageBatch(order, pos[t], pre, satisfied, secEnd, sectionHasRestarts); len(batch) >= 2 {
					mr.p.Task(n+1, fmt.Sprintf("install %d %s packages", len(batch), batch[0].Manager))
					resMap, berr := runPackageBatch(ctx, env, batch)
					if berr != nil {
						n++
						recordOutcome(t, Failed, "install", berr, touchApply, false)
						continue
					}
					if len(resMap) == len(batch) {
						pt := batch[0]
						for _, bpt := range batch[1:] {
							batchedDone[bpt] = resMap[bpt]
						}
						n++
						recordOutcome(t, Changed, resMap[pt].detail, nil, touchApply, false)
						if sa != nil {
							canSeq = sa.SupportsApplySeq()
						}
						continue
					}
				}
			}
			// When a task must check/apply and the transport supports sequential remote execution,
			// execute the contiguous run of encodable tasks in this section in one round trip.
			if p == nil && canSeq {
				var ops []transport.ApplyOp
				for j := ti; j < len(s.Tasks); j++ {
					op, ok := encodeApplyOp(ctx, env, j, s.Tasks[j])
					if !ok {
						break
					}
					ops = append(ops, op)
				}
				if len(ops) > 0 {
					mr.p.Task(n+1, taskLabel(s.Tasks[ti]))
					doneOps := make(map[int]transport.ApplyOpResult, len(ops))
					seqErr := sa.ApplySeq(ctx, ops, func(res transport.ApplyOpResult) {
						doneOps[res.Index] = res
						nextPos := res.Index + 1
						if res.Status != "fallback" && nextPos < ti+len(ops) {
							mr.p.Task(n+(nextPos-ti)+1, taskLabel(s.Tasks[nextPos]))
						}
					})
					consumed := 0
					for j := ti; j < ti+len(ops); j++ {
						if res, ok := doneOps[j]; ok {
							if res.Status == "fallback" {
								break
							}
							n++
							consumed++
							st, detail, err, tc := decodeApplyOpResult(res)
							recordOutcome(s.Tasks[j], st, detail, err, tc, true)
							if stopped {
								break
							}
							continue
						}
						if seqErr != nil {
							n++
							consumed++
							recordOutcome(s.Tasks[j], Failed, "check failed", seqErr, touchApply, true)
							break
						}
					}
					if consumed > 0 {
						ti += consumed - 1
						continue
					}
				}
			}
			n++
			mr.p.Task(n, taskLabel(t))
			st, detail, err, tc := applyTask(ctx, env, t, p)
			recordOutcome(t, st, detail, err, tc, false)
			if sa != nil {
				canSeq = sa.SupportsApplySeq()
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
				mutated = true
				pre, refresh, fresh = nil, true, false
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
	if !stopped && mutated {
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
	if mutated || reconnect {
		preVerify = nil
	}
	verifyMachineWithCache(ctx, mr, preVerify)
}

// mayChangeShellEnv reports whether mutating t could alter shell startup files or environment
// exports inherited by subsequent remote commands.
func mayChangeShellEnv(t Task) bool {
	isEnvPath := func(p string) bool {
		clean := path.Clean(strings.ReplaceAll(p, "\\", "/"))
		base := path.Base(clean)
		if strings.HasPrefix(base, ".bash") || strings.HasPrefix(base, ".zsh") ||
			base == ".profile" || base == ".shrc" || base == ".env" || base == "environment" {
			return true
		}
		return strings.Contains(clean, "/.config/fam/") || strings.Contains(clean, "/.ssh/") ||
			strings.HasPrefix(clean, "/etc/profile") || strings.HasPrefix(clean, "/etc/bash") ||
			strings.HasPrefix(clean, "/etc/zsh") || clean == "/etc/environment"
	}
	switch x := t.(type) {
	case *ScriptTask, *PackageTask, *SshdEnvTask, *OwnedRemoveTask:
		return true
	case *FileTask:
		return isEnvPath(x.Path)
	case *SymlinkTask:
		return isEnvPath(x.Path)
	case *BlockTask:
		return isEnvPath(x.Path)
	case *MergeTask:
		return isEnvPath(x.Path)
	}
	return false
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
	results, connErr := checkAll(ctx, env, blocks, nil, false)
	if connErr != nil {
		// Change no file after a connection error. Report the first block that lost it.
		for k, t := range blocks {
			if res := results[k]; transport.IsConnError(res.err) {
				i := index[t]
				prev := r.Tasks[i].Status
				r.Tasks[i] = taskResult(t, Failed, "check failed", res.err)
				r.Counts.Move(prev, string(Failed))
				setError(r, res.err)
				return true
			}
		}
		setError(r, connErr)
		return true
	}
	// A block with a satisfied result needs nothing. Each other block checks again right before
	// FAM writes it, as before. After FAM writes one block, the results of the other blocks can be
	// old (two startup files can be one file, a link), so each next block checks again.
	wrote := false
	for k, t := range blocks {
		res := results[k]
		if !wrote && res.err == nil && res.c.Status == Satisfied {
			continue
		}
		st, detail, err, _ := applyTask(ctx, env, t, nil)
		if st == Unchanged {
			continue
		}
		wrote = true
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
	if pre == nil && !mayChangeShellEnv(t) {
		if fa, ok := t.(FastApplier); ok {
			if handled, fst, fdetail, ferr, ftc := fa.ApplyChecked(ctx, env); handled {
				return fst, fdetail, ferr, ftc
			}
		}
	}
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
		// The value before the update: from the refresh after the last update, else read now.
		// A lost connection gives "", and the update then reports the connection error.
		var before string
		if pre != nil && pre.hasBefore {
			before = pre.before
		} else {
			before, _ = u.Before(ctx, env)
		}
		up, err := u.Update(ctx, env, before)
		if err != nil {
			return Failed, "update failed", err, touchApply
		}
		if !up.Ran {
			return Unchanged, up.Detail, nil, touchNone
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
	if mayChangeShellEnv(t) && t.Group() != "environment" {
		if sr, ok := env.T.(transport.SessionRefresher); ok {
			var rerr error
			switch t.(type) {
			case *ScriptTask, *PackageTask:
				rerr = sr.RefreshSessionIfNeeded(ctx)
			default:
				rerr = sr.RefreshSession(ctx)
			}
			if rerr != nil {
				return Failed, "check after apply failed", rerr, touchApply
			}
		}
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
