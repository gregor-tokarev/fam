package config

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/apple/pkl-go/pkl"
	"github.com/gregor-tokarev/fam/internal/schema"
)

// localModules preserves file identities while directing @fam imports to the embedded package.
// The evaluator uses no disk cache and permits no network reads.
type localModules struct{ replacements *strings.Replacer }

func (*localModules) Scheme() string            { return "file" }
func (*localModules) IsGlobbable() bool         { return true }
func (*localModules) IsLocal() bool             { return true }
func (*localModules) HasHierarchicalUris() bool { return true }
func (*localModules) ListElements(u url.URL) ([]pkl.PathElement, error) {
	entries, err := os.ReadDir(u.Path)
	if err != nil {
		return nil, err
	}
	out := make([]pkl.PathElement, 0, len(entries))
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink == 0 {
			out = append(out, pkl.NewPathElement(e.Name(), e.IsDir()))
		}
	}
	return out, nil
}

var dependencyImport = regexp.MustCompile(`(?:import\s*\(?\s*|amends\s+|extends\s+|read\??\s*\(\s*)#*"[^"\n]+"#*`)

func (r *localModules) Read(u url.URL) (string, error) {
	b, err := os.ReadFile(u.Path)
	if err != nil {
		return "", err
	}
	return dependencyImport.ReplaceAllStringFunc(string(b), r.replacements.Replace), nil
}

// importFS keeps package imports in memory. Relative imports keep their normal paths.
type importFS struct {
	fs.FS
	replacements *strings.Replacer
}

func (f importFS) ReadFile(name string) ([]byte, error) {
	b, err := fs.ReadFile(f.FS, name)
	if err == nil && strings.HasSuffix(name, ".pkl") {
		b = []byte(dependencyImport.ReplaceAllStringFunc(string(b), f.replacements.Replace))
	}
	return b, err
}

func embeddedOptions(pkg *schema.Package, project *pkl.Project, base string) func(*pkl.EvaluatorOptions) {
	return func(o *pkl.EvaluatorOptions) {
		type mounted struct {
			scheme  string
			files   fs.FS
			aliases map[string]string
		}
		z, _ := zip.NewReader(bytes.NewReader(pkg.Zip), int64(len(pkg.Zip)))
		mounts := map[string]mounted{schema.URI(pkg.Version): {scheme: "fam", files: z}}
		var deps struct {
			ResolvedDependencies map[string]struct {
				Type string `json:"type"`
				URI  string `json:"uri"`
			} `json:"resolvedDependencies"`
		}
		if b, err := os.ReadFile(filepath.Join(base, schema.DepsFile)); err == nil {
			_ = json.Unmarshal(b, &deps)
		}
		cache, _ := schema.CacheDir()
		var keys []string
		for k := range deps.ResolvedDependencies {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			dep := deps.ResolvedDependencies[k]
			uri := strings.Replace(dep.URI, "projectpackage:", "package:", 1)
			if dep.Type != "remote" || mounts[uri].files != nil {
				continue
			}
			u, err := url.Parse(uri)
			if err != nil {
				continue
			}
			folder := filepath.Join(cache, "package-2", u.Host, filepath.FromSlash(u.Path))
			name := path.Base(u.Path)
			b, err := os.ReadFile(filepath.Join(folder, name+".zip"))
			if err != nil {
				continue
			}
			archive, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
			if err != nil {
				continue
			}
			var metadata struct {
				Dependencies map[string]struct {
					URI string `json:"uri"`
				} `json:"dependencies"`
			}
			meta, err := os.ReadFile(filepath.Join(folder, name+".json"))
			if err == nil {
				_ = json.Unmarshal(meta, &metadata)
			}
			aliases := map[string]string{}
			for alias, d := range metadata.Dependencies {
				aliases[alias] = d.URI
			}
			mounts[uri] = mounted{scheme: fmt.Sprintf("fampackage%d", len(mounts)), files: archive, aliases: aliases}
		}
		pairs := []string{}
		var uris []string
		for uri := range mounts {
			uris = append(uris, uri)
		}
		sort.Strings(uris)
		for _, uri := range uris {
			m := mounts[uri]
			pairs = append(pairs, "\""+uri+"#/", "\""+m.scheme+":/", "\""+strings.Replace(uri, "package:", "projectpackage:", 1)+"#/", "\""+m.scheme+":/")
		}
		projectPairs := append([]string{}, pairs...)
		projectPairs = append(projectPairs, "\"@fam/", "\"fam:/")
		if project != nil {
			for alias, dep := range project.Dependencies().RemoteDependencies {
				if m, ok := mounts[dep.PackageUri]; ok {
					projectPairs = append(projectPairs, "\"@"+alias+"/", "\""+m.scheme+":/")
				}
			}
		}
		for _, uri := range uris {
			m := mounts[uri]
			localPairs := append([]string{}, pairs...)
			for alias, target := range m.aliases {
				if targetMount, ok := mounts[target]; ok {
					localPairs = append(localPairs, "\"@"+alias+"/", "\""+targetMount.scheme+":/")
				}
			}
			pkl.WithFs(importFS{FS: m.files, replacements: strings.NewReplacer(localPairs...)}, m.scheme)(o)
		}
		o.ModuleReaders = append(o.ModuleReaders, &localModules{replacements: strings.NewReplacer(projectPairs...)})
	}
}
