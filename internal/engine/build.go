package engine

import (
	"context"
	"fmt"
	"regexp"
	"sync"

	"github.com/gregor-tokarev/fam/internal/config"
	"github.com/gregor-tokarev/fam/internal/facts"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks"
)

// Section is the tasks of one group, in run order.
type Section struct {
	// Name is the group name, for example "bundle worker-setup".
	Name  string
	Tasks []Task
	// RestartUnits marks the sections that restart units at their end (present bundles).
	RestartUnits bool
	// SkipReason marks an unsupported bundle. Its tasks do not run.
	SkipReason string
}

// convert turns a schema task into an engine task.
func convert(t tasks.Task, group string, absent bool, bundleManager string, f facts.Facts) (Task, error) {
	b := base{group: group, sudo: t.GetSudo()}
	units := make([]Unit, 0, len(t.GetRestarts()))
	for _, u := range t.GetRestarts() {
		scope := "user"
		if t.GetSudo() {
			scope = "system"
		}
		units = append(units, Unit{Name: u, Scope: scope})
	}
	isAbsent := func(s string) bool { return absent || s == "absent" }
	switch x := t.(type) {
	case tasks.File:
		ft := &FileTask{base: b, Path: x.GetPath(), Mode: parseMode(x.GetMode(), 0o644), Absent: isAbsent(string(x.GetState())), units: units}
		if c := x.GetContent(); c != nil {
			ft.Content = []byte(*c)
		} else if !ft.Absent {
			return nil, fmt.Errorf("%s: file %s has no content", group, x.GetPath())
		}
		return ft, nil
	case tasks.Merge:
		v, err := fromPkl(x.GetValues())
		if err != nil {
			return nil, fmt.Errorf("%s: merge %s: %w", group, x.GetPath(), err)
		}
		m, ok := v.(*omap)
		if !ok {
			return nil, fmt.Errorf("%s: merge %s: values must be an object", group, x.GetPath())
		}
		return &MergeTask{base: b, Path: x.GetPath(), Format: x.GetFormat(), Values: m, Mode: parseMode(x.GetMode(), 0o644), Absent: isAbsent(string(x.GetState())), units: units}, nil
	case tasks.Symlink:
		return &SymlinkTask{base: b, Path: x.GetPath(), Target: x.GetTarget(), Absent: isAbsent(string(x.GetState())), units: units}, nil
	case tasks.Service:
		return &ServiceTask{base: b, Unit: x.GetName(), Scope: x.GetScope(), Content: x.GetContent(), Enabled: x.GetEnabled(), Running: x.GetRunning(), Absent: isAbsent(string(x.GetState())), units: units}, nil
	case tasks.Linger:
		lt := &LingerTask{base: b, Absent: isAbsent(string(x.GetState())), units: units}
		if u := x.GetUser(); u != nil {
			lt.User = *u
		}
		return lt, nil
	case tasks.Package:
		pt := &PackageTask{base: b, Absent: isAbsent(string(x.GetState())), units: units}
		if v := x.GetBinary(); v != nil {
			pt.Binary = *v
		}
		if p := x.GetPackage(); p != nil && *p != nil {
			name, mgr, err := PackageSpec(*p, bundleManager, f)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", group, err)
			}
			if err := managedPackage(pt, name, mgr); err != nil {
				return nil, fmt.Errorf("%s: %w", group, err)
			}
		}
		// Own commands win over the commands of the package manager.
		if v := x.GetInstall(); v != nil {
			pt.Install = *v
		}
		if v := x.GetUpdate(); v != nil {
			pt.UpdateSh = *v
		}
		if v := x.GetUninstall(); v != nil {
			pt.Uninstall = *v
		}
		if pt.Install == "" {
			return nil, fmt.Errorf("%s: a package needs `package` or an `install` command", group)
		}
		if pt.Binary == "" {
			return nil, fmt.Errorf("%s: a package without `package` needs `binary`", group)
		}
		pt.VersionCmd = pt.Binary + " --version"
		if v := x.GetVersionCommand(); v != nil {
			pt.VersionCmd = *v
		}
		return pt, nil
	case tasks.GitHubRelease:
		if t.GetSudo() {
			return nil, fmt.Errorf("%s: GitHubRelease runs as the SSH user. Remove sudo", group)
		}
		return &GitHubReleaseTask{base: b, Repo: x.GetRepo(), Binary: x.GetBinary(), Assets: x.GetAssets(), Tag: x.GetTag(), VersionCommand: x.GetVersionCommand(), Absent: isAbsent(string(x.GetState())), units: units}, nil
	case tasks.Script:
		if absent {
			return nil, nil // FAM cannot undo a script.
		}
		st := &ScriptTask{base: b, Title: x.GetName(), ApplySh: x.GetApply(), units: units}
		if c := x.GetCheck(); c != nil {
			st.CheckSh = *c
		} else if c := x.GetCreates(); c != nil {
			st.Creates = *c
		} else {
			return nil, fmt.Errorf("%s: script %q has no check. Set `check` or `creates`", group, x.GetName())
		}
		return st, nil
	}
	return nil, fmt.Errorf("%s: unknown task type %T", group, t)
}

