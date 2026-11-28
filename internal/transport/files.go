package transport

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// FileInfo describes a path on a machine.
type FileInfo struct {
	Exists bool
	// Type is "file", "dir", "link", or "other".
	Type string
	// Mode is the permission bits, for example 0o644.
	Mode os.FileMode
	// SHA256 is the content digest of a regular file.
	SHA256 string
	// Target is the target of a symbolic link.
	Target string
}

// files implements the file operations with shell commands, so SSH and local behave the same.
type files struct {
	run func(ctx context.Context, c Cmd) (Result, error)
	os  string
}

// SetOS selects OS-specific commands. FAM calls it after fact collection.
func (f *files) SetOS(goos string) { f.os = goos }

func (f *files) modeCmd(p string) string {
	if f.os == "darwin" {
		return "stat -f %Lp " + p
	}
	return "stat -c %a " + p
}

// SHA256Cmd returns the command that prints the SHA-256 of stdin.
func (f *files) SHA256Cmd() string {
	if f.os == "darwin" {
		return "shasum -a 256"
	}
	return "sha256sum"
}

// ReadFile reads a regular file. exists is false when the path does not exist.
func (f *files) ReadFile(ctx context.Context, path string, sudo bool) (data []byte, exists bool, err error) {
	q := Quote(path)
	script := fmt.Sprintf(`if [ -f %[1]s ]; then exec cat -- %[1]s; elif [ -e %[1]s ] || [ -L %[1]s ]; then exit 4; fi
d=$(dirname -- %[1]s)
while [ ! -e "$d" ] && [ "$d" != / ]; do d=$(dirname -- "$d"); done
if [ -d "$d" ] && [ ! -x "$d" ]; then echo "cannot look into $d: permission denied" >&2; exit 5; fi
exit 3`, q)
	r, err := f.run(ctx, Cmd{Script: script, Sudo: sudo, Label: "read " + path, Secret: true})
	if err != nil {
		return nil, false, err
	}
	switch r.Code {
	case 0:
		return r.Stdout, true, nil
	case 3:
		return nil, false, nil
	case 4:
		return nil, true, fmt.Errorf("%s exists and is not a regular file", path)
	default:
		return nil, false, fmt.Errorf("read %s: exit code %d: %s", path, r.Code, r.Err())
	}
}

// ReadFileInfo reads a regular file and its permission bits in a single command.
func (f *files) ReadFileInfo(ctx context.Context, path string, sudo bool) (data []byte, info FileInfo, err error) {
	q := Quote(path)
	script := fmt.Sprintf(`p=%[1]s
if [ -f "$p" ]; then
  if [ -L "$p" ]; then
    echo "FAM_META:link:0"
  else
    m=$(%[2]s) || exit 5
    echo "FAM_META:file:$m"
  fi
  exec cat -- "$p"
elif [ -e "$p" ] || [ -L "$p" ]; then
  exit 4
fi
d=$(dirname -- "$p")
while [ ! -e "$d" ] && [ "$d" != / ]; do d=$(dirname -- "$d"); done
if [ -d "$d" ] && [ ! -x "$d" ]; then echo "cannot look into $d: permission denied" >&2; exit 5; fi
exit 3`, q, f.modeCmd(`"$p"`))
	r, err := f.run(ctx, Cmd{Script: script, Sudo: sudo, Label: "read " + path, Secret: true})
	if err != nil {
		return nil, FileInfo{}, err
	}
	switch r.Code {
	case 0:
		head, body, ok := strings.Cut(string(r.Stdout), "\n")
		if !ok || !strings.HasPrefix(head, "FAM_META:") {
			return nil, FileInfo{}, fmt.Errorf("read %s: malformed metadata", path)
		}
		parts := strings.SplitN(strings.TrimPrefix(head, "FAM_META:"), ":", 2)
		if len(parts) != 2 {
			return nil, FileInfo{}, fmt.Errorf("read %s: malformed metadata", path)
		}
		fi := FileInfo{Exists: true, Type: parts[0]}
		if n, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 8, 32); err == nil {
			fi.Mode = os.FileMode(n)
		}
		return []byte(body), fi, nil
	case 3:
		return nil, FileInfo{}, nil
	case 4:
		return nil, FileInfo{Exists: true}, fmt.Errorf("%s exists and is not a regular file", path)
	default:
		return nil, FileInfo{}, fmt.Errorf("read %s: exit code %d: %s", path, r.Code, r.Err())
	}
}

