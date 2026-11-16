// Package secrets reads the secrets file of the cockpit and writes it in a form that shells read safely.
package secrets

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Secret is one NAME=value pair.
type Secret struct {
	Name  string
	Value string
}

var nameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ExpandHome expands `~/` on the cockpit.
func ExpandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(p[1:], "/")), nil
	}
	return filepath.Abs(p)
}

// Load reads the secrets file. It stops when the file is inside base or its mode is not 0600.
// Errors name the variables, never the values.
func Load(path, base string) ([]Secret, error) {
	p, err := ExpandHome(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("secrets file: %w", err)
	}
	if st.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("the secrets file %s has mode %04o. FAM needs mode 0600: run `chmod 600 %s`", p, st.Mode().Perm(), p)
	}
	if inside(p, base) {
		return nil, fmt.Errorf("the secrets file %s is inside the project base %s. Move it out of the project, so that it does not go into Git", p, base)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

func inside(p, base string) bool {
	rp, err1 := filepath.EvalSymlinks(p)
	rb, err2 := filepath.EvalSymlinks(base)
	if err1 != nil {
		rp = p
	}
	if err2 != nil {
		rb = base
	}
	rel, err := filepath.Rel(rb, rp)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Parse reads `NAME=value` lines. Empty lines and lines that start with `#` are comments.
// A value can be in single or double quotes. FAM rejects a value that it cannot write safely,
// for example a value with a new line.
func Parse(data []byte) ([]Secret, error) {
	var out []Secret
	seen := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimRight(sc.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")
		name, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			return nil, fmt.Errorf("secrets file line %d: no `=`. Use NAME=value", lineNo)
		}
		name = strings.TrimSpace(name)
		if !nameRe.MatchString(name) {
			return nil, fmt.Errorf("secrets file line %d: %q is not a valid variable name", lineNo, name)
		}
		v, err := unquote(value)
		if err != nil {
			return nil, fmt.Errorf("secrets file line %d: the value of %s %v. FAM cannot write it safely", lineNo, name, err)
		}
		for _, r := range v {
			if r == 0 || r == '\n' || r == '\r' || (r < 0x20 && r != '\t') || r == 0x7f {
				return nil, fmt.Errorf("secrets file line %d: the value of %s contains a control character or a new line. FAM cannot write it safely", lineNo, name)
			}
		}
		if i, ok := seen[name]; ok {
			out[i].Value = v
			continue
		}
		seen[name] = len(out)
		out = append(out, Secret{Name: name, Value: v})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// unquote reads a value. A value that starts with a quote is read like a shell word:
// single-quoted parts, double-quoted parts (with \", \\, \$, \` escapes), and \x outside
// quotes, joined together. So `'it'\”s'` (the form that Render writes) reads as `it's`.
// Any other value is taken as it is, without the spaces around it.
func unquote(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" || (v[0] != '\'' && v[0] != '"') {
		return v, nil
	}
	var b strings.Builder
	for i := 0; i < len(v); {
		c := v[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(v[i+1:], '\'')
			if j < 0 {
				return "", fmt.Errorf("has an open single quote (a value over more than one line has a new line)")
			}
			b.WriteString(v[i+1 : i+1+j])
			i += j + 2
		case c == '"':
			i++
			closed := false
			for i < len(v) {
				d := v[i]
				if d == '"' {
					closed = true
					i++
					break
				}
				if d == '\\' && i+1 < len(v) {
					switch n := v[i+1]; n {
					case '"', '\\', '$', '`':
						b.WriteByte(n)
						i += 2
						continue
					case 'n', 'r':
						return "", fmt.Errorf("contains an escaped new line")
					}
				}
				b.WriteByte(d)
				i++
			}
			if !closed {
				return "", fmt.Errorf("has an open double quote (a value over more than one line has a new line)")
			}
		case c == '\\' && i+1 < len(v):
			b.WriteByte(v[i+1])
			i += 2
		case c == ' ' || c == '\t':
			if strings.TrimSpace(v[i:]) != "" {
				return "", fmt.Errorf("has text after the closing quote")
			}
			i = len(v)
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// Header is the first line of the secrets file that FAM writes on a machine.
const Header = "# Written by FAM. Do not edit. FAM replaces this file on each apply.\n"

// Render writes the secrets as `export NAME='value'` lines that sh, Bash, and zsh read safely.
func Render(list []Secret) []byte {
	var b strings.Builder
	b.WriteString(Header)
	for _, s := range list {
		fmt.Fprintf(&b, "export %s='%s'\n", s.Name, strings.ReplaceAll(s.Value, "'", `'\''`))
	}
	return []byte(b.String())
}

// Names returns the sorted variable names.
func Names(list []Secret) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}
