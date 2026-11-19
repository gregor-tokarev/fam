package engine

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gregor-tokarev/fam/internal/config"
	"github.com/gregor-tokarev/fam/internal/output"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks"
	"github.com/gregor-tokarev/fam/internal/transport"
)

// VerifySpec is one verify script of a bundle.
type VerifySpec struct {
	Bundle string
	Name   string
	Event  string
	Script string
	// Group is the task group that the script proves, for example "bundle rustup".
	Group string
	// Gate is true when a pass of the script before apply makes the updates of Group unnecessary.
	Gate bool
}

// buildSections returns the task sections of one machine in run order:
// environment and secrets, agents, bundles (every machine, then the tags, then the machine),
// skills, instructions.
func buildSections(ctx context.Context, env *Env, spec *config.Spec, staticOnly bool) ([]Section, []VerifySpec, error) {
	var out []Section
	envSec, err := environmentSection(env, spec, env.Secrets)
	if err != nil {
		return nil, nil, err
	}
	out = append(out, envSec)
	// Agents come after the environment, so that their installers see the variables (for
	// example a proxy) and the secrets.
	agentSec, err := agentsSection(env, spec)
	if err != nil {
		return nil, nil, err
	}
	if len(agentSec.Tasks) > 0 {
		out = append(out, agentSec)
	}
	var verify []VerifySpec
	for _, u := range spec.Bundles {
		s, err := bundleSection(ctx, u, env.Facts, staticOnly)
		if err != nil {
			return nil, nil, err
		}
		if len(s.Tasks) > 0 || s.SkipReason != "" {
			out = append(out, s)
		}
		if u.State != "present" || u.Skipped() {
			continue
		}
		for _, v := range u.Bundle.Verify {
			name := firstLine(v.Script)
			if v.Name != nil {
				name = *v.Name
			}
			verify = append(verify, VerifySpec{Bundle: u.Bundle.Name, Name: name, Event: string(v.Event), Script: v.Script, Group: "bundle " + u.Bundle.Name, Gate: true})
		}
	}
	// The skills of the bundle tasks: each name once, across all bundles.
	bundleSkills := map[string]string{}
	keep := map[string]bool{}
	var skillNames []string
	for _, sec := range out {
		for _, t := range sec.Tasks {
			if st, ok := t.(*SkillTask); ok {
				if prev, ok := bundleSkills[st.Skill.Name]; ok {
					return nil, nil, fmt.Errorf("two skills have the name %q: %s and %s", st.Skill.Name, prev, st.Skill.Dir)
				}
				bundleSkills[st.Skill.Name] = st.Skill.Dir
				keep[st.Skill.Name] = true
				skillNames = append(skillNames, st.Skill.Name)
			}
		}
	}
	// Each skill name that a bundle lists. Keep existing skills of skipped bundles without
	// loading their sources or verifying them.
	listed := map[string]bool{}
	for _, u := range spec.Bundles {
		for i, task := range append(append([]tasks.Task{}, u.Bundle.Tasks...), u.Bundle.Uninstall...) {
			if name, ok := bundleSkillName(task, u); ok {
				listed[name] = true
				if u.Skipped() && i < len(u.Bundle.Tasks) {
					keep[name] = true
				}
			}
		}
	}
	sk, err := skillsSection(env, keep, listed)
	if err != nil {
		return nil, nil, err
	}
	// A skill that an absent bundle removes but another bundle installs stays.
	installed := map[string]bool{}
	for _, n := range skillNames {
		installed[n] = true
	}
	for i := range out {
		kept := out[i].Tasks[:0]
		for _, t := range out[i].Tasks {
			if rt, ok := t.(*SkillRemoveTask); ok && installed[rt.Name_] {
				continue
			}
			kept = append(kept, t)
		}
		out[i].Tasks = kept
	}
	out = append(out, sk)
	in, err := instructionsSection(env, spec)
	if err != nil {
		return nil, nil, err
	}
	out = append(out, in)
	for _, a := range spec.Agents {
		verify = append(verify, agentVerify(a.Name, skillNames)...)
	}
	return out, verify, nil
}

