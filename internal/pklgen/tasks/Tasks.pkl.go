// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

import (
	"context"

	"github.com/apple/pkl-go/pkl"
)

// The task types of FAM.
//
// The task types of bundles.
// Import this module with a different name, because `tasks` is also a property name:
//
// ```
// import "../.fam/tasks.pkl" as fam
// ```
//
// Paths that start with `~/` are relative to the home directory of the SSH user.
type Tasks struct {
}

// LoadFromPath loads the pkl module at the given path and evaluates it into a Tasks
func LoadFromPath(ctx context.Context, path string) (ret Tasks, err error) {
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

// Load loads the pkl module at the given source and evaluates it with the given evaluator into a Tasks
func Load(ctx context.Context, evaluator pkl.Evaluator, source *pkl.ModuleSource) (Tasks, error) {
	var ret Tasks
	err := evaluator.EvaluateModule(ctx, source, &ret)
	return ret, err
}
