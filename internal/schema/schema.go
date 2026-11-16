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

// Check stops when `.fam/` and the binary have different versions.
func Check(base string) error {
	v, err := ProjectVersion(base)
	if err != nil {
		return err
	}
	if v != version.Version {
		return fmt.Errorf("the schema in %s was written by FAM %s, but this binary is FAM %s. Run `fam upgrade` to update %s, or use FAM %s",
			filepath.Join(base, Dir), v, version.Version, Dir, v)
	}
	return nil
}