// shellRun is one verify shell of verifyMachine. It keeps only the report row, not the output.
// For a connection error it keeps only the error, and the report makes the row from it.
type shellRun struct {
	row output.VerifyResult
	err error
	ran bool
}

// verifyRow is the report row of one verify shell.
func verifyRow(v VerifySpec, shell string, res transport.Result, err error) output.VerifyResult {
	vr := output.VerifyResult{Bundle: v.Bundle, Name: v.Name, Event: v.Event, Shell: shell, Code: res.Code}
	switch {
	case err != nil:
		vr.Status = "failed"
		vr.Code = -1
		vr.Output = err.Error()
	case res.Code == 0:
		vr.Status = "passed"
	default:
		vr.Status = "failed"
		vr.Output = tail(strings.TrimSpace(string(res.Stdout)+"\n"+string(res.Stderr)), 5)
	}
	return vr
}

// runVerify runs one verify script in a new login shell.
func runVerify(ctx context.Context, env *Env, v VerifySpec, interactive bool) (transport.Result, error) {
	return env.T.Run(ctx, transport.Cmd{Script: v.Script, LoginShell: true, Interactive: interactive, Timeout: 5 * time.Minute, Label: "verify " + v.Name})
}

// gateUpdates runs, before the apply loop, the verify scripts of each bundle and agent with a
// package update. When all these scripts pass in both shells, the tools of the group work, and
// FAM skips the updates of its packages: an update can take minutes (for example a Cargo build).
// A group without such scripts also skips its updates. A failed script lets the updates run.
// Other tasks of the group still apply their changes. `fam apply --update` runs all updates.
// It returns only a connection error.
func gateUpdates(ctx context.Context, mr *machineRun) error {
	pkgs := map[string][]*PackageTask{}
	for _, s := range mr.sects {
		if s.SkipReason != "" {
			continue
		}
		for _, t := range s.Tasks {
			if pt, ok := t.(*PackageTask); ok && pt.Updates() {
				pkgs[pt.Group()] = append(pkgs[pt.Group()], pt)
			}
		}
	}
	if len(pkgs) == 0 {
		return nil
	}
	var scripts []VerifySpec
	verified := map[string]bool{}
	for _, v := range mr.verify {
		if v.Gate && v.Event == "fresh_shell" && len(pkgs[v.Group]) > 0 {
			scripts = append(scripts, v)
			verified[v.Group] = true
		}
	}
	failed := map[string]bool{}
	if len(scripts) > 0 {
		mr.p.Count("verifying before updates", len(scripts))
		var mu sync.Mutex
		var connErr error
		var wg sync.WaitGroup
		sem := make(chan struct{}, parallelCommands)
		rp := newRunning(mr.p)
		for i, v := range scripts {
			sem <- struct{}{}
			mu.Lock()
			stop := connErr != nil
			mu.Unlock()
			if stop {
				<-sem
				break
			}
			wg.Add(1)
			rp.begin(i, "verify "+v.Name)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				defer rp.end(i)
				// The two shells run one after the other, as in verifyMachine.
				for _, interactive := range []bool{false, true} {
					res, err := runVerify(ctx, mr.env, v, interactive)
					if err != nil || res.Code != 0 {
						mu.Lock()
						failed[v.Group] = true
						if transport.IsConnError(err) && connErr == nil {
							connErr = err
						}
						mu.Unlock()
						return
					}
				}
			}()
		}
		wg.Wait()
		if connErr != nil {
			return connErr
		}
	}
	for g, list := range pkgs {
		if failed[g] {
			continue
		}
		detail := "update skipped (no verify script; --update runs it)"
		if verified[g] {
			detail = "verify passed, update skipped"
		}
		for _, pt := range list {
			pt.SkipUpdate = detail
		}
	}
	return nil
}

// verifyShells are the two shells of each fresh_shell script.
var verifyShells = [2]string{"noninteractive", "interactive"}

