// Package config evaluates the Pkl config and turns it into the plan input for each machine.
package config

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/apple/pkl-go/pkl"

	"github.com/gregor-tokarev/fam/internal/pklbin"
	"github.com/gregor-tokarev/fam/internal/pklgen/base"
	"github.com/gregor-tokarev/fam/internal/pklgen/config"
	"github.com/gregor-tokarev/fam/internal/schema"
)

// Evaluator evaluates the config entrypoint with external properties.
type Evaluator struct {
	manager  pkl.EvaluatorManager
	path     string
	project  *pkl.Project
	cacheDir string
}

// NewEvaluator starts the pinned pkl binary, and reads the PklProject in base (the dependency on
// the fam package). Close it after use.
func NewEvaluator(ctx context.Context, base, configPath string, progress io.Writer) (*Evaluator, error) {
	bin, err := pklbin.Path(ctx, progress)
	if err != nil {
		return nil, err
	}
	cacheDir, err := schema.CacheDir()
	if err != nil {
		return nil, err
	}
	e := &Evaluator{manager: pkl.NewEvaluatorManagerWithCommand([]string{bin}), path: configPath, cacheDir: cacheDir}
	ev, err := e.manager.NewEvaluator(ctx, pkl.PreconfiguredOptions)
	if err != nil {
		e.Close()
		return nil, fmt.Errorf("start the Pkl evaluator: %w", err)
	}
	defer ev.Close()
	pp := filepath.Join(base, "PklProject")
	e.project, err = pkl.LoadProjectFromEvaluator(ctx, ev, pkl.FileSource(pp))
	if err != nil {
		e.Close()
		return nil, &EvalError{Path: pp, Err: err}
	}
	return e, nil
}

// options are the evaluator options of each evaluation.
func (e *Evaluator) options(props map[string]string) []func(*pkl.EvaluatorOptions) {
	return []func(*pkl.EvaluatorOptions){
		pkl.WithDefaultAllowedModules, pkl.WithDefaultAllowedResources,
		pkl.WithProjectDependencies(e.project),
		func(o *pkl.EvaluatorOptions) {
			o.CacheDir = e.cacheDir
			o.Properties = props
			o.Logger = pkl.NoopLogger
		},
	}
}

// Close stops the pkl process.
func (e *Evaluator) Close() error { return e.manager.Close() }

// Eval evaluates the entrypoint once with the given properties.
func (e *Evaluator) Eval(ctx context.Context, props map[string]string) (*config.Config, error) {
	ev, err := e.manager.NewEvaluator(ctx, e.options(props)...)
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
// Pkl is lazy, so this does not evaluate bundles that depend on the machine facts.
func (e *Evaluator) MachineList(ctx context.Context, props map[string]string) ([]Machine, error) {
	ev, err := e.manager.NewEvaluator(ctx, e.options(props)...)
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
	ev, err := e.manager.NewEvaluator(ctx, e.options(props)...)
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
	msg := fmt.Sprintf("evaluate %s:\n%s", e.Path, strings.TrimSpace(e.Err.Error()))
	return msg
}

// FileFromURI converts a `file:` URI from `reflect.Module(module).uri` to a path.
func FileFromURI(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("module %s is not a local file. FAM supports only file modules for bundles", uri)
	}
	return u.Path, nil
}
