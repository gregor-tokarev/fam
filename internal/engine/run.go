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
	"time"

	"github.com/gregor-tokarev/fam/internal/config"
	"github.com/gregor-tokarev/fam/internal/facts"
	"github.com/gregor-tokarev/fam/internal/output"
	pklconfig "github.com/gregor-tokarev/fam/internal/pklgen/config"
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
	RunID    string
	// OnSecrets receives the secret values, so that the run log can hide them.
	OnSecrets func(values []string)
}

// machineRun holds the state of one machine during a run.
type machineRun struct {
	m      config.Machine
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
	ev, err := config.NewEvaluator(ctx, opts.Project.Base, opts.Project.ConfigPath, opts.Progress)
	if err != nil {
		return fail(err)
	}
	defer ev.Close()
	local := facts.Facts{}
	{
		lt := transport.NewLocal("cockpit", nil, nil)
		if f, err := facts.Collect(ctx, lt, "", "cockpit"); err == nil {
			local = f
		}
	}
	// Step 1 reads only what does not depend on the facts of a machine: the machines and their
	// roles, and the secrets. Pkl is lazy, so the rest of the config is not evaluated here.
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
	if sec != nil {
		if sec.Delivery != "copy-file" {
			return fail(fmt.Errorf("secrets delivery %q is not supported. Use \"copy-file\"", sec.Delivery))
		}
		secretList, err = secrets.Load(sec.File, opts.Project.Base)
		if err != nil {
			return fail(err)
		}
		values := make([]string, 0, len(secretList))
		for _, s := range secretList {
			values = append(values, s.Value)
		}
		addSecrets(values)
	}
	cockpits := 0
	for _, m := range machines {
		if m.IsCockpit() {
			cockpits++
		}
	}
	if cockpits > 1 {
		return fail(fmt.Errorf("the config has %d machines with role \"cockpit\". Only one machine can be the cockpit", cockpits))
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
	var evalMu sync.Mutex
	work := func(i int) {
		m := machines[i]
		mr := &machineRun{m: m, res: &output.MachineResult{Name: m.Name, Role: m.Role, Tasks: []output.TaskResult{}}}
		runs[i] = mr
		runMachine(ctx, opts, mr, secretList, addSecrets, func(props map[string]string) (*pklconfig.Config, error) {
			evalMu.Lock()
			defer evalMu.Unlock()
			return ev.Eval(ctx, props)
		})
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
	if p.SkillsDir != "" {
		if st, err := os.Stat(p.SkillsDir); err == nil && !st.IsDir() {
			return fmt.Errorf("%s is not a directory", p.SkillsDir)
		}
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
	f, err := facts.Collect(ctx, t, m.Name, m.Role)
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
	mr.spec = spec
	sects, verify, err := buildSections(ctx, mr.env, spec)
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

// 4. Plan: run every check.
func planMachine(ctx context.Context, mr *machineRun) {
	r := mr.res
	r.Status = "ok"
	for _, s := range mr.sects {
		for _, t := range s.Tasks {
			c, err := check(ctx, mr.env, t)
			r.Tasks = append(r.Tasks, taskResult(t, c.Status, c.Detail, err))
			r.Counts.Add(string(c.Status))
			if transport.IsConnError(err) {
				setError(r, err)
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
	// The plan stops with a conflict before any change.
	for _, s := range mr.sects {
		for _, t := range s.Tasks {
			c, err := check(ctx, env, t)
			if transport.IsConnError(err) {
				setError(r, err)
				return
			}
			if c.Status == Conflict {
				r.Tasks = append(r.Tasks, taskResult(t, Conflict, "", err))
				r.Counts.Add(string(Conflict))
			}
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
	// The position of each task in r.Tasks.
	index := map[Task]int{}
	// A change of the sshd configuration applies only to new connections.
	reconnect := false
	for _, s := range mr.sects {
		var units []Unit
		failedInSection := false
		env.restarted = nil
		for _, t := range s.Tasks {
			if stopped {
				r.Tasks = append(r.Tasks, taskResult(t, Skipped, "an earlier task failed", nil))
				r.Counts.Add(string(Skipped))
				continue
			}
			st, detail, err := applyTask(ctx, env, t)
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
	for _, s := range mr.sects {
		for _, t := range s.Tasks {
			bt, ok := t.(*BlockTask)
			if !ok {
				continue
			}
			i, ok := index[t]
			if !ok {
				continue
			}
			c, err := check(ctx, env, bt)
			if err == nil && c.Status == Satisfied {
				continue
			}
			st, detail, err := applyTask(ctx, env, bt)
			if st == Unchanged {
				continue
			}
			if detail == "" {
				detail = c.Detail
			}
			if st == Changed {
				detail += " (again: a later task changed the file)"
			}
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
	}
	return false
}

// applyTask runs check, apply, and check again.
func applyTask(ctx context.Context, env *Env, t Task) (Status, string, error) {
	c, err := check(ctx, env, t)
	if err != nil {
		if c.Status == Conflict {
			return Conflict, "", err
		}
		// Never run apply after an error.
		return Failed, "check failed", err
	}
	if c.Status == Satisfied {
		if u, ok := t.(Updater); ok {
			changed, detail, err := u.Update(ctx, env)
			if err != nil {
				return Failed, "update failed", err
			}
			if c2, err := check(ctx, env, t); err != nil {
				return Failed, "check after update failed", err
			} else if c2.Status != Satisfied {
				return Failed, detail, fmt.Errorf("the check finds a difference after the update: %s", c2.Detail)
			}
			if changed {
				return Changed, detail, nil
			}
			return Unchanged, detail, nil
		}
		return Unchanged, "", nil
	}
	detail, err := t.Apply(ctx, env)
	if err != nil {
		return Failed, c.Detail, err
	}
	c2, err := check(ctx, env, t)
	if err != nil {
		return Failed, "check after apply failed", err
	}
	if c2.Status != Satisfied {
		msg := "the check still finds a difference after apply"
		if c2.Detail != "" {
			msg += ": " + c2.Detail
		}
		return Failed, c.Detail, errors.New(msg)
	}
	switch detail {
	case "", "written", "linked", "copied", "applied", "configured", "removed", "merged", "no change":
		detail = c.Detail
	default:
		if c.Detail != "" && !strings.Contains(detail, c.Detail) {
			detail = c.Detail + ": " + detail
		}
	}
	return Changed, detail, nil
}
