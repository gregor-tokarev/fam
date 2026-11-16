// Code generated from Pkl module `fam.Machines`. DO NOT EDIT.
package machines

import (
	"context"

	"github.com/apple/pkl-go/pkl"
	"github.com/gregor-tokarev/fam/internal/pklgen/base"
)

// The machines of the fleet, in a separate file.
//
// ```
// amends ".fam/Machines.pkl"
//
//	machines {
//	  ["cockpit"] { role = "cockpit" }
//	  ["worker1"] { role = "worker"; address = "worker1.example.com"; user = "dev" }
//	}
//
// ```
//
// `config.pkl` uses it with `machines = import("machines.pkl").machines`.
type Machines struct {
	// The machines of the fleet. The key is the machine name.
	Machines map[string]base.Machine `pkl:"machines"`
}

// LoadFromPath loads the pkl module at the given path and evaluates it into a Machines
func LoadFromPath(ctx context.Context, path string) (ret Machines, err error) {
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

// Load loads the pkl module at the given source and evaluates it with the given evaluator into a Machines
func Load(ctx context.Context, evaluator pkl.Evaluator, source *pkl.ModuleSource) (Machines, error) {
	var ret Machines
	err := evaluator.EvaluateModule(ctx, source, &ret)
	return ret, err
}
