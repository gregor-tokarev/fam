package engine

import (
	"context"
	"strings"
	"time"

	"github.com/gregor-tokarev/fam/internal/config"
	"github.com/gregor-tokarev/fam/internal/output"
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
// environment and secrets, agents, bundles (every machine, then the role, then the machine),
// skills, instructions.
func buildSections(env *Env, spec *config.Spec) ([]Section, []VerifySpec, error) {
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
		s, err := BundleSection(u, spec.PackageManagers)
		if err != nil {
			return nil, nil, err
		}
		if len(s.Tasks) > 0 {
			out = append(out, s)
		}
		if u.State != "present" {
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
	sk, err := skillsSection(env, spec)
	if err != nil {
		return nil, nil, err
	}
	var skillNames []string
	for _, t := range sk.Tasks {
		if st, ok := t.(*SkillTask); ok {
			skillNames = append(skillNames, st.Skill.Name)
		}
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
//   - fresh_shell: a noninteractive and an interactive login shell of the user.
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
	run := func(v VerifySpec, interactive bool) (transport.Result, error) {
		return env.T.Run(ctx, transport.Cmd{Script: v.Script, LoginShell: true, Interactive: interactive, Timeout: 5 * time.Minute, Label: "verify " + v.Name})
	}
	var reconnect []VerifySpec
	for _, v := range mr.verify {
		switch v.Event {
		case "fresh_shell":
			res, err := run(v, false)
			add(v, "noninteractive", res, err)
			if transport.IsConnError(err) {
				setError(r, err)
				return
			}
			res, err = run(v, true)
			add(v, "interactive", res, err)
			if transport.IsConnError(err) {
				setError(r, err)
				return
			}
		case "reconnect":
			reconnect = append(reconnect, v)
		case "reboot":
			r.Verify = append(r.Verify, output.VerifyResult{Bundle: v.Bundle, Name: v.Name, Event: v.Event, Shell: "-", Status: "skipped", Output: "reboot verification is not in FAM version 0"})
		}
	}
	if len(reconnect) > 0 {
		if err := env.T.Reconnect(ctx); err != nil {
			setError(r, err)
			return
		}
		for _, v := range reconnect {
			res, err := run(v, false)
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
