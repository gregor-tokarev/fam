package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
	if !staticOnly {
		prefetchSkills(ctx, spec.Bundles)
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
			verify = append(verify, VerifySpec{Bundle: u.Bundle.Name, Name: name, Event: string(v.Event), Script: v.Script})
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

// verifyMachine runs the verify scripts in new shells.
//   - fresh_shell: a noninteractive and an interactive login shell of the user. These scripts
//     only prove that the tools work, so they run at the same time. FAM reports them in the
//     order of the config.
//   - reconnect: FAM closes the shared SSH connection first, then runs the script in a new one.
//   - reboot: not in version 0.
func verifyMachine(ctx context.Context, mr *machineRun) {
	r := mr.res
	env := mr.env
	add := func(v VerifySpec, shell string, res transport.Result, err error) {
		vr := output.VerifyResult{Bundle: v.Bundle, Name: v.Name, Event: v.Event, Shell: shell, Code: res.Code}
		out := strings.TrimSpace(string(res.Stdout) + "\n" + string(res.Stderr))
		switch {
		case err != nil:
			vr.Status = "failed"
			vr.Code = -1
			vr.Output = err.Error()
		case res.Code == 0:
			vr.Status = "passed"
		default:
			vr.Status = "failed"
			vr.Output = tail(out, 5)
		}
		if vr.Status == "passed" {
			r.Counts.VerifyOK++
		} else {
			r.Counts.VerifyBad++
		}
		r.Verify = append(r.Verify, vr)
	}
	run := func(ctx context.Context, v VerifySpec, interactive bool) (transport.Result, error) {
		return env.T.Run(ctx, transport.Cmd{Script: v.Script, LoginShell: true, Interactive: interactive, Timeout: 5 * time.Minute, Label: "verify " + v.Name})
	}
	runnable := 0
	for _, v := range mr.verify {
		if v.Event == "fresh_shell" || v.Event == "reconnect" {
			runnable++
		}
	}
	mr.p.Count("verifying", runnable)

	// The fresh_shell runs: two shells for each script, at the same time.
	type shellRun struct {
		res transport.Result
		err error
		ran bool
	}
	runs := make([][2]shellRun, len(mr.verify))
	var connErr error
	{
		cctx, cancel := context.WithCancel(ctx)
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, parallelCommands)
		left := make([]int, len(mr.verify))
		done := 0
		for i, v := range mr.verify {
			if v.Event != "fresh_shell" {
				continue
			}
			left[i] = 2
			for k, interactive := range []bool{false, true} {
				sem <- struct{}{}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					// A stop after a connection error of another shell: no result.
					stopped := func() bool { return cctx.Err() != nil && ctx.Err() == nil }
					if stopped() {
						return
					}
					res, err := run(cctx, v, interactive)
					conn := transport.IsConnError(err)
					if stopped() && !conn {
						return
					}
					mu.Lock()
					defer mu.Unlock()
					runs[i][k] = shellRun{res: res, err: err, ran: true}
					if conn && connErr == nil {
						connErr = err
						cancel()
					}
					if left[i]--; left[i] == 0 {
						done++
						mr.p.Task(done, "verify "+v.Name)
					}
				}()
			}
		}
		wg.Wait()
		cancel()
	}

	var reconnect []VerifySpec
	for i, v := range mr.verify {
		switch v.Event {
		case "fresh_shell":
			for k, shell := range []string{"noninteractive", "interactive"} {
				sr := runs[i][k]
				if !sr.ran {
					setError(r, connErr)
					return
				}
				add(v, shell, sr.res, sr.err)
				if transport.IsConnError(sr.err) {
					setError(r, sr.err)
					return
				}
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
