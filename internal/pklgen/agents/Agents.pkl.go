// Code generated from Pkl module `fam.Agents`. DO NOT EDIT.
package agents

import (
	"context"

	"github.com/apple/pkl-go/pkl"
	"github.com/gregor-tokarev/fam/internal/pklgen/base"
	"github.com/gregor-tokarev/fam/internal/pklgen/base/agentname"
)

// The coding agents, in a separate file.
//
// ```
// amends "@fam/Agents.pkl"
//
//	agents {
//	  ["claude"] { settings { autoMemoryEnabled = false } }
//	  ["codex"] {}
//	}
//
// ```
//
// `config.pkl` uses it with `agents = import("agents.pkl").agents`.
// FAM knows the paths of each agent. Write settings only.
type Agents struct {
	// The agents. Supported keys: `"claude"` (Claude Code) and `"codex"` (Codex).
	Agents map[agentname.AgentName]base.Agent `pkl:"agents"`
}

// LoadFromPath loads the pkl module at the given path and evaluates it into a Agents
func LoadFromPath(ctx context.Context, path string) (ret Agents, err error) {
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

// Load loads the pkl module at the given source and evaluates it with the given evaluator into a Agents
func Load(ctx context.Context, evaluator pkl.Evaluator, source *pkl.ModuleSource) (Agents, error) {
	var ret Agents
	err := evaluator.EvaluateModule(ctx, source, &ret)
	return ret, err
}
