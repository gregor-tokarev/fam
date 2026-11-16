// Code generated from Pkl module `fam.base`. DO NOT EDIT.
package base

import (
	"context"

	"github.com/apple/pkl-go/pkl"
)

// Shared types of the FAM schema.
type Base struct {
}

// LoadFromPath loads the pkl module at the given path and evaluates it into a Base
func LoadFromPath(ctx context.Context, path string) (ret Base, err error) {
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

// Load loads the pkl module at the given source and evaluates it with the given evaluator into a Base
func Load(ctx context.Context, evaluator pkl.Evaluator, source *pkl.ModuleSource) (Base, error) {
	var ret Base
	err := evaluator.EvaluateModule(ctx, source, &ret)
	return ret, err
}
