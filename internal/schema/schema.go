// Package schema holds the Pkl schema and the standard bundles of FAM. They form the Pkl package
// `fam`: a project declares it in its PklProject and imports `@fam/Config.pkl`. FAM builds the
// package from the files in its binary, and puts it into the Pkl cache, so that FAM and editors
// find it without a download.
package schema

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gregor-tokarev/fam/internal/selfupdate"
	"github.com/gregor-tokarev/fam/internal/version"
)

//go:embed all:fam
var files embed.FS

const placeholder = "@FAM_VERSION@"

// Files of a project.
const (
	ProjectFile = "PklProject"
	DepsFile    = "PklProject.deps.json"
)

// releaseBase is where the releases of FAM publish the package files.
const releaseBase = "github.com/gregor-tokarev/fam/releases/download/v"

// URI returns the package URI of FAM version v.
func URI(v string) string { return "package://" + releaseBase + v + "/fam@" + v }

// depsKey is the key of the package in PklProject.deps.json: the base URI and the major version.
func depsKey(v string) string {
	major, _, _ := strings.Cut(v, ".")
	return "package://" + releaseBase + v + "/fam@" + major
}

// Package is the Pkl package of one FAM version.
type Package struct {
	Version  string
	Zip      []byte
	Metadata []byte
	// Checksum is the SHA-256 of Metadata, as in PklProject.deps.json.
	Checksum string
}

// Build makes the package of FAM version v from the files in the binary. The zip file is the
// same for the same files and version.
func Build(v string) (*Package, error) {
	var names []string
	err := fs.WalkDir(files, "fam", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		names = append(names, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mod := time.Date(1980, 2, 1, 0, 0, 0, 0, time.UTC)
	for _, p := range names {
		b, err := files.ReadFile(p)
		if err != nil {
			return nil, err
		}
		b = bytes.ReplaceAll(b, []byte(placeholder), []byte(v))
		h := &zip.FileHeader{Name: strings.TrimPrefix(p, "fam/"), Method: zip.Deflate, Modified: mod}
		h.SetMode(0o644)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(b); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	zipSum := sha256.Sum256(buf.Bytes())
	meta, err := json.MarshalIndent(map[string]any{
		"name":                "fam",
		"packageUri":          URI(v),
		"version":             v,
		"packageZipUrl":       "https://" + releaseBase + v + "/fam@" + v + ".zip",
		"packageZipChecksums": map[string]string{"sha256": hex.EncodeToString(zipSum[:])},
		"dependencies":        map[string]any{},
		"authors":             []string{},
		"sourceCode":          "https://github.com/gregor-tokarev/fam",
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	meta = append(meta, '\n')
	sum := sha256.Sum256(meta)
	return &Package{Version: v, Zip: buf.Bytes(), Metadata: meta, Checksum: hex.EncodeToString(sum[:])}, nil
}

// Current is the package of this binary.
func Current() (*Package, error) { return Build(version.Version) }

// CacheDir is the package cache of Pkl 0.33 and later on Linux and macOS: $XDG_CACHE_HOME/pkl,
// or ~/.cache/pkl. FAM evaluates with it.
func CacheDir() (string, error) {
	if x := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "pkl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "pkl"), nil
}

// SeedAll puts the package into the cache of Pkl 0.33 and later, and into ~/.pkl/cache, which
// editor plugins with an older Pkl read.
func SeedAll(p *Package) error {
	dirs := []string{}
	if d, err := CacheDir(); err == nil {
		dirs = append(dirs, d)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".pkl", "cache"))
	}
	if len(dirs) == 0 {
		return errors.New("no home folder for the Pkl cache")
	}
	for _, d := range dirs {
		if err := Seed(d, p); err != nil {
			return err
		}
	}
	return nil
}

// Seed writes the package into the Pkl cache, unless the cache already has the same files.
func Seed(cacheDir string, p *Package) error {
	dir := filepath.Join(cacheDir, "package-2", filepath.FromSlash(releaseBase+p.Version), "fam@"+p.Version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{"fam@" + p.Version + ".zip", p.Zip}, {"fam@" + p.Version + ".json", p.Metadata}} {
		path := filepath.Join(dir, f.name)
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, f.data) {
			continue
		}
		if err := writeAtomic(path, f.data); err != nil {
			return err
		}
	}
	return nil
}

// WriteFiles writes the files that a release publishes: the metadata (`fam@<version>`) and the
// zip file (`fam@<version>.zip`).
func WriteFiles(dir string, p *Package) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "fam@"+p.Version), p.Metadata, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "fam@"+p.Version+".zip"), p.Zip, 0o644)
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".fam-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// uriPattern finds the URI of the fam dependency in a PklProject file: `uri = "package://…"`.
var uriPattern = regexp.MustCompile(`(uri\s*=\s*#*")package://` + regexp.QuoteMeta(releaseBase) + `[^"/\s]+/fam@[^"\s]+("#*)`)

