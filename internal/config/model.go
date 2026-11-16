package config

import (
	"fmt"
	"path/filepath"
	"sort"

	"github.com/gregor-tokarev/fam/internal/pklgen/base"
	"github.com/gregor-tokarev/fam/internal/pklgen/bundle"
	"github.com/gregor-tokarev/fam/internal/pklgen/config"
	"github.com/gregor-tokarev/fam/internal/pklgen/playbook"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks"
)

// Machine is one entry of the machines mapping.
type Machine struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Address string `json:"address,omitempty"`
	User    string `json:"user,omitempty"`
}

// Target returns the SSH target `user@address`.
func (m Machine) Target() string {
	if m.User != "" {
		return m.User + "@" + m.Address
	}
	return m.Address
}

// IsCockpit reports whether FAM runs the commands of this machine locally.
func (m Machine) IsCockpit() bool { return m.Role == "cockpit" }

// BundleUse is one bundle on one machine.
type BundleUse struct {
	Bundle bundle.Bundle
	// State is "present" or "absent".
	State string
	// Origin is "config" for a global bundle and "machine" for a machine entry.
	Origin string
	// Dir is the directory of the bundle file. FAM resolves bundle paths relative to it.
	Dir string
}

// Spec is the evaluated config for one machine.
type Spec struct {
	Machine Machine
	// PackageManagers maps a package name to its package manager: config.pkl, then the machine.
	PackageManagers map[string]string
	Bundles         []BundleUse
	Agents          []AgentSpec
	Secrets         *base.Secrets
	Playbooks       []playbook.Playbook
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
		mm := Machine{Name: name, Role: string(m.Role)}
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
		return nil, fmt.Errorf("the config has %d machines with role \"cockpit\". Only one machine can be the cockpit", cockpits)
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
	p, err := FileFromURI(u.Bundle.Source)
	if err != nil {
		return u, err
	}
	u.Dir = filepath.Dir(p)
	return u, nil
}

// Validate checks the rules that Pkl cannot check: bundle identity and playbook machines.
func Validate(cfg *config.Config) error {
	if _, err := Machines(cfg); err != nil {
		return err
	}
	sources := map[string]string{}
	check := func(entries []any, where string) error {
		seen := map[string]bool{}
		for _, e := range entries {
			u, err := bundleEntry(e, where)
			if err != nil {
				return err
			}
			name := u.Bundle.Name
			if prev, ok := sources[name]; ok && prev != u.Bundle.Source {
				return fmt.Errorf("two different files use the bundle name %q: %s and %s. A bundle name identifies one bundle", name, prev, u.Bundle.Source)
			}
			sources[name] = u.Bundle.Source
			if seen[name] {
				return fmt.Errorf("the bundle %q is listed two times in %s", name, where)
			}
			seen[name] = true
			if err := validateTasks(u.Bundle.Tasks, "bundle "+name); err != nil {
				return err
			}
			if err := validateTasks(u.Bundle.Uninstall, "bundle "+name+" (uninstall)"); err != nil {
				return err
			}
		}
		return nil
	}
	if err := check(cfg.Bundles, "the bundles of config.pkl"); err != nil {
		return err
	}
	names := make([]string, 0, len(cfg.Machines))
	for n := range cfg.Machines {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := check(cfg.Machines[n].Bundles, "the bundles of machine "+n); err != nil {
			return err
		}
	}
	pbNames := map[string]string{}
	for _, p := range cfg.Playbooks {
		if prev, ok := pbNames[p.Name]; ok {
			return fmt.Errorf("two playbooks have the name %q: %s and %s", p.Name, prev, p.Source)
		}
		pbNames[p.Name] = p.Source
		for _, m := range p.Machines {
			if _, ok := cfg.Machines[m]; !ok {
				return fmt.Errorf("playbook %q selects the machine %q, which is not in the machines mapping", p.Name, m)
			}
		}
		if err := validateTasks(p.Tasks, "playbook "+p.Name); err != nil {
			return err
		}
	}
	return nil
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
	spec := &Spec{Secrets: cfg.Secrets, PackageManagers: map[string]string{}}
	for k, v := range cfg.PackageManagers {
		spec.PackageManagers[k] = string(v)
	}
	for k, v := range m.PackageManagers {
		spec.PackageManagers[k] = string(v)
	}
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
	// A machine entry wins over the same bundle in config.pkl.
	for _, e := range m.Bundles {
		u, err := bundleEntry(e, "machine")
		if err != nil {
			return nil, err
		}
		if i, ok := index[u.Bundle.Name]; ok {
			spec.Bundles[i] = u
			continue
		}
		index[u.Bundle.Name] = len(spec.Bundles)
		spec.Bundles = append(spec.Bundles, u)
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
	for _, p := range cfg.Playbooks {
		if selects(p, name, string(m.Role)) {
			spec.Playbooks = append(spec.Playbooks, p)
		}
	}
	return spec, nil
}

func selects(p playbook.Playbook, machine, role string) bool {
	for _, r := range p.Roles {
		if string(r) == role {
			return true
		}
	}
	for _, m := range p.Machines {
		if m == machine {
			return true
		}
	}
	return false
}