// WriteFile writes data to path atomically: a temporary file in the same directory, then a rename.
// It creates the parent directories.
func (f *files) WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode, sudo bool) error {
	q := Quote(path)
	m := fmt.Sprintf("%04o", mode.Perm())
	if !sudo {
		script := fmt.Sprintf(`set -e
p=%[1]s
d=$(dirname -- "$p")
mkdir -p -- "$d"
t=$(mktemp "$d/.fam-tmp.XXXXXX")
trap 'rm -f -- "$t"' EXIT
cat > "$t"
chmod %[2]s "$t"
mv -f -- "$t" "$p"
trap - EXIT`, q, m)
		r, err := f.run(ctx, Cmd{Script: script, Stdin: data, Label: "write " + path})
		if err != nil {
			return err
		}
		if r.Code != 0 {
			return fmt.Errorf("write %s: exit code %d: %s", path, r.Code, r.Err())
		}
		return nil
	}
	// sudo cannot read stdin: write a temporary file as the user first.
	r, err := f.run(ctx, Cmd{Script: `set -e; t=$(mktemp "${TMPDIR:-/tmp}/fam-stage.XXXXXX"); cat > "$t"; echo "$t"`, Stdin: data, Label: "stage " + path})
	if err != nil {
		return err
	}
	if r.Code != 0 {
		return fmt.Errorf("stage %s: exit code %d: %s", path, r.Code, r.Err())
	}
	stage := r.Out()
	script := fmt.Sprintf(`set -e
s=%[3]s
trap 'rm -f -- "$s"' EXIT
p=%[1]s
d=$(dirname -- "$p")
mkdir -p -- "$d"
t=$(mktemp "$d/.fam-tmp.XXXXXX")
cat -- "$s" > "$t"
chmod %[2]s "$t"
mv -f -- "$t" "$p"`, q, m, Quote(stage))
	r, err = f.run(ctx, Cmd{Script: script, Sudo: true, Label: "write " + path})
	if err != nil {
		return err
	}
	if r.Code != 0 {
		_, _ = f.run(ctx, Cmd{Script: "rm -f -- " + Quote(stage)})
		return fmt.Errorf("write %s with sudo: exit code %d: %s", path, r.Code, r.Err())
	}
	return nil
}

// Stat returns information about path. It does not follow a symbolic link.
func (f *files) Stat(ctx context.Context, path string, sudo bool) (FileInfo, error) {
	q := Quote(path)
	script := fmt.Sprintf(`p=%[1]s
if [ -L "$p" ]; then echo type=link; printf 'target=%%s\n' "$(readlink -- "$p")"
elif [ -d "$p" ]; then echo type=dir; m=$(%[2]s) || exit 5; echo "mode=$m"
elif [ -f "$p" ]; then echo type=file; m=$(%[2]s) || exit 5; echo "mode=$m"; h=$(%[3]s < "$p") || exit 5; echo "sha256=${h%%%% *}"
elif [ -e "$p" ]; then echo type=other
else
  # A path that does not exist, or a path that FAM cannot see: a folder on the way is not searchable.
  d=$(dirname -- "$p")
  while [ ! -e "$d" ] && [ "$d" != / ]; do d=$(dirname -- "$d"); done
  if [ -d "$d" ] && [ ! -x "$d" ]; then echo "cannot look into $d: permission denied" >&2; exit 5; fi
  echo type=none
fi`, q, f.modeCmd(`"$p"`), f.SHA256Cmd())
	r, err := f.run(ctx, Cmd{Script: script, Sudo: sudo, Label: "stat " + path})
	if err != nil {
		return FileInfo{}, err
	}
	if r.Code != 0 {
		return FileInfo{}, fmt.Errorf("stat %s: exit code %d: %s", path, r.Code, r.Err())
	}
	var fi FileInfo
	for _, line := range strings.Split(string(r.Stdout), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "type":
			fi.Type = v
		case "target":
			fi.Target = v
		case "sha256":
			fi.SHA256 = v
		case "mode":
			n, err := strconv.ParseUint(strings.TrimSpace(v), 8, 32)
			if err == nil {
				fi.Mode = os.FileMode(n)
			}
		}
	}
	if fi.Type == "" {
		return FileInfo{}, fmt.Errorf("stat %s: no result", path)
	}
	fi.Exists = fi.Type != "none"
	if !fi.Exists {
		fi.Type = ""
	}
	return fi, nil
}