// versionPattern is a semantic version 2.0.0 (the regular expression of semver.org).
var versionPattern = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// commentStart finds the start of a comment (`//` or `/*`) after the start of a line or a
// space. The `//` of a URI (`package://`) does not match.
var commentStart = regexp.MustCompile(`(^|\s)(//|/\*)`)

// replaceURI replaces the URI of the fam dependency in the code of a PklProject file, not in its
// comments. It returns the number of URIs that it found.
func replaceURI(b []byte, uri string) ([]byte, int) {
	lines := strings.SplitAfter(string(b), "\n")
	n := 0
	block := false // in a /* */ comment
	for i, line := range lines {
		from := 0
		if block {
			end := strings.Index(line, "*/")
			if end < 0 {
				continue
			}
			block = false
			from = end + 2
		}
		to := len(line)
		if m := commentStart.FindStringSubmatchIndex(line[from:]); m != nil {
			to = from + m[4]
			if line[to:to+2] == "/*" && !strings.Contains(line[to:], "*/") {
				block = true
			}
		}
		code := line[from:to]
		k := len(uriPattern.FindAllStringIndex(code, -1))
		if k == 0 {
			continue
		}
		n += k
		lines[i] = line[:from] + uriPattern.ReplaceAllString(code, "${1}"+uri+"${2}") + line[to:]
	}
	return []byte(strings.Join(lines, "")), n
}

// projectFile is the PklProject that `fam new` and `fam migrate` write.
func projectFile(v string) string {
	return fmt.Sprintf(`amends "pkl:Project"

// The schema and the standard bundles of FAM. `+"`fam migrate`"+` changes the version.
dependencies {
  ["fam"] { uri = %q }
}
`, URI(v))
}

// WriteProject writes PklProject and PklProject.deps.json for package p. In an existing
// PklProject, it changes only the URI of the fam dependency. In PklProject.deps.json, it
// replaces only the entry of fam.
func WriteProject(base string, p *Package) error {
	pp := filepath.Join(base, ProjectFile)
	b, err := os.ReadFile(pp)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		b = []byte(projectFile(p.Version))
	case err != nil:
		return err
	default:
		nb, n := replaceURI(b, URI(p.Version))
		if n == 1 {
			b = nb
			break
		}
		if n > 1 {
			return fmt.Errorf("%s has more than one fam dependency URI. Keep one: `[\"fam\"] { uri = %q }`", pp, URI(p.Version))
		}
		return fmt.Errorf("%s has no fam dependency. Add `[\"fam\"] { uri = %q }` to its `dependencies`, then run `fam migrate` again", pp, URI(p.Version))
	}
	deps := map[string]any{}
	dp := filepath.Join(base, DepsFile)
	if old, err := os.ReadFile(dp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	} else if err == nil {
		if err := json.Unmarshal(old, &deps); err != nil {
			return fmt.Errorf("read %s: %w", dp, err)
		}
		if deps == nil {
			return fmt.Errorf("read %s: the file is not a JSON object", dp)
		}
	}
	resolved, _ := deps["resolvedDependencies"].(map[string]any)
	if resolved == nil {
		resolved = map[string]any{}
	}
	for k := range resolved {
		if strings.HasPrefix(k, "package://"+releaseBase) {
			delete(resolved, k)
		}
	}
	resolved[depsKey(p.Version)] = map[string]any{
		"type":      "remote",
		"uri":       "projectpackage://" + strings.TrimPrefix(URI(p.Version), "package://"),
		"checksums": map[string]string{"sha256": p.Checksum},
	}
	deps["schemaVersion"] = 1
	deps["resolvedDependencies"] = resolved
	out, err := json.MarshalIndent(deps, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(pp, b); err != nil {
		return err
	}
	return writeAtomic(dp, append(out, '\n'))
}

