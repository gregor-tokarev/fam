package cli

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/gregor-tokarev/fam/internal/output"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks"
	"github.com/gregor-tokarev/fam/internal/stdlib"
	"github.com/spf13/cobra"
)

func (o *Options) format() output.Format {
	if o.JSON {
		return output.JSON
	}
	if o.TOON {
		return output.TOON
	}
	return output.Text
}

type stdItem struct {
	Call    string `json:"call"`
	Summary string `json:"summary"`
}
type stdGroup struct {
	Group   string    `json:"group"`
	Bundles []stdItem `json:"bundles"`
}
type stdDetail struct {
	Call              string   `json:"call"`
	Doc               string   `json:"doc"`
	Name              string   `json:"name"`
	RequiredSecrets   []string `json:"requiredSecrets"`
	Platforms         []string `json:"platforms,omitempty"`
	UnsupportedReason string   `json:"unsupportedReason,omitempty"`
	Skills            []string `json:"skills"`
	Verify            []string `json:"verify"`
}

func stdCmd(o *Options) *cobra.Command {
	c := &cobra.Command{Use: "std", Short: "Find and inspect standard bundles"}
	c.AddCommand(&cobra.Command{Use: "list [group]", Short: "List standard bundle calls and summaries", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := o.resolve(); err != nil {
			return err
		}
		groups, err := stdlib.Catalogue(cmd.Context())
		if err != nil {
			return err
		}
		if len(args) > 0 {
			groups, err = stdlib.Select(groups, args[0])
			if err != nil {
				return err
			}
		}
		result := []stdGroup{}
		for _, g := range groups {
			row := stdGroup{Group: g.Name, Bundles: []stdItem{}}
			for _, b := range g.Bundles {
				row.Bundles = append(row.Bundles, stdItem{Call: "std." + g.Name + "." + b.Name + "()", Summary: stdlib.Summary(b.Doc)})
			}
			result = append(result, row)
		}
		return output.WriteValue(cmd.OutOrStdout(), result, o.format(), func() {
			for _, g := range result {
				fmt.Fprintf(cmd.OutOrStdout(), "%s:\n", g.Group)
				for _, b := range g.Bundles {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s\n", b.Call, b.Summary)
				}
			}
		})
	}})
	c.AddCommand(&cobra.Command{Use: "show <group>.<name>", Short: "Show bundle requirements, skills, and verify commands", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := o.resolve(); err != nil {
			return err
		}
		name := args[0]
		catalogue, err := stdlib.Catalogue(cmd.Context())
		if err != nil {
			return err
		}
		found := false
		var names []string
		for _, g := range catalogue {
			for _, e := range g.Bundles {
				n := g.Name + "." + e.Name
				names = append(names, n)
				found = found || n == name
			}
		}
		if !found {
			sort.Slice(names, func(i, j int) bool {
				a, b := distance(name, names[i]), distance(name, names[j])
				if a == b {
					return names[i] < names[j]
				}
				return a < b
			})
			if len(names) > 3 {
				names = names[:3]
			}
			detail := struct {
				Error   string   `json:"error"`
				Matches []string `json:"matches"`
			}{"unknown standard bundle " + name, names}
			if err := output.WriteValue(cmd.OutOrStdout(), detail, o.format(), func() {
				fmt.Fprintf(cmd.OutOrStdout(), "%s. Closest matches: %s\n", detail.Error, strings.Join(names, ", "))
			}); err != nil {
				return err
			}
			return ExitError{1}
		}
		d := stdDetail{Call: "std." + name + "()", RequiredSecrets: []string{}, Skills: []string{}, Verify: []string{}}
		var platforms []string
		restricted := false
		// Representative Linux libc/architecture pairs and supported macOS hosts.
		for _, platform := range []string{"linux-glibc-amd64", "linux-glibc-arm64", "linux-musl-amd64", "linux-musl-arm64", "darwin-amd64", "darwin-arm64"} {
			f := stdlib.LinuxFacts()
			parts := strings.Split(platform, "-")
			if parts[0] == "darwin" {
				f.OS = "darwin"
				f.Distro = "macos"
				f.DistroVersion = "15"
				f.Arch = parts[1]
				f.Libc = ""
				f.Init = "launchd"
			} else {
				f.Arch = parts[2]
				f.Libc = parts[1]
				if f.Libc == "musl" {
					f.Distro = "alpine"
					f.DistroVersion = "3.23"
				}
			}
			groups, err := stdlib.Lookup(cmd.Context(), f, name)
			if err != nil {
				return err
			}
			for _, g := range groups {
				for _, e := range g.Bundles {
					b := e.Bundle
					d.Name = b.Name
					d.Doc = e.Doc
					for _, n := range b.RequiredSecrets {
						d.RequiredSecrets = appendUnique(d.RequiredSecrets, n)
					}
					if b.Supported {
						platforms = append(platforms, platform)
					} else {
						restricted = true
					}
					if b.UnsupportedReason != nil {
						d.UnsupportedReason = *b.UnsupportedReason
					}
					for _, t := range b.Tasks {
						var n string
						switch x := t.(type) {
						case tasks.GitHubSkill:
							if x.GetState() == "absent" {
								continue
							}
							n = path.Base(x.GetPath())
							if x.GetName() != nil {
								n = *x.GetName()
							}
						case tasks.Skill:
							if x.GetState() == "absent" {
								continue
							}
							n = path.Base(x.GetPath())
							if n == "skill" {
								n = b.Name
							}
							if x.GetName() != nil {
								n = *x.GetName()
							}
						}
						if n != "" {
							d.Skills = appendUnique(d.Skills, n)
						}
					}
					for _, v := range b.Verify {
						d.Verify = appendUnique(d.Verify, v.Script)
					}
				}
			}
		}
		if restricted {
			d.Platforms = platforms
		} else {
			d.UnsupportedReason = ""
		}
		return output.WriteValue(cmd.OutOrStdout(), d, o.format(), func() {
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%s\n\n%s\n\nname: %s\nrequiredSecrets: %s\n", d.Call, d.Doc, d.Name, strings.Join(d.RequiredSecrets, ", "))
			if restricted {
				fmt.Fprintf(w, "supported platforms (representative facts): %s\n%s\n", strings.Join(d.Platforms, ", "), d.UnsupportedReason)
			}
			fmt.Fprintf(w, "skills: %s\nverify:\n", strings.Join(d.Skills, ", "))
			for _, v := range d.Verify {
				fmt.Fprintln(w, v)
			}
		})
	}})
	return c
}
func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}
func distance(a, b string) int {
	x, y := []rune(a), []rune(b)
	row := make([]int, len(y)+1)
	for i := range row {
		row[i] = i
	}
	for i, c := range x {
		prev := row[0]
		row[0] = i + 1
		for j, d := range y {
			old := row[j+1]
			cost := 0
			if c != d {
				cost = 1
			}
			row[j+1] = min(row[j]+1, row[j+1]+1, prev+cost)
			prev = old
		}
	}
	return row[len(y)]
}
