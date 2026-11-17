// Package stdlib reads the standard bundles from the embedded Pkl package.
package stdlib

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/apple/pkl-go/pkl"
	"github.com/gregor-tokarev/fam/internal/facts"
	"github.com/gregor-tokarev/fam/internal/pklbin"
	"github.com/gregor-tokarev/fam/internal/pklgen/bundle"
	"github.com/gregor-tokarev/fam/internal/schema"
)

// Group is one module of std.pkl, including empty groups.
type Group struct {
	Name    string
	Bundles []Entry
}

// Entry is one named bundle in a group.
type Entry struct {
	Name   string
	Bundle bundle.Bundle
	// Doc is the doc comment of the bundle property in its group module.
	Doc string
}

// LinuxFacts fixes the inputs of the generated documentation.
func LinuxFacts() facts.Facts {
	return facts.Reference()
}

// Read evaluates std.pkl with the given facts. It needs no project or network downloads.
func Read(ctx context.Context, f facts.Facts) ([]Group, error) {
	return read(ctx, f, "", false)
}

// Catalogue reads only names and comments, without evaluation of bundle tasks.
func Catalogue(ctx context.Context) ([]Group, error) { return read(ctx, LinuxFacts(), "", true) }

// Lookup evaluates only the selected bundle.
func Lookup(ctx context.Context, f facts.Facts, name string) ([]Group, error) {
	return read(ctx, f, name, false)
}

func read(ctx context.Context, f facts.Facts, selected string, namesOnly bool) ([]Group, error) {
	pkg, err := schema.Current()
	if err != nil {
		return nil, err
	}
	if err := schema.SeedAll(pkg); err != nil {
		return nil, err
	}
	bin, err := pklbin.Path(ctx, nil)
	if err != nil {
		return nil, err
	}
	cache, err := schema.CacheDir()
	if err != nil {
		return nil, err
	}
	manager := pkl.NewEvaluatorManagerWithCommand([]string{bin})
	defer manager.Close()
	ev, err := manager.NewEvaluator(ctx, pkl.PreconfiguredOptions, func(o *pkl.EvaluatorOptions) {
		o.CacheDir = cache
		o.Properties = f.Properties()
		o.Logger = pkl.NoopLogger
	})
	if err != nil {
		return nil, err
	}
	defer ev.Close()
	src := pkl.UriSource(schema.URI(pkg.Version) + "#/std.pkl")
	// Each group module declares one function per bundle. Pkl cannot call a function that
	// reflection finds, so the second expression calls each function by name.
	var docs map[string]map[string]string
	if err := ev.EvaluateExpression(ctx, src, `toDynamic().toMap().mapValues((_, group) -> import("pkl:reflect").Module(group).moduleClass.methods.mapValues((_, m) -> m.docComment ?? ""))`, &docs); err != nil {
		return nil, fmt.Errorf("read the functions of the standard bundles: %w", err)
	}
	var groups map[string]map[string]bundle.Bundle
	if !namesOnly {
		calls := docs
		if selected != "" {
			calls = map[string]map[string]string{}
			for g, fns := range docs {
				for n, doc := range fns {
					if g+"."+n == selected {
						calls[g] = map[string]string{n: doc}
					}
				}
			}
		}
		if err := ev.EvaluateExpression(ctx, src, callAll(calls), &groups); err != nil {
			return nil, fmt.Errorf("read standard bundles: %w", err)
		}
	}
	var out []Group
	for name, fns := range docs {
		g := Group{Name: name}
		for n, doc := range fns {
			if selected != "" && name+"."+n != selected {
				continue
			}
			b := groups[name][n]
			if selected != "" {
				var full string
				if err := ev.EvaluateExpression(ctx, pkl.UriSource(b.Source), `import("pkl:reflect").Module(module).docComment ?? ""`, &full); err != nil {
					return nil, err
				}
				if full != "" {
					doc = full
				}
			}
			g.Bundles = append(g.Bundles, Entry{Name: n, Bundle: b, Doc: doc})
		}
		sort.Slice(g.Bundles, func(i, j int) bool { return g.Bundles[i].Name < g.Bundles[j].Name })
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// callAll returns a Pkl expression that calls every bundle function of the groups, for example
// `Map("networking", Map("portless", networking.portless()))`.
func callAll(groups map[string]map[string]string) string {
	var gs []string
	for g := range groups {
		gs = append(gs, g)
	}
	sort.Strings(gs)
	var parts []string
	for _, g := range gs {
		var calls []string
		for f := range groups[g] {
			calls = append(calls, fmt.Sprintf("%q, %s.%s()", f, g, f))
		}
		sort.Strings(calls)
		parts = append(parts, fmt.Sprintf("%q, Map(%s)", g, strings.Join(calls, ", ")))
	}
	return "Map(" + strings.Join(parts, ", ") + ")"
}

// Select validates a comma separated group filter. An empty filter selects all groups.
func Select(groups []Group, filter string) ([]Group, error) {
	if strings.TrimSpace(filter) == "" {
		return groups, nil
	}
	wanted := map[string]bool{}
	for _, n := range strings.Split(filter, ",") {
		wanted[strings.TrimSpace(n)] = true
	}
	var out []Group
	for _, g := range groups {
		if wanted[g.Name] {
			out = append(out, g)
			delete(wanted, g.Name)
		}
	}
	if len(wanted) > 0 {
		var unknown []string
		for n := range wanted {
			unknown = append(unknown, n)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown standard groups: %s", strings.Join(unknown, ", "))
	}
	return out, nil
}

// Summary returns the first sentence used by the catalogue.
func Summary(doc string) string { return summary(doc) }
