// Package schema holds the Pkl schema and the standard bundles that FAM writes into `.fam/`.
package schema

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregor-tokarev/fam/internal/version"
)

//go:embed all:fam
var files embed.FS

// Dir is the name of the schema directory in a project.
const Dir = ".fam"

// VersionFile is the file in `.fam/` that holds the FAM version that wrote it.
const VersionFile = "VERSION"

const placeholder = "@FAM_VERSION@"

// Write replaces `<base>/.fam` with the schema of this FAM version.
// It writes into a temporary directory first, then swaps the directories.
func Write(base string) error {
	target := filepath.Join(base, Dir)
	tmp, err := os.MkdirTemp(base, ".fam-new-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	err = fs.WalkDir(files, "fam", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(path, "fam"), "/")
		dest := filepath.Join(tmp, rel)
		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}
		b, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		b = bytes.ReplaceAll(b, []byte(placeholder), []byte(version.Version))
		return os.WriteFile(dest, b, 0o644)
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, VersionFile), []byte(version.Version+"\n"), 0o644); err != nil {
		return err
	}
	// Move the old schema to a new, unique folder, so that FAM never deletes a folder that it did not make.
	if _, err := os.Stat(target); err == nil {
		old, err := os.MkdirTemp(base, ".fam-old-")
		if err != nil {
			return err
		}
		if err := os.Remove(old); err != nil {
			return err
		}
		if err := os.Rename(target, old); err != nil {
			return err
		}
		if err := os.Rename(tmp, target); err != nil {
			_ = os.Rename(old, target)
			return err
		}
		return os.RemoveAll(old)
	}
	return os.Rename(tmp, target)
}

// ProjectVersion reads the FAM version that wrote `<base>/.fam`.
func ProjectVersion(base string) (string, error) {
	b, err := os.ReadFile(filepath.Join(base, Dir, VersionFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s has no %s/%s. Run `fam new` to create a project, or `fam upgrade` to write the schema", base, Dir, VersionFile)
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Check stops when `.fam/` does not fit this binary. A `.fam/` of another FAM version fits when
// it has the same schema as this binary: the same files with the same content, apart from the
// version and the doc comments. So a release that does not change the schema needs no
// `fam upgrade`, and a release that does (for example a fixed standard bundle) asks for it.
func Check(base string) error {
	v, err := ProjectVersion(base)
	if err != nil {
		return err
	}
	if v == version.Version {
		return nil
	}
	if same, err := sameSchema(filepath.Join(base, Dir), v); err == nil && same {
		return nil
	}
	return fmt.Errorf("the schema in %s was written by FAM %s and differs from the schema of this binary (FAM %s). Run `fam upgrade` to update %s, or use FAM %s",
		filepath.Join(base, Dir), v, version.Version, Dir, v)
}

// sameSchema reports whether dir holds the schema of this binary as FAM `v` would write it,
// apart from doc comments in Pkl files.
func sameSchema(dir, v string) (bool, error) {
	want := map[string]bool{VersionFile: true}
	same := true
	err := fs.WalkDir(files, "fam", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !same {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(path, "fam"), "/")
		want[rel] = true
		b, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			same = false
			return nil
		}
		b = bytes.ReplaceAll(b, []byte(placeholder), []byte(v))
		if strings.HasSuffix(rel, ".pkl") {
			b, got = withoutDocs(b), withoutDocs(got)
		}
		if !bytes.Equal(b, got) {
			same = false
		}
		return nil
	})
	if err != nil || !same {
		return false, err
	}
	// A file that this binary does not write (for example a removed module) is a difference too.
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !same {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		// Hidden files, for example .DS_Store of macOS, are not part of the schema.
		if !want[filepath.ToSlash(rel)] && !strings.HasPrefix(d.Name(), ".") {
			same = false
		}
		return nil
	})
	return same && err == nil, err
}

// withoutDocs removes the doc comment lines (`///` and `//!`) of a Pkl file.
func withoutDocs(b []byte) []byte {
	var out [][]byte
	for _, line := range bytes.Split(b, []byte("\n")) {
		t := bytes.TrimSpace(line)
		if bytes.HasPrefix(t, []byte("///")) || bytes.HasPrefix(t, []byte("//!")) {
			continue
		}
		out = append(out, line)
	}
	return bytes.Join(out, []byte("\n"))
}