// bundleSkillName resolves a skill name without reading or downloading its source.
func bundleSkillName(t tasks.Task, u config.BundleUse) (string, bool) {
	x, ok := t.(tasks.AgentSkill)
	if !ok {
		return "", false
	}
	if n := x.GetName(); n != nil {
		return *n, true
	}
	spec, err := parseSkillSource(x, u.Dir, u.Bundle.Name)
	if err != nil {
		return "", true
	}
	return spec.name, true
}

// skillTasks turns an AgentSkill task into engine tasks: the copy and the Claude link, or the
// removal when the task or the bundle is absent. ok is false for other task types.
func skillTasks(ctx context.Context, t tasks.Task, u config.BundleUse, group string, absent bool, staticOnly bool) (out []Task, ok bool, err error) {
	x, ok := t.(tasks.AgentSkill)
	if !ok {
		return nil, false, nil
	}
	g := base{group: group}
	origin := "bundle " + u.Bundle.Name
	absent = absent || x.GetState() == "absent"
	spec, err := parseSkillSource(x, u.Dir, u.Bundle.Name)
	if err != nil {
		return nil, true, fmt.Errorf("%s: %w", group, err)
	}
	name := spec.name
	if n := x.GetName(); n != nil {
		name = *n
	}
	// Skills live in the home folder of the SSH user, so FAM never copies them with sudo.
	if t.GetSudo() {
		return nil, true, fmt.Errorf("%s: skill %s: skill tasks run as the SSH user. Remove `sudo = true`", group, name)
	}
	if !skillName.MatchString(name) {
		return nil, true, fmt.Errorf("%s: the skill name %q is not valid: use letters, digits, '.', '_', and '-', and start with a letter or digit. Set `name`", group, name)
	}
	units := make([]Unit, 0, len(t.GetRestarts()))
	for _, n := range t.GetRestarts() {
		units = append(units, Unit{Name: n, Scope: "user"})
	}
	if absent {
		return []Task{&SkillRemoveTask{base: g, Name_: name, units: units}}, true, nil
	}
	var src *SkillSource
	if staticOnly && spec.kind != skillLocal {
		// fam check does not download skills.
		src = &SkillSource{Name: name, Dir: spec.String(), Origin: origin}
	} else if src, err = spec.load(ctx, name, origin); err != nil {
		return nil, true, err
	}
	return []Task{&SkillTask{base: g, Skill: src, units: units}, &SkillLinkTask{base: g, Name_: name}}, true, nil
}

// prefetchSlots allows at most 4 skill downloads at a time for the whole run (all machines), so
// that a server with a request limit does not refuse them.
var prefetchSlots = make(chan struct{}, 4)

