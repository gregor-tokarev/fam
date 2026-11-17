package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gregor-tokarev/fam/internal/config"
	"github.com/gregor-tokarev/fam/internal/engine"
	"github.com/gregor-tokarev/fam/internal/output"
	"github.com/gregor-tokarev/fam/internal/pklbin"
	"github.com/gregor-tokarev/fam/internal/schema"
	"github.com/gregor-tokarev/fam/internal/selfupdate"
	"github.com/gregor-tokarev/fam/internal/transport"
	"github.com/gregor-tokarev/fam/internal/version"
	"github.com/spf13/cobra"
)

type doctorCheck struct {
	Status string `json:"status"`
	Check  string `json:"check"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func doctorCmd(o *Options) *cobra.Command {
	var offline bool
	c := &cobra.Command{Use: "doctor", Short: "Diagnose the project, editor, and machine connections", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := o.resolve(); err != nil {
			return err
		}
		checks := []doctorCheck{}
		add := func(status, name, detail, fix string) {
			checks = append(checks, doctorCheck{status, name, strings.Join(strings.Fields(detail), " "), fix})
		}
		add("ok", "fam", version.Version, "")
		if !offline {
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			latest, err := selfupdate.Latest(ctx)
			cancel()
			if err == nil && selfupdate.Compare(latest, version.Version) > 0 {
				add("warn", "release", "FAM "+latest+" is available", "fam update")
			}
		}
		pkg, err := schema.Current()
		if err != nil {
			return err
		}
		pin, err := schema.ProjectPin(o.Base)
		switch {
		case err != nil:
			add("fail", "PklProject", err.Error(), "fam migrate")
		case pin != pkg.Version:
			add("fail", "PklProject", fmt.Sprintf("pins FAM %s, this binary is %s", pin, pkg.Version), "fam migrate")
		default:
			add("ok", "PklProject", "pins FAM "+pin, "")
		}
		pv, sum, err := schema.Pinned(o.Base)
		switch {
		case err != nil:
			add("fail", "PklProject.deps.json", err.Error(), "fam migrate")
		case pv != pin || pv != pkg.Version || sum != pkg.Checksum:
			add("fail", "PklProject.deps.json", "does not match PklProject and this binary", "fam migrate")
		default:
			add("ok", "PklProject.deps.json", "matches the package pin", "")
		}
		if err := editorState(o.Base, pkg); err != nil {
			add("warn", "editor", err.Error()+". Restart the editor language server after migration", "fam migrate")
		} else {
			add("ok", "editor", ".pkl-lsp/projects.json matches the package pin", "")
		}
		cache, err := schema.CacheDir()
		if err != nil {
			add("fail", "Pkl cache", err.Error(), "fam migrate")
		} else if err := schema.CacheStatus(cache, pkg); err != nil {
			add("fail", "Pkl cache", err.Error(), "fam migrate")
		} else {
			add("ok", "Pkl cache", cache, "")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			add("fail", "editor cache", err.Error(), "fam migrate")
		} else {
			cache = filepath.Join(home, ".pkl", "cache")
			if err := schema.CacheStatus(cache, pkg); err != nil {
				add("fail", "editor cache", err.Error(), "fam migrate")
			} else {
				add("ok", "editor cache", cache, "")
			}
		}
		if bin, err := pklbin.CachedPath(); err != nil {
			add("fail", "pkl", err.Error(), "fam internal pkl-path")
		} else {
			add("ok", "pkl", version.PklVersion+" at "+bin, "")
		}
		result := engine.CheckProject(cmd.Context(), o.project())
		status := "ok"
		fix := ""
		detail := plural(result.Errors, "error") + ", " + plural(result.Warnings, "warning")
		if !result.OK {
			status = "fail"
			fix = "fam check"
		} else if result.Warnings > 0 {
			status = "warn"
			fix = "fam check"
		}
		add(status, "config", detail, fix)
		{
			machines := result.Machines
			if len(o.Machines) > 0 {
				machines = nil
				for _, name := range o.Machines {
					found := false
					for _, m := range result.Machines {
						if m.Name == name {
							machines = append(machines, m)
							found = true
						}
					}
					if !found {
						add("fail", "machine "+name, "no machine with this name", "fam check")
					}
				}
			}
			if offline {
				var local []config.Machine
				for _, m := range machines {
					if m.IsCockpit() {
						local = append(local, m)
					}
				}
				machines = local
			}
			reach := make([]doctorCheck, len(machines))
			var wg sync.WaitGroup
			for i, m := range machines {
				wg.Add(1)
				go func(i int, m config.Machine) {
					defer wg.Done()
					entry := doctorCheck{Status: "ok", Check: "machine " + m.Name}
					if m.IsCockpit() {
						entry.Detail = "cockpit is local. SSH is not required"
						reach[i] = entry
						return
					}
					if m.Address == "" {
						entry.Status = "fail"
						entry.Detail = "no SSH address"
						entry.Fix = "fam check"
						reach[i] = entry
						return
					}
					t := transport.NewSSH(m.Name, m.Target(), o.SSHConfig, nil)
					t.ConnectTimeout = 5 * time.Second
					r, err := t.Run(cmd.Context(), transport.Cmd{Script: "true", Timeout: 10 * time.Second, Label: "doctor SSH"})
					if err != nil || r.Code != 0 {
						entry.Status = "fail"
						entry.Detail = "SSH connection failed"
						if err != nil {
							entry.Detail = err.Error()
						}
						entry.Fix = "ssh "
						if o.SSHConfig != "" {
							entry.Fix += "-F " + transport.Quote(o.SSHConfig) + " "
						}
						entry.Fix += transport.Quote(m.Target()) + " true"
					} else {
						entry.Detail = "SSH runs true"
					}
					_ = t.Close()
					reach[i] = entry
				}(i, m)
			}
			wg.Wait()
			for _, r := range reach {
				add(r.Status, r.Check, r.Detail, r.Fix)
			}
		}
		ok := true
		for _, c := range checks {
			if c.Status == "fail" {
				ok = false
			}
		}
		resultOut := struct {
			OK     bool          `json:"ok"`
			Checks []doctorCheck `json:"checks"`
		}{ok, checks}
		if err := output.WriteValue(cmd.OutOrStdout(), resultOut, o.format(), func() {
			for _, c := range checks {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s: %s\n", c.Status, c.Check, c.Detail)
				if c.Fix != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "fix: %s\n", c.Fix)
				}
			}
		}); err != nil {
			return err
		}
		if !ok {
			return ExitError{1}
		}
		return nil
	}}
	c.Flags().BoolVar(&offline, "offline", false, "skip release and SSH network checks")
	return c
}
func editorState(base string, p *schema.Package) error {
	b, err := os.ReadFile(filepath.Join(base, ".pkl-lsp", "projects.json"))
	if err != nil {
		return err
	}
	var state struct {
		PklProjects []struct {
			ProjectFile          string            `json:"projectFile"`
			DeclaredDependencies map[string]string `json:"declaredDependencies"`
			ResolvedDependencies map[string]struct {
				URI       string            `json:"uri"`
				Checksums map[string]string `json:"checksums"`
			} `json:"resolvedDependencies"`
		} `json:"pklProjects"`
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return err
	}
	for _, e := range state.PklProjects {
		if e.ProjectFile == schema.ProjectFile && e.DeclaredDependencies["fam"] == schema.URI(p.Version) {
			for _, d := range e.ResolvedDependencies {
				if d.URI == schema.URI(p.Version) && d.Checksums["sha256"] == p.Checksum {
					return nil
				}
			}
		}
	}
	return fmt.Errorf(".pkl-lsp/projects.json does not pin the package of this binary")
}
