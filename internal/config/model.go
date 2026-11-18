package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gregor-tokarev/fam/internal/pklgen/base"
	"github.com/gregor-tokarev/fam/internal/pklgen/bundle"
	"github.com/gregor-tokarev/fam/internal/pklgen/config"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks"
)

// Machine is one entry of the machines mapping.
type Machine struct {
	Name    string   `json:"name"`
	Cockpit bool     `json:"cockpit,omitempty"`
	Tags    []string `json:"tags"`
	Address string   `json:"address,omitempty"`
	User    string   `json:"user,omitempty"`
}

// Target returns the SSH target `user@address`.
func (m Machine) Target() string {
	if m.User != "" {
		return m.User + "@" + m.Address
	}
	return m.Address
}

// IsCockpit reports whether FAM runs the commands of this machine locally.
func (m Machine) IsCockpit() bool { return m.Cockpit }

// BundleUse is one bundle on one machine.
type BundleUse struct {
	Bundle bundle.Bundle
	// State is "present" or "absent".
	State string
	// Origin is "config" for a bundle of every machine, "tag <name>" for a bundle of a tag, and
	// "machine" for a machine entry.
	Origin string
	// Dir is the directory of the bundle file. FAM resolves bundle paths relative to it.
	Dir string
}

// Skipped reports whether a present bundle does not support this machine.
func (u BundleUse) Skipped() bool { return u.State == "present" && !u.Bundle.Supported }

// SkipReason returns the explanation for an unsupported bundle.
func (u BundleUse) SkipReason() string {
	if reason := u.Bundle.UnsupportedReason; reason != nil && strings.TrimSpace(*reason) != "" {
		return *reason
	}
	return "the bundle does not support this machine"
}

// Spec is the evaluated config for one machine.
type Spec struct {
	Machine Machine
	Bundles []BundleUse
	Agents  []AgentSpec
	Secrets *base.Secrets
}

// AgentSpec is one agent entry.
type AgentSpec struct {
	Name  string
	Agent base.Agent
}

