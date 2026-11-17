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
	return facts.Facts{OS: "linux", Distro: "ubuntu", DistroVersion: "26.04", Arch: "amd64", Libc: "glibc", CPUs: 4, Home: "/home/fam", User: "fam", Hostname: "fam-docs", Init: "systemd", Shell: "/bin/bash", Machine: "docs", Role: "worker"}
}

// Read evaluates std.pkl with the given facts. It needs no project or network downloads.
func Read(ctx context.Context, f facts.Facts) ([]Group, error) {
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
	var groups map[string]map[string]bundle.Bundle
	if err := ev.EvaluateExpression(ctx, pkl.UriSource(schema.URI(pkg.Version)+"#/std.pkl"), `toDynamic().toMap().mapValues((_, group) -> group.toDynamic().toMap())`, &groups); err != nil {
		return nil, fmt.Errorf("read standard bundles: %w", err)
	}
	var docs map[string]map[string]string
	if err := ev.EvaluateExpression(ctx, pkl.UriSource(schema.URI(pkg.Version)+"#/std.pkl"), `toDynamic().toMap().mapValues((_, group) -> import("pkl:reflect").Module(group).moduleClass.properties.mapValues((_, p) -> p.docComment ?? ""))`, &docs); err != nil {
		return nil, fmt.Errorf("read the doc comments of the standard bundles: %w", err)
	}
	var out []Group
	for name, bs := range groups {
		g := Group{Name: name}
		for n, b := range bs {
			g.Bundles = append(g.Bundles, Entry{Name: n, Bundle: b, Doc: docs[name][n]})
		}
		sort.Slice(g.Bundles, func(i, j int) bool { return g.Bundles[i].Name < g.Bundles[j].Name })
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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
