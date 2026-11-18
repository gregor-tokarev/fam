// Code generated from Pkl module `fam.Bundle`. DO NOT EDIT.
package bundle

import (
	"context"

	"github.com/apple/pkl-go/pkl"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks/manager"
)

// A bundle: a reusable part of the config. Usually one tool or one piece of machine setup, with
// its tasks (also skills: [t.AgentSkill]), environment, and checks.
//
// `config.pkl` selects the machines: `bundles` (every machine), `tags` (every machine with a
// tag), or the `bundles` of one machine. A bundle itself does not know the machines.
//
// A bundle file amends this module:
//
// ```
// amends "@fam/Bundle.pkl"
// import "@fam/tasks.pkl" as fam
//
// name = "posthog"
//
//	tasks {
//	  new fam.Package { binary = "posthog-cli"; install = #"…"# }
//	}
//
// ```
//
// Bundles list secret names. Values come from the secrets file of the config.
type Bundle struct {
	// The identity of the bundle.
	//
	// Two different files with the same name are an error.
	Name string `pkl:"name"`

	// Whether the tool supports the current machine facts.
	// When false, FAM skips a present bundle and its verify scripts without an error.
	// A skipped bundle adds no variables or PATH entries to ~/.config/fam/env.sh.
	// FAM leaves files from an earlier installation on the machine and does not remove them.
	// An absent bundle ignores this field.
	Supported bool `pkl:"supported"`

	// Why the tool does not support the machine. FAM shows this when it skips the bundle.
	UnsupportedReason *string `pkl:"unsupportedReason"`

	// The version of the bundle. Standard bundles have the FAM version.
	Version *string `pkl:"version"`

	// The location of this bundle file. FAM resolves all paths of the bundle relative to it.
	//
	// FAM sets it. Do not set it.
	Source string `pkl:"source"`

	// The package manager of each `Package` task of this bundle, also for a task that selects its
	// own manager. When it is `null`, each task uses its own manager.
	//
	// Set it with [withManager].
	Manager *manager.Manager `pkl:"manager"`

	// Environment variables that FAM exports in every shell of the machine.
	Env map[string]string `pkl:"env"`

	// Names of environment variables that the tool needs at run time.
	// FAM warns when the secrets file of the config does not give a name.
	RequiredSecrets []string `pkl:"requiredSecrets"`

	// Directories that FAM adds to the start of `PATH` in every shell and in FAM scripts,
	// for example `"~/.cargo/bin"`. `~/` is the home directory.
	Path []string `pkl:"path"`

	// The install tasks, in order.
	Tasks []tasks.Task `pkl:"tasks"`

	// Checks that prove that the bundle works.
	Verify []Verify `pkl:"verify"`

	// Tasks that FAM runs after it undoes [tasks], when the bundle state is `"absent"`.
	//
	// Use them for steps that FAM cannot undo, for example a [t.Script].
	Uninstall []tasks.Task `pkl:"uninstall"`
}

// LoadFromPath loads the pkl module at the given path and evaluates it into a Bundle
func LoadFromPath(ctx context.Context, path string) (ret Bundle, err error) {
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

// Load loads the pkl module at the given source and evaluates it with the given evaluator into a Bundle
func Load(ctx context.Context, evaluator pkl.Evaluator, source *pkl.ModuleSource) (Bundle, error) {
	var ret Bundle
	err := evaluator.EvaluateModule(ctx, source, &ret)
	return ret, err
}