// Machines returns the machines of the config, sorted by name, and validates them.
func Machines(cfg *config.Config) ([]Machine, error) {
	var out []Machine
	cockpits := 0
	for name, m := range cfg.Machines {
		mm := Machine{Name: name, Cockpit: m.Cockpit, Tags: append([]string{}, m.Tags...)}
		if m.Address != nil {
			mm.Address = *m.Address
		}
		if m.User != nil {
			mm.User = *m.User
		}
		if mm.IsCockpit() {
			cockpits++
		}
		out = append(out, mm)
	}
	if cockpits > 1 {
		return nil, fmt.Errorf("the config has %d machines with `cockpit = true`. Only one machine can be the cockpit", cockpits)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func bundleEntry(entry any, origin string) (BundleUse, error) {
	var u BundleUse
	switch b := entry.(type) {
	case bundle.Bundle:
		u = BundleUse{Bundle: b, State: "present", Origin: origin}
	case *bundle.Bundle:
		u = BundleUse{Bundle: *b, State: "present", Origin: origin}
	case base.BundleRef:
		u = BundleUse{Bundle: b.Bundle, State: string(b.State), Origin: origin}
	case *base.BundleRef:
		u = BundleUse{Bundle: b.Bundle, State: string(b.State), Origin: origin}
	default:
		return u, fmt.Errorf("a bundles entry has the type %T. Use a bundle module or `new { bundle = …; state = … }`", entry)
	}
	// A bundle of a package (the standard bundles) has no folder: it cannot use local files.
	if !strings.HasPrefix(u.Bundle.Source, "file:") {
		return u, nil
	}
	p, err := FileFromURI(u.Bundle.Source)
	if err != nil {
		return u, err
	}
	u.Dir = filepath.Dir(p)
	return u, nil
}

// Validate checks the rules that Pkl cannot check: bundle identity and the scripts.
func Validate(cfg *config.Config) error { return errors.Join(ValidationErrors(cfg)...) }

// ValidationErrors returns independent errors across all bundle scopes and machines.
func ValidationErrors(cfg *config.Config) []error {
	var problems []error
	add := func(err error) {
		if err != nil {
			problems = append(problems, err)
		}
	}
	if _, err := Machines(cfg); err != nil {
		add(err)
	}
	if len(cfg.Machines) == 0 {
		add(fmt.Errorf("the config has no machines"))
	}
	names := make([]string, 0, len(cfg.Machines))
	for name := range cfg.Machines {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, tag := range cfg.Machines[name].Tags {
			if _, ok := cfg.Tags[tag]; !ok {
				add(fmt.Errorf("machine %s has the tag %q, but `tags` in the config does not define it. Add `[%q] {}` to `tags` when the tag has no bundles", name, tag, tag))
			}
		}
	}
	sources := map[string]string{}
	check := func(entries []any, where string) {
		seen := map[string]bool{}
		for _, e := range entries {
			u, err := bundleEntry(e, where)
			if err != nil {
				add(err)
				continue
			}
			name := u.Bundle.Name
			if prev, ok := sources[name]; ok && prev != u.Bundle.Source {
				add(fmt.Errorf("two different files use the bundle name %q: %s and %s. A bundle name identifies one bundle", name, prev, u.Bundle.Source))
			}
			sources[name] = u.Bundle.Source
			if seen[name] {
				add(fmt.Errorf("the bundle %q is listed two times in %s", name, where))
			}
			seen[name] = true
			if err := validateTasks(u.Bundle.Tasks, "bundle "+name); err != nil {
				add(err)
			}
			if err := validateTasks(u.Bundle.Uninstall, "bundle "+name+" (uninstall)"); err != nil {
				add(err)
			}
		}
	}
	check(cfg.Bundles, "the bundles of config.pkl")
	for _, n := range names {
		check(cfg.Machines[n].Bundles, "the bundles of machine "+n)
	}
	tags := make([]string, 0, len(cfg.Tags))
	for t := range cfg.Tags {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	for _, t := range tags {
		check(cfg.Tags[t].Bundles, "the bundles of tag "+t)
	}
	return problems
}

func validateTasks(ts []tasks.Task, where string) error {
	for i, t := range ts {
		if s, ok := t.(tasks.Script); ok {
			if s.GetCheck() == nil && s.GetCreates() == nil {
				return fmt.Errorf("%s: script %q (task %d) has no check. Set `check` or `creates`", where, s.GetName(), i+1)
			}
		}
	}
	return nil
}

// Resolve returns the config for one machine.
func Resolve(cfg *config.Config, name string) (*Spec, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	m, ok := cfg.Machines[name]
	if !ok {
		return nil, fmt.Errorf("the config for machine %q has no machine %q", name, name)
	}
	ms, _ := Machines(cfg)
	spec := &Spec{Secrets: cfg.Secrets}
	for _, mm := range ms {
		if mm.Name == name {
			spec.Machine = mm
		}
	}
	index := map[string]int{}
	for _, e := range cfg.Bundles {
		u, err := bundleEntry(e, "config")
		if err != nil {
			return nil, err
		}
		index[u.Bundle.Name] = len(spec.Bundles)
		spec.Bundles = append(spec.Bundles, u)
	}
	// The bundles of the tags come after the bundles of every machine, in the order of the tags of
	// the machine, and the machine entries last. An entry wins over the same bundle earlier in this
	// order, at its earlier position.
	add := func(entries []any, origin string) error {
		for _, e := range entries {
			u, err := bundleEntry(e, origin)
			if err != nil {
				return err
			}
			if i, ok := index[u.Bundle.Name]; ok {
				spec.Bundles[i] = u
				continue
			}
			index[u.Bundle.Name] = len(spec.Bundles)
			spec.Bundles = append(spec.Bundles, u)
		}
		return nil
	}
	for _, tag := range m.Tags {
		if err := add(cfg.Tags[tag].Bundles, "tag "+tag); err != nil {
			return nil, err
		}
	}
	if err := add(m.Bundles, "machine"); err != nil {
		return nil, err
	}
	agentNames := make([]string, 0, len(cfg.Agents))
	for n := range cfg.Agents {
		agentNames = append(agentNames, string(n))
	}
	sort.Strings(agentNames)
	for _, n := range agentNames {
		for k, a := range cfg.Agents {
			if string(k) == n {
				spec.Agents = append(spec.Agents, AgentSpec{Name: n, Agent: a})
			}
		}
	}
	return spec, nil
}
