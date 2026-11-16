// Code generated from Pkl module `fam.Playbook`. DO NOT EDIT.
package playbook

import (
	"context"

	"github.com/apple/pkl-go/pkl"
	"github.com/gregor-tokarev/fam/internal/pklgen/base/role"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks"
)

// A playbook: tasks for a group of machines.
//
// ```
// amends "../.fam/Playbook.pkl"
// import "../.fam/tasks.pkl" as fam
// import "../.fam/facts.pkl"
//
// roles { "worker" }
//
//	tasks {
//	  when (facts.os == "linux") { new fam.Linger {} }
//	}
//
// ```
//
// List each playbook in the `playbooks` of `config.pkl`.
type Playbook struct {
	// The location of this playbook file.
	Source string `pkl:"source"`

	// The name of the playbook in output. The default is the file name.
	Name string `pkl:"name"`

	// A short description for output.
	Description *string `pkl:"description"`

	// The playbook runs on all machines with one of these roles.
	Roles []role.Role `pkl:"roles"`

	// The playbook also runs on these machines. Each name must be a key of `machines`.
	Machines []string `pkl:"machines"`

	// The tasks, in order.
	Tasks []tasks.Task `pkl:"tasks"`
}

// LoadFromPath loads the pkl module at the given path and evaluates it into a Playbook
func LoadFromPath(ctx context.Context, path string) (ret Playbook, err error) {
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

// Load loads the pkl module at the given source and evaluates it with the given evaluator into a Playbook
func Load(ctx context.Context, evaluator pkl.Evaluator, source *pkl.ModuleSource) (Playbook, error) {
	var ret Playbook
	err := evaluator.EvaluateModule(ctx, source, &ret)
	return ret, err
}
