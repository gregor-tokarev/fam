package output

import (
	"sort"
	"strings"
)

// Mask replaces a secret value.
const Mask = "[secret]"

// Redactor replaces secret values in text.
type Redactor struct {
	r *strings.Replacer
}

// NewRedactor returns a redactor for the values. Longer values go first,
// so that a value inside another value does not leave a part of the longer one.
func NewRedactor(values []string) *Redactor {
	// Also hide the forms in which a value can appear: without its outer spaces (FAM trims
	// output), and with single quotes escaped for a shell ('\'').
	var vs []string
	for _, v := range values {
		vs = append(vs, v)
		if t := strings.TrimSpace(v); t != v && t != "" {
			vs = append(vs, t)
		}
		// Each shell quoting layer replaces ' with '\''; FAM nests up to three layers.
		q := v
		for i := 0; i < 3 && strings.Contains(q, "'"); i++ {
			q = strings.ReplaceAll(q, "'", `'\''`)
			vs = append(vs, q)
		}
	}
	sort.Slice(vs, func(i, j int) bool { return len(vs[i]) > len(vs[j]) })
	var pairs []string
	for _, v := range vs {
		if strings.TrimSpace(v) == "" {
			continue
		}
		pairs = append(pairs, v, Mask)
	}
	if len(pairs) == 0 {
		return &Redactor{}
	}
	return &Redactor{r: strings.NewReplacer(pairs...)}
}

// String hides the secret values in s.
func (r *Redactor) String(s string) string {
	if r == nil || r.r == nil {
		return s
	}
	return r.r.Replace(s)
}

// Redact hides the secret values in the free-text fields of the result: names, details,
// errors, and output. FAM never changes enumerated fields (status, kind, command), so a secret
// value such as "error" cannot change the meaning of the result.
func Redact(run *Run, values []string) {
	rd := NewRedactor(values)
	if rd.r == nil {
		return
	}
	s := rd.String
	run.Error = s(run.Error)
	for i := range run.Machines {
		m := &run.Machines[i]
		m.Name, m.Target, m.Error = s(m.Name), s(m.Target), s(m.Error)
		if f := m.Facts; f != nil {
			f.Machine, f.Hostname, f.User, f.Home = s(f.Machine), s(f.Hostname), s(f.User), s(f.Home)
		}
		for j := range m.Warnings {
			m.Warnings[j] = s(m.Warnings[j])
		}
		for j := range m.Tasks {
			t := &m.Tasks[j]
			t.Group, t.Name, t.Detail, t.Error = s(t.Group), s(t.Name), s(t.Detail), s(t.Error)
		}
		for j := range m.Restarts {
			m.Restarts[j].Unit, m.Restarts[j].Error = s(m.Restarts[j].Unit), s(m.Restarts[j].Error)
		}
		for j := range m.Verify {
			v := &m.Verify[j]
			v.Bundle, v.Name, v.Output = s(v.Bundle), s(v.Name), s(v.Output)
		}
	}
}
