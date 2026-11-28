package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/apple/pkl-go/pkl"
	"github.com/gregor-tokarev/fam/internal/transport"
	"github.com/pelletier/go-toml/v2"
)

// omap is a JSON or TOML table that keeps the order of its keys.
type omap struct {
	keys []string
	vals map[string]any
}

func newOmap() *omap { return &omap{vals: map[string]any{}} }

func (m *omap) get(k string) (any, bool) { v, ok := m.vals[k]; return v, ok }

func (m *omap) set(k string, v any) {
	if _, ok := m.vals[k]; !ok {
		m.keys = append(m.keys, k)
	}
	m.vals[k] = v
}

func (m *omap) del(k string) {
	if _, ok := m.vals[k]; !ok {
		return
	}
	delete(m.vals, k)
	for i, key := range m.keys {
		if key == k {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			break
		}
	}
}

// --- conversion from Pkl ---

// fromPkl converts a value of a Pkl Dynamic object into omap, []any, and scalars.
func fromPkl(v any) (any, error) {
	switch x := v.(type) {
	case pkl.Object:
		return objectFromPkl(&x)
	case *pkl.Object:
		return objectFromPkl(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			c, err := fromPkl(e)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	case map[any]any:
		m := newOmap()
		keys := make([]string, 0, len(x))
		conv := map[string]any{}
		for k, e := range x {
			ks := fmt.Sprint(k)
			keys = append(keys, ks)
			conv[ks] = e
		}
		sort.Strings(keys)
		for _, k := range keys {
			c, err := fromPkl(conv[k])
			if err != nil {
				return nil, err
			}
			m.set(k, c)
		}
		return m, nil
	case map[string]any:
		m := newOmap()
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			c, err := fromPkl(x[k])
			if err != nil {
				return nil, err
			}
			m.set(k, c)
		}
		return m, nil
	case int:
		return int64(x), nil
	case int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		var n int64
		fmt.Sscan(fmt.Sprint(x), &n)
		return n, nil
	case float32:
		return float64(x), nil
	case string, bool, float64, nil:
		return x, nil
	default:
		return nil, fmt.Errorf("Merge cannot write a value of type %T", v)
	}
}

func objectFromPkl(o *pkl.Object) (any, error) {
	if len(o.Properties) == 0 && len(o.Entries) == 0 && len(o.Elements) > 0 {
		return fromPkl(o.Elements)
	}
	if len(o.Elements) > 0 {
		return nil, fmt.Errorf("an object has both keys and list elements. A JSON, TOML, or YAML value is either an object or a list")
	}
	m := newOmap()
	pm, err := fromPkl(o.Properties)
	if err != nil {
		return nil, err
	}
	em, err := fromPkl(o.Entries)
	if err != nil {
		return nil, err
	}
	for _, src := range []*omap{pm.(*omap), em.(*omap)} {
		for _, k := range src.keys {
			m.set(k, src.vals[k])
		}
	}
	return m, nil
}

// --- JSON with key order ---

func parseJSON(data []byte) (*omap, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return newOmap(), nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeJSON(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("extra data after the JSON value")
	}
	m, ok := v.(*omap)
	if !ok {
		return nil, fmt.Errorf("the JSON file is not an object")
	}
	return m, nil
}

func decodeJSON(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			m := newOmap()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := decodeJSON(dec)
				if err != nil {
					return nil, err
				}
				m.set(kt.(string), v)
			}
			_, err := dec.Token()
			return m, err
		case '[':
			var out []any
			for dec.More() {
				v, err := decodeJSON(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			_, err := dec.Token()
			if out == nil {
				out = []any{}
			}
			return out, err
		}
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n, nil
		}
		f, err := t.Float64()
		return f, err
	}
	return tok, nil
}

func writeJSON(buf *bytes.Buffer, v any, indent string) error {
	switch x := v.(type) {
	case *omap:
		if len(x.keys) == 0 {
			buf.WriteString("{}")
			return nil
		}
		buf.WriteString("{\n")
		for i, k := range x.keys {
			kb, _ := json.Marshal(k)
			buf.WriteString(indent + "  ")
			buf.Write(kb)
			buf.WriteString(": ")
			if err := writeJSON(buf, x.vals[k], indent+"  "); err != nil {
				return err
			}
			if i < len(x.keys)-1 {
				buf.WriteString(",")
			}
			buf.WriteString("\n")
		}
		buf.WriteString(indent + "}")
	case []any:
		if len(x) == 0 {
			buf.WriteString("[]")
			return nil
		}
		buf.WriteString("[\n")
		for i, e := range x {
			buf.WriteString(indent + "  ")
			if err := writeJSON(buf, e, indent+"  "); err != nil {
				return err
			}
			if i < len(x)-1 {
				buf.WriteString(",")
			}
			buf.WriteString("\n")
		}
		buf.WriteString(indent + "]")
	default:
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		var tmp bytes.Buffer
		enc = json.NewEncoder(&tmp)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(x); err != nil {
			return err
		}
		buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
	}
	return nil
}