// WriteEditorState writes `.pkl-lsp/projects.json`, the project state of the Pkl language server
// (pkl-lsp), from PklProject.deps.json. pkl-lsp writes this file on "Sync Projects", and reads
// it when it starts. Editors that cannot send that request (Zed) then resolve `@fam/…` too. It
// keeps the other entries and the other declared dependencies of an existing state.
func WriteEditorState(base string, p *Package) error {
	b, err := os.ReadFile(filepath.Join(base, DepsFile))
	if err != nil {
		return err
	}
	var deps struct {
		ResolvedDependencies map[string]map[string]any `json:"resolvedDependencies"`
	}
	if err := json.Unmarshal(b, &deps); err != nil {
		return fmt.Errorf("read %s: %w", DepsFile, err)
	}
	resolved := map[string]any{}
	for k, d := range deps.ResolvedDependencies {
		e := map[string]any{}
		for f, v := range d {
			switch f {
			case "type":
				e["myType"] = v
			case "uri":
				s, _ := v.(string)
				e["uri"] = strings.Replace(s, "projectpackage://", "package://", 1)
			default:
				e[f] = v
			}
		}
		resolved[k] = e
	}

	dir := filepath.Join(base, ".pkl-lsp")
	sp := filepath.Join(dir, "projects.json")
	var state struct {
		SchemaVersion int              `json:"schemaVersion"`
		PklProjects   []map[string]any `json:"pklProjects"`
	}
	if old, err := os.ReadFile(sp); err == nil {
		// A damaged state is a cache: replace it.
		if json.Unmarshal(old, &state) != nil {
			state.PklProjects = nil
		}
	}
	var entry map[string]any
	for _, e := range state.PklProjects {
		if e["projectFile"] == ProjectFile {
			entry = e
		}
	}
	if entry == nil {
		entry = map[string]any{"projectFile": ProjectFile, "packageName": nil, "packageUri": nil, "evaluatorSettings": map[string]any{}}
		state.PklProjects = append(state.PklProjects, entry)
	}
	declared, _ := entry["declaredDependencies"].(map[string]any)
	if declared == nil {
		declared = map[string]any{}
	}
	declared["fam"] = URI(p.Version)
	entry["declaredDependencies"] = declared
	entry["resolvedDependencies"] = resolved
	state.SchemaVersion = 1

	out, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// pkl-lsp writes the same .gitignore: the state is local to the machine.
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*\n"), 0o644); err != nil {
			return err
		}
	}
	return writeAtomic(sp, append(out, '\n'))
}

// Pinned returns the FAM version and the checksum that PklProject.deps.json of a project pins.
func Pinned(base string) (v, checksum string, err error) {
	b, err := os.ReadFile(filepath.Join(base, DepsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("%s has no %s. Run `fam new` to create a project, or `fam migrate` to add the fam package", base, DepsFile)
	}
	if err != nil {
		return "", "", err
	}
	var deps struct {
		ResolvedDependencies map[string]struct {
			URI       string            `json:"uri"`
			Checksums map[string]string `json:"checksums"`
		} `json:"resolvedDependencies"`
	}
	if err := json.Unmarshal(b, &deps); err != nil {
		return "", "", fmt.Errorf("read %s: %w", filepath.Join(base, DepsFile), err)
	}
	dp := filepath.Join(base, DepsFile)
	found := 0
	for k, d := range deps.ResolvedDependencies {
		if !strings.HasPrefix(k, "package://"+releaseBase) {
			continue
		}
		found++
		_, v, _ = strings.Cut(d.URI, "/fam@")
		checksum = d.Checksums["sha256"]
		if !versionPattern.MatchString(v) || k != depsKey(v) || d.URI != "projectpackage://"+strings.TrimPrefix(URI(v), "package://") {
			return "", "", fmt.Errorf("%s has an invalid fam entry %q. Run `fam migrate`", dp, k)
		}
	}
	switch found {
	case 0:
		return "", "", fmt.Errorf("%s has no fam package. Run `fam migrate`", dp)
	case 1:
		return v, checksum, nil
	}
	return "", "", fmt.Errorf("%s has more than one fam package. Run `fam migrate`", dp)
}

// Check stops when the fam package that a project pins does not fit this binary. A package of
// another patch release of the same minor version fits: patch releases do not change the
// schema. Pkl then loads that package from its cache, or from the GitHub release.
func Check(base string, own *Package) error {
	v, sum, err := Pinned(base)
	if err != nil {
		return err
	}
	if v == own.Version {
		if sum != own.Checksum {
			return fmt.Errorf("the fam package that %s pins differs from the package of this binary (FAM %s). Run `fam migrate`", filepath.Join(base, DepsFile), v)
		}
		return nil
	}
	if selfupdate.Compare(v, own.Version) > 0 {
		return fmt.Errorf("the project uses FAM %s, and this is FAM %s. Run `fam update`", v, own.Version)
	}
	if minorOf(v) == minorOf(own.Version) && !strings.Contains(v, "-") && !strings.Contains(own.Version, "-") {
		return nil
	}
	return fmt.Errorf("the project uses FAM %s, and this is FAM %s. Run `fam migrate`", v, own.Version)
}

// minorOf returns the major and minor part of a version ("0.9" for "0.9.3").
func minorOf(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}