// verifyMachine runs the verify scripts in new shells.
//   - fresh_shell: a noninteractive and an interactive login shell of the user. These scripts
//     only prove that the tools work, so they run at the same time. FAM reports them in the
//     order of the config.
//   - reconnect: FAM closes the shared SSH connection first, then runs the script in a new one.
//   - reboot: not in version 0.
func verifyMachine(ctx context.Context, mr *machineRun) {
	r := mr.res
	env := mr.env
	addRow := func(vr output.VerifyResult) {
		if vr.Status == "passed" {
			r.Counts.VerifyOK++
		} else {
			r.Counts.VerifyBad++
		}
		r.Verify = append(r.Verify, vr)
	}
	add := func(v VerifySpec, shell string, res transport.Result, err error) {
		addRow(verifyRow(v, shell, res, err))
	}
	run := func(ctx context.Context, v VerifySpec, interactive bool) (transport.Result, error) {
		return runVerify(ctx, env, v, interactive)
	}
	runnable := 0
	for _, v := range mr.verify {
		if v.Event == "fresh_shell" || v.Event == "reconnect" {
			runnable++
		}
	}
	mr.p.Count("verifying", runnable)

	// The fresh_shell runs. Different scripts run at the same time. The two shells of one script
	// run one after the other, so that a script that uses a fixed port or file does not meet a
	// copy of itself. After a connection error of the script at position j, FAM starts no script
	// after j, and no second shell of a script after j: the report ends at j, as when the scripts
	// ran one after the other. Scripts before j run both shells.
	runs := make([][2]shellRun, len(mr.verify))
	{
		var mu sync.Mutex
		var stopAt atomic.Int64
		stopAt.Store(math.MaxInt64)
		var wg sync.WaitGroup
		sem := make(chan struct{}, parallelCommands)
		rp := newRunning(mr.p)
		for i, v := range mr.verify {
			if v.Event != "fresh_shell" {
				continue
			}
			sem <- struct{}{}
			if int64(i) > stopAt.Load() {
				<-sem
				break
			}
			wg.Add(1)
			rp.begin(i, "verify "+v.Name)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				defer rp.end(i)
				for k, interactive := range []bool{false, true} {
					if k > 0 && int64(i) > stopAt.Load() {
						return
					}
					res, err := run(ctx, v, interactive)
					mu.Lock()
					conn := transport.IsConnError(err)
					if conn {
						runs[i][k] = shellRun{err: err, ran: true}
					} else {
						runs[i][k] = shellRun{row: verifyRow(v, verifyShells[k], res, err), ran: true}
					}
					if conn && int64(i) < stopAt.Load() {
						stopAt.Store(int64(i))
					}
					mu.Unlock()
					if conn {
						return
					}
				}
			}()
		}
		wg.Wait()
	}

	// Report the shells in config order, as if they ran one after the other: up to and
	// including the first shell, in config order, that lost the connection. A shell that did not
	// start after a connection error has no row.
	var reconnect []VerifySpec
	for i, v := range mr.verify {
		switch v.Event {
		case "fresh_shell":
			for k := range verifyShells {
				sr := runs[i][k]
				if !sr.ran {
					continue
				}
				if transport.IsConnError(sr.err) {
					addRow(verifyRow(v, verifyShells[k], transport.Result{}, sr.err))
					setError(r, sr.err)
					return
				}
				addRow(sr.row)
			}
		case "reconnect":
			reconnect = append(reconnect, v)
		case "reboot":
			r.Verify = append(r.Verify, output.VerifyResult{Bundle: v.Bundle, Name: v.Name, Event: v.Event, Shell: "-", Status: "skipped", Output: "reboot verification is not in FAM version 0"})
		}
	}
	if len(reconnect) > 0 {
		mr.p.Stage("reconnecting to verify")
		if err := env.T.Reconnect(ctx); err != nil {
			setError(r, err)
			return
		}
		done := runnable - len(reconnect)
		for _, v := range reconnect {
			done++
			mr.p.Task(done, "verify "+v.Name)
			res, err := run(ctx, v, false)
			add(v, "new connection", res, err)
			if transport.IsConnError(err) {
				setError(r, err)
				return
			}
		}
	}
	if r.Counts.VerifyBad > 0 && r.Status != "error" {
		r.Status = "failed"
		r.ErrorKind = "verify"
		r.Error = "a verify script failed"
	}
}