// --- TOML ---

func parseTOML(data []byte) (*omap, error) {
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return fromTOML(m).(*omap), nil
}

// fromTOML converts a decoded TOML value. Dates and times stay as they are.
func fromTOML(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := newOmap()
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			m.set(k, fromTOML(x[k]))
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = fromTOML(e)
		}
		return out
	case int:
		return int64(x)
	}
	return v
}

func toPlain(v any) any {
	switch x := v.(type) {
	case *omap:
		out := make(map[string]any, len(x.keys))
		for _, k := range x.keys {
			out[k] = toPlain(x.vals[k])
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toPlain(e)
		}
		return out
	}
	return v
}

// --- merge ---

func mergeInto(dst *omap, src *omap, prefix string, changed *[]string) {
	for _, k := range src.keys {
		sv := src.vals[k]
		dv, ok := dst.get(k)
		key := prefix + k
		if sm, isMap := sv.(*omap); isMap {
			dm, dIsMap := dv.(*omap)
			if !ok || !dIsMap {
				dm = newOmap()
				dst.set(k, dm)
			}
			mergeInto(dm, sm, key+".", changed)
			if (!ok || !dIsMap) && len(sm.keys) == 0 {
				*changed = append(*changed, key)
			}
			continue
		}
		if !ok || !equalValues(dv, sv) {
			dst.set(k, sv)
			*changed = append(*changed, key)
		}
	}
}

func removeFrom(dst *omap, src *omap, prefix string, removed *[]string) {
	for _, k := range src.keys {
		dv, ok := dst.get(k)
		if !ok {
			continue
		}
		key := prefix + k
		if sm, isMap := src.vals[k].(*omap); isMap {
			if dm, dIsMap := dv.(*omap); dIsMap {
				removeFrom(dm, sm, key+".", removed)
				if len(dm.keys) == 0 {
					dst.del(k)
				}
				continue
			}
		}
		dst.del(k)
		*removed = append(*removed, key)
	}
}

func equalValues(a, b any) bool {
	switch x := a.(type) {
	case *omap:
		y, ok := b.(*omap)
		if !ok || len(x.keys) != len(y.keys) {
			return false
		}
		for _, k := range x.keys {
			yv, ok := y.vals[k]
			if !ok || !equalValues(x.vals[k], yv) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !equalValues(x[i], y[i]) {
				return false
			}
		}
		return true
	case int64:
		switch y := b.(type) {
		case int64:
			return x == y
		case float64:
			return float64(x) == y
		}
		return false
	case float64:
		switch y := b.(type) {
		case int64:
			return x == float64(y)
		case float64:
			return x == y
		}
		return false
	default:
		return fmt.Sprintf("%T:%v", a, a) == fmt.Sprintf("%T:%v", b, b)
	}
}

// MergeTask merges keys into a JSON or TOML file.
type MergeTask struct {
	base
	Path   string
	Format string
	Values *omap
	Mode   os.FileMode
	Absent bool
	Label  string
	units  []Unit
}

// Kind implements Task.
func (t *MergeTask) Kind() string { return "merge" }

// Name implements Task.
func (t *MergeTask) Name() string {
	if t.Label != "" {
		return t.Label
	}
	return t.Path
}

// Restarts implements Restarter.
func (t *MergeTask) Restarts() []Unit { return t.units }

