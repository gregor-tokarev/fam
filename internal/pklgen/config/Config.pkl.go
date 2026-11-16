// Code generated from Pkl module `fam.Config`. DO NOT EDIT.
package config

import (
	"context"

	"github.com/apple/pkl-go/pkl"
	"github.com/gregor-tokarev/fam/internal/pklgen/base"
	"github.com/gregor-tokarev/fam/internal/pklgen/base/agentname"
	"github.com/gregor-tokarev/fam/internal/pklgen/playbook"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks/manager"
)

// The entrypoint of a FAM project.
//
// `config.pkl` amends this module:
//
// ```
// amends ".fam/Config.pkl"
// import ".fam/std.pkl"
//
// bundles { std.agentBrowser }
// machines = import("machines.pkl").machines
// agents = import("agents.pkl").agents
// playbooks { import("setup/worker.pkl") }
// ```
//
// FAM evaluates this module once for each machine, with the facts of that machine.
type Config struct {
	// Bundles for every machine.
	//
	// An entry is a bundle module, or a [base.BundleRef] with a state.
	// A plain module means `state = "present"`.
	Bundles []any `pkl:"bundles"`

	// The machines of the fleet. FAM changes only these machines.
	//
	// The key is the machine name.
	Machines map[string]base.Machine `pkl:"machines"`

	// The coding agents that FAM installs and configures on every machine.
	Agents map[agentname.AgentName]base.Agent `pkl:"agents"`

	// How FAM gives secrets to the machines. When it is `null`, FAM does not manage secrets.
	Secrets *base.Secrets `pkl:"secrets"`

	// All playbooks of the project.
	Playbooks []playbook.Playbook `pkl:"playbooks"`

	// The package manager for a package name, for every machine.
	//
	// It replaces the manager of each `Package` task with this package, also in the standard bundles:
	//
	// ```
	// packageManagers { ["agent-browser"] = "vite" }
	// ```
	//
	// A machine can set its own `packageManagers`, which win over these.
	PackageManagers map[string]manager.Manager `pkl:"packageManagers"`
}

// LoadFromPath loads the pkl module at the given path and evaluates it into a Config
func LoadFromPath(ctx context.Context, path string) (ret Config, err error) {
	evaluator, err := pkl.NewEvaluator(ctx, pkl.PreconfiguredOptions)
	if err != nil {
		return ret, err
	}
	defer func() {
		cerr := evaluator.Close()
		if err == nil {
			err = cerr
		}
	}()
	ret, err = Load(ctx, evaluator, pkl.FileSource(path))
	return ret, err
}

// Load loads the pkl module at the given source and evaluates it with the given evaluator into a Config
func Load(ctx context.Context, evaluator pkl.Evaluator, source *pkl.ModuleSource) (Config, error) {
	var ret Config
	err := evaluator.EvaluateModule(ctx, source, &ret)
	return ret, err
}