// prefetchSkills downloads the remote skills of the present bundles, several at a time, before
// FAM builds the tasks one bundle after the other. Each download goes into the cache of the
// run, so the tasks get the same result, also an error, without a new request.
func prefetchSkills(ctx context.Context, bundles []config.BundleUse) {
	var wg sync.WaitGroup
	for _, u := range bundles {
		if u.State != "present" || u.Skipped() {
			continue
		}
		for _, t := range u.Bundle.Tasks {
			x, ok := t.(tasks.AgentSkill)
			if !ok || x.GetState() == "absent" {
				continue
			}
			spec, err := parseSkillSource(x, u.Dir, u.Bundle.Name)
			if err != nil || spec.kind == skillLocal {
				continue
			}
			wg.Add(1)
			prefetchSlots <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-prefetchSlots }()
				_, _ = spec.load(ctx, spec.name, "bundle "+u.Bundle.Name)
			}()
		}
	}
	wg.Wait()
}

// skillName is the rule for skill names, as in tasks.pkl.
var skillName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// BundleSection returns the tasks of one bundle. An absent bundle undoes its typed tasks
// in reverse order, then runs its uninstall tasks. A present bundle restarts the units of its
// changed tasks once, at its end. A restart of a unit that a Service task of the same bundle
// manages uses the scope of that task.
func BundleSection(ctx context.Context, u config.BundleUse, f facts.Facts) (Section, error) {
	return bundleSection(ctx, u, f, false)
}

func bundleSection(ctx context.Context, u config.BundleUse, f facts.Facts, staticOnly bool) (Section, error) {
	bundleManager := ""
	if u.Bundle.Manager != nil {
		bundleManager = string(*u.Bundle.Manager)
	}
	group := "bundle " + u.Bundle.Name
	sec := Section{Name: group}
	if u.Skipped() {
		sec.SkipReason = u.SkipReason()
		return sec, nil
	}
	if u.State == "absent" {
		for i := len(u.Bundle.Tasks) - 1; i >= 0; i-- {
			if st, ok, err := skillTasks(ctx, u.Bundle.Tasks[i], u, group, true, staticOnly); ok || err != nil {
				if err != nil {
					return sec, err
				}
				sec.Tasks = append(sec.Tasks, st...)
				continue
			}
			t, err := convert(u.Bundle.Tasks[i], group, true, bundleManager, f)
			if err != nil {
				return sec, err
			}
			if t != nil {
				sec.Tasks = append(sec.Tasks, t)
			}
		}
		for _, ut := range u.Bundle.Uninstall {
			if st, ok, err := skillTasks(ctx, ut, u, group, false, staticOnly); ok || err != nil {
				if err != nil {
					return sec, err
				}
				sec.Tasks = append(sec.Tasks, st...)
				continue
			}
			t, err := convert(ut, group, false, bundleManager, f)
			if err != nil {
				return sec, err
			}
			sec.Tasks = append(sec.Tasks, t)
		}
		return sec, nil
	}
	sec.RestartUnits = true
	// The scope of each unit that a Service task of this bundle manages. A name with a user and
	// a system Service task is ambiguous: its restarts keep the scope from the `sudo` of the task.
	scopes := map[string]string{}
	ambiguous := map[string]bool{}
	for _, t := range u.Bundle.Tasks {
		if s, ok := t.(tasks.Service); ok {
			if prev, ok := scopes[s.GetName()]; ok && prev != s.GetScope() {
				ambiguous[s.GetName()] = true
			}
			scopes[s.GetName()] = s.GetScope()
		}
	}
	for n := range ambiguous {
		delete(scopes, n)
	}
	for _, bt := range u.Bundle.Tasks {
		ts, ok, err := skillTasks(ctx, bt, u, group, false, staticOnly)
		if err != nil {
			return sec, err
		}
		if !ok {
			t, err := convert(bt, group, false, bundleManager, f)
			if err != nil {
				return sec, err
			}
			ts = []Task{t}
		}
		for _, t := range ts {
			if r, ok := t.(Restarter); ok {
				for i, unit := range r.Restarts() {
					if s, ok := scopes[unit.Name]; ok {
						r.Restarts()[i].Scope = s
					}
				}
			}
		}
		sec.Tasks = append(sec.Tasks, ts...)
	}
	return sec, nil
}
