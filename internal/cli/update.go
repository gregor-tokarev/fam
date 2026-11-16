package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/gregor-tokarev/fam/internal/selfupdate"
	"github.com/gregor-tokarev/fam/internal/version"
)

func updateCmd() *cobra.Command {
	var target string
	var check bool
	c := &cobra.Command{
		Use:   "update",
		Short: "Update fam itself to the latest release (`fam upgrade` updates the .fam/ folder of a project)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			cur := version.Version
			want := target
			if want == "" {
				latest, err := selfupdate.Latest(cmd.Context())
				if err != nil {
					return err
				}
				want = latest
				if selfupdate.Compare(cur, latest) >= 0 {
					fmt.Fprintf(out, "fam %s is up to date (latest release: %s)\n", cur, latest)
					return nil
				}
			}
			if check {
				cmdline := "fam update"
				if target != "" {
					if err := selfupdate.Exists(cmd.Context(), target); err != nil {
						return fmt.Errorf("release %s: %w", target, err)
					}
					cmdline += " --version " + target
				}
				fmt.Fprintf(out, "fam %s is available (this is fam %s). Run `%s` to install it\n", want, cur, cmdline)
				return nil
			}
			exe, err := selfupdate.Executable()
			if err != nil {
				return err
			}
			if err := selfupdate.Install(cmd.Context(), want, exe); err != nil {
				return err
			}
			fmt.Fprintf(out, "Updated %s from fam %s to fam %s\n", exe, cur, want)
			fmt.Fprintln(out, "When this release changes the schema, `fam plan` asks you to run `fam upgrade` in a project")
			return nil
		},
	}
	c.Flags().StringVar(&target, "version", "", "install this release (for example 0.2.0) instead of the latest")
	c.Flags().BoolVar(&check, "check", false, "only show whether a newer release exists")
	return c
}