func (t *MergeTask) compute(ctx context.Context, env *Env) (out []byte, keys []string, exists bool, mode os.FileMode, err error) {
	p := env.Path(t.Path)
	data, fi, err := env.T.ReadFileInfo(ctx, p, t.sudo)
	if err != nil {
		return nil, nil, false, 0, err
	}
	exists = fi.Exists
	mode = t.Mode
	if exists && fi.Type == "file" {
		mode = fi.Mode.Perm()
	}
	var doc *omap
	switch t.Format {
	case "json":
		doc, err = parseJSON(data)
	case "toml":
		doc, err = parseTOML(data)
	default:
		return nil, nil, false, 0, fmt.Errorf("unknown merge format %q", t.Format)
	}
	if err != nil {
		return nil, nil, false, 0, fmt.Errorf("%s is not valid %s: %v. FAM does not change it", p, strings.ToUpper(t.Format), err)
	}
	if t.Absent {
		removeFrom(doc, t.Values, "", &keys)
	} else {
		mergeInto(doc, t.Values, "", &keys)
	}
	if len(keys) == 0 {
		return nil, nil, exists, mode, nil
	}
	switch t.Format {
	case "json":
		var buf bytes.Buffer
		if err := writeJSON(&buf, doc, ""); err != nil {
			return nil, nil, false, 0, err
		}
		buf.WriteString("\n")
		out = buf.Bytes()
	case "toml":
		out, err = toml.Marshal(toPlain(doc))
		if err != nil {
			return nil, nil, false, 0, err
		}
	}
	return out, keys, exists, mode, nil
}

// Check implements Task.
func (t *MergeTask) Check(ctx context.Context, env *Env) (Check, error) {
	_, keys, exists, _, err := t.compute(ctx, env)
	if err != nil {
		return Check{}, err
	}
	if len(keys) == 0 {
		return Check{Status: Satisfied}, nil
	}
	verb := "set "
	if t.Absent {
		verb = "remove "
	} else if !exists {
		verb = "create with "
	}
	return Check{Status: Change, Detail: verb + strings.Join(keys, ", ")}, nil
}

// Apply implements Task.
func (t *MergeTask) Apply(ctx context.Context, env *Env) (string, error) {
	out, keys, _, mode, err := t.compute(ctx, env)
	if err != nil {
		return "", err
	}
	if len(keys) == 0 {
		return "no change", nil
	}
	if err := env.T.WriteFile(ctx, env.Path(t.Path), out, mode, t.sudo); err != nil {
		return "", sudoHint(err, t.sudo, "merge")
	}
	return "merged", nil
}

func (t *MergeTask) checkBytes(p string, data []byte) ([]string, error) {
	var doc *omap
	var err error
	switch t.Format {
	case "json":
		doc, err = parseJSON(data)
	case "toml":
		doc, err = parseTOML(data)
	default:
		return nil, fmt.Errorf("unknown merge format %q", t.Format)
	}
	if err != nil {
		return nil, fmt.Errorf("%s is not valid %s: %v. FAM does not change it", p, strings.ToUpper(t.Format), err)
	}
	var keys []string
	if t.Absent {
		removeFrom(doc, t.Values, "", &keys)
	} else {
		mergeInto(doc, t.Values, "", &keys)
	}
	return keys, nil
}

// ApplyChecked implements FastApplier: reads and merges once, writes and verifies the digest on
// disk in a single round trip (2 round trips total instead of 4), and re-runs the semantic merge
// check on the written content so format-specific omissions (e.g. null in TOML) are still caught.
func (t *MergeTask) ApplyChecked(ctx context.Context, env *Env) (bool, Status, string, error, touch) {
	vw, ok := env.T.(transport.VerifiedWriter)
	if !ok {
		return false, "", "", nil, touchNone
	}
	out, keys, exists, mode, err := t.compute(ctx, env)
	if err != nil {
		return true, Failed, "check failed", err, touchNone
	}
	if len(keys) == 0 {
		return true, Unchanged, "", nil, touchNone
	}
	verb := "set "
	if t.Absent {
		verb = "remove "
	} else if !exists {
		verb = "create with "
	}
	checkDetail := verb + strings.Join(keys, ", ")
	p := env.Path(t.Path)
	fi, err := vw.WriteFileVerify(ctx, p, out, mode, t.sudo)
	if err != nil {
		return true, Failed, checkDetail, sudoHint(err, t.sudo, "merge"), touchApply
	}
	if !fi.Exists || fi.Type != "file" || fi.SHA256 != sha(out) {
		return true, Failed, checkDetail, fmt.Errorf("the check still finds a difference after apply"), touchApply
	}
	remKeys, err := t.checkBytes(p, out)
	if err != nil {
		return true, Failed, "check after apply failed", err, touchApply
	}
	if len(remKeys) > 0 {
		remVerb := "set "
		if t.Absent {
			remVerb = "remove "
		}
		return true, Failed, checkDetail, fmt.Errorf("the check still finds a difference after apply: %s%s", remVerb, strings.Join(remKeys, ", ")), touchApply
	}
	return true, Changed, checkDetail, nil, touchApply
}
