// Package config evaluates the Pkl config and turns it into the plan input for each machine.
package config

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/apple/pkl-go/pkl"

	"github.com/gregor-tokarev/fam/internal/pklbin"
	"github.com/gregor-tokarev/fam/internal/pklgen/base"
	"github.com/gregor-tokarev/fam/internal/pklgen/config"
)

// Evaluator evaluates the config entrypoint with external properties.
type Evaluator struct {
	manager pkl.EvaluatorManager
	path    string
}

// NewEvaluator starts the pinned pkl binary. Close it after use.
func NewEvaluator(ctx context.Context, configPath string, progress io.Writer) (*Evaluator, error) {
	bin, err := pklbin.Path(ctx, progress)
	if err != nil {
		return nil, err
	}
	return &Evaluator{manager: pkl.NewEvaluatorManagerWithCommand([]string{bin}), path: configPath}, nil
}

// Close stops the pkl process.
func (e *Evaluator) Close() error { return e.manager.Close() }

// Eval evaluates the entrypoint once with the given properties.
func (e *Evaluator) Eval(ctx context.Context, props map[string]string) (*config.Config, error) {
	ev, err := e.manager.NewEvaluator(ctx, pkl.WithDefaultAllowedModules, pkl.WithDefaultAllowedResources, pkl.WithDefaultCacheDir,
		func(o *pkl.EvaluatorOptions) {
			o.Properties = props
			o.Logger = pkl.NoopLogger
		})
	if err != nil {
		return nil, fmt.Errorf("start the Pkl evaluator: %w", err)
	}
	defer ev.Close()
	var cfg config.Config
	if err := ev.EvaluateModule(ctx, pkl.FileSource(e.path), &cfg); err != nil {
		return nil, &EvalError{Path: e.path, Err: err}
	}
	return &cfg, nil
}

// MachineList evaluates only the machines mapping: name, role, address, and user of each machine.
// Pkl is lazy, so this does not evaluate bundles or playbooks that depend on the machine facts.
func (e *Evaluator) MachineList(ctx context.Context, props map[string]string) ([]Machine, error) {
	ev, err := e.manager.NewEvaluator(ctx, pkl.WithDefaultAllowedModules, pkl.WithDefaultAllowedResources, pkl.WithDefaultCacheDir,
		func(o *pkl.EvaluatorOptions) {
			o.Properties = props
			o.Logger = pkl.NoopLogger
		})
	if err != nil {
		return nil, fmt.Errorf("start the Pkl evaluator: %w", err)
	}
	defer ev.Close()
	var raw map[string]map[string]string
	expr := `machines.toMap().mapValues((_, m) -> Map("role", m.role, "address", m.address ?? "", "user", m.user ?? ""))`
	if err := ev.EvaluateExpression(ctx, pkl.FileSource(e.path), expr, &raw); err != nil {
		return nil, &EvalError{Path: e.path, Err: err}
	}
	out := make([]Machine, 0, len(raw))
	for name, m := range raw {
		out = append(out, Machine{Name: name, Role: m["role"], Address: m["address"], User: m["user"]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Secrets evaluates only the secrets property. It does not depend on the machine facts.
func (e *Evaluator) Secrets(ctx context.Context, props map[string]string) (*base.Secrets, error) {
	ev, err := e.manager.NewEvaluator(ctx, pkl.WithDefaultAllowedModules, pkl.WithDefaultAllowedResources, pkl.WithDefaultCacheDir,
		func(o *pkl.EvaluatorOptions) {
			o.Properties = props
			o.Logger = pkl.NoopLogger
		})
	if err != nil {
		return nil, fmt.Errorf("start the Pkl evaluator: %w", err)
	}
	defer ev.Close()
	var sec *base.Secrets
	if err := ev.EvaluateExpression(ctx, pkl.FileSource(e.path), "secrets", &sec); err != nil {
		return nil, &EvalError{Path: e.path, Err: err}
	}
	return sec, nil
}

// EvalError is a Pkl evaluation error.
type EvalError struct {
	Path string
	Err  error
}

func (e *EvalError) Error() string {
	return fmt.Sprintf("evaluate %s:\n%s", e.Path, strings.TrimSpace(e.Err.Error()))
}

// FileFromURI converts a `file:` URI from `reflect.Module(module).uri` to a path.
func FileFromURI(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("module %s is not a local file. FAM supports only file modules for bundles and playbooks", uri)
	}
	return u.Path, nil
}
