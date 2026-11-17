package output

import (
	"fmt"
	"io"
	"strings"
)

var symbols = map[string]string{
	"satisfied": "ok",
	"unchanged": "ok",
	"change":    "~",
	"changed":   "+",
	"failed":    "x",
	"error":     "x",
	"conflict":  "!",
	"skipped":   "-",
	"passed":    "ok",
}

// WriteText writes the result for a person.
func WriteText(w io.Writer, r *Run) {
	for _, m := range r.Machines {
		target := m.Transport
		if m.Target != "" {
			target += " " + m.Target
		}
		fmt.Fprintf(w, "== %s (%s, %s): %s\n", m.Name, m.Role, target, m.Status)
		if m.Facts != nil {
			f := m.Facts
			fmt.Fprintf(w, "   %s %s %s, %s, %d CPUs, user %s\n", f.OS, f.Distro, f.DistroVersion, f.Arch, f.CPUs, f.User)
		}
		for _, warning := range m.Warnings {
			fmt.Fprintf(w, "   warning: %s\n", warning)
		}
		group := ""
		for _, t := range m.Tasks {
			if t.Group != group {
				group = t.Group
				fmt.Fprintf(w, "   %s\n", group)
			}
			line := fmt.Sprintf("     %-3s %-9s %s %s", symbols[t.Status], t.Status, t.Kind, t.Name)
			if t.Detail != "" {
				line += ": " + t.Detail
			}
			fmt.Fprintln(w, line)
			if t.Error != "" {
				for _, l := range strings.Split(t.Error, "\n") {
					fmt.Fprintf(w, "         %s\n", l)
				}
			}
		}
		for _, rs := range m.Restarts {
			line := fmt.Sprintf("   restart %s (%s): %s", rs.Unit, rs.Scope, rs.Status)
			if rs.Error != "" {
				line += ": " + rs.Error
			}
			fmt.Fprintln(w, line)
		}
		if len(m.Verify) > 0 {
			fmt.Fprintln(w, "   verify")
			for _, v := range m.Verify {
				fmt.Fprintf(w, "     %-3s %-6s %s [%s, %s]", symbols[v.Status], v.Status, v.Name, v.Event, v.Shell)
				if v.Status != "passed" {
					fmt.Fprintf(w, ": exit code %d", v.Code)
					if v.Output != "" {
						fmt.Fprintf(w, ": %s", strings.ReplaceAll(v.Output, "\n", " | "))
					}
				}
				fmt.Fprintln(w)
			}
		}
		if m.Error != "" {
			fmt.Fprintf(w, "   error (%s): %s\n", m.ErrorKind, strings.ReplaceAll(m.Error, "\n", "\n   "))
		}
		fmt.Fprintln(w)
	}
	c := r.Summary
	switch r.Command {
	case "plan":
		fmt.Fprintf(w, "Plan: %d satisfied, %d to change, %d conflicts, %d errors, %d skipped.\n", c.Satisfied, c.Change, c.Conflict, c.Error, c.Skipped)
	case "apply":
		fmt.Fprintf(w, "Apply: %d unchanged, %d changed, %d failed, %d conflicts, %d skipped. Verify: %d passed, %d failed.\n",
			c.Unchanged, c.Changed, c.Failed+c.Error, c.Conflict, c.Skipped, c.VerifyOK, c.VerifyBad)
	case "verify":
		fmt.Fprintf(w, "Verify: %d passed, %d failed.\n", c.VerifyOK, c.VerifyBad)
	}
	if r.Error != "" {
		fmt.Fprintf(w, "Error: %s\n", r.Error)
	}
	if r.LogFile != "" {
		fmt.Fprintf(w, "Run log: %s\n", r.LogFile)
	}
}
