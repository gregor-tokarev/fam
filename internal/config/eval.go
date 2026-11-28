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
	readonly *schema.Package
	base     string
}

// NewEvaluator starts the pinned pkl binary, and reads the PklProject in base (the dependency on
// the fam package). Close it after use.
func NewEvaluator(ctx context.Context, base, configPath string, progress io.Writer) (*Evaluator, error) {
	return newEvaluator(ctx, base, configPath, progress, false)
}

// NewReadOnlyEvaluator uses cached packages and the pinned Pkl binary without writes or network access.
func NewReadOnlyEvaluator(ctx context.Context, base, configPath string) (*Evaluator, error) {
	return newEvaluator(ctx, base, configPath, nil, true)
}

func newEvaluator(ctx context.Context, base, configPath string, progress io.Writer, readonly bool) (*Evaluator, error) {
	var bin string
	var err error
	if readonly {
		bin, err = pklbin.CachedPath()
	} else {
		bin, err = pklbin.Path(ctx, progress)
	}
	if err != nil {
		return nil, err
	}
	cacheDir, err := schema.CacheDir()
	if err != nil {
		return nil, err
	}
	// Pkl gives the dependencies of PklProject only to modules in the project folder.
	if rel, err := filepath.Rel(base, configPath); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("the config %s is not in the project %s: it must be in the folder of PklProject, or below it", configPath, base)
	}
	e := &Evaluator{manager: pkl.NewEvaluatorManagerWithCommand([]string{bin}), path: configPath, cacheDir: cacheDir, base: base}
	if readonly {
		e.readonly, err = schema.Current()
		if err != nil {
			e.Close()
			return nil, err
		}
	}
	ev, err := e.manager.NewEvaluator(ctx, pkl.PreconfiguredOptions, func(o *pkl.EvaluatorOptions) {
		o.CacheDir = cacheDir
		e.readOnlyOptions(o)
	})
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
			e.readOnlyOptions(o)
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

// Init evaluates both the machines mapping and the secrets property in a single Pkl evaluator
// session so config.pkl is parsed and initialized only once.
func (e *Evaluator) Init(ctx context.Context, props map[string]string) ([]Machine, *base.Secrets, error) {
	ev, err := e.manager.NewEvaluator(ctx, e.options(props)...)
	if err != nil {
		return nil, nil, fmt.Errorf("start the Pkl evaluator: %w", err)
	}
	defer ev.Close()
	machines, err := evalMachineList(ctx, ev, e.path)
	if err != nil {
		return nil, nil, err
	}
	var sec *base.Secrets
	if err := ev.EvaluateExpression(ctx, pkl.FileSource(e.path), "secrets", &sec); err != nil {
		return nil, nil, &EvalError{Path: e.path, Err: err}
	}
	return machines, sec, nil
}

// MachineList evaluates only the machines mapping: name, cockpit, tags, address, and user of each machine.
// Pkl is lazy, so this does not evaluate bundles that depend on the machine facts.
func (e *Evaluator) MachineList(ctx context.Context, props map[string]string) ([]Machine, error) {
	ev, err := e.manager.NewEvaluator(ctx, e.options(props)...)
	if err != nil {
		return nil, fmt.Errorf("start the Pkl evaluator: %w", err)
	}
	defer ev.Close()
	return evalMachineList(ctx, ev, e.path)
}

func evalMachineList(ctx context.Context, ev pkl.Evaluator, path string) ([]Machine, error) {
	var raw map[string]map[string]string
	// Tag names cannot contain a comma, so one string keeps the map simple.
	expr := `machines.toMap().mapValues((_, m) -> Map("cockpit", m.cockpit.toString(), "tags", m.tags.toList().join(","), "address", m.address ?? "", "user", m.user ?? ""))`
	if err := ev.EvaluateExpression(ctx, pkl.FileSource(path), expr, &raw); err != nil {
		return nil, &EvalError{Path: path, Err: err}
	}
	out := make([]Machine, 0, len(raw))
	for name, m := range raw {
		tags := []string{}
		if m["tags"] != "" {
			tags = strings.Split(m["tags"], ",")
		}
		out = append(out, Machine{Name: name, Cockpit: m["cockpit"] == "true", Tags: tags, Address: m["address"], User: m["user"]})
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
	if h := e.Hint(); h != "" {
		msg += "\n\n" + h
	}
	return msg
}

// Hint explains how to fix a config of an older FAM version, or returns "".
func (e *EvalError) Hint() string {
	msg := e.Err.Error()
	if strings.Contains(msg, "Cannot find property `role`") || strings.Contains(msg, "Cannot find property `roles`") {
		return "FAM replaced roles with tags. In machines.pkl, change `role = \"cockpit\"` to `cockpit = true`, and " +
			"`role = \"worker\"` to `tags { \"worker\" }`. In config.pkl, rename `roles` to `tags`."
	}
	return ""
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

func (e *Evaluator) readOnlyOptions(o *pkl.EvaluatorOptions) {
	if e.readonly == nil {
		return
	}
	o.CacheDir = ""
	o.AllowedModules = []string{"pkl:", "file:", "package:", "projectpackage:"}
	o.AllowedResources = []string{"prop:", "env:", "file:", "package:", "projectpackage:"}
	embeddedOptions(e.readonly, e.project, e.base)(o)
}
