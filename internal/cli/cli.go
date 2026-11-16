// Package cli implements the `fam` command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gregor-tokarev/fam/internal/pklbin"
	"github.com/gregor-tokarev/fam/internal/project"
	"github.com/gregor-tokarev/fam/internal/version"
)

// Options are the global flags.
type Options struct {
	Base             string
	Config           string
	Machines         []string
	Serial           bool
	JSON             bool
	TOON             bool
	SudoPasswordFile string
	SSHConfig        string
	InstructionsDir  string
	SkillsDir        string
}

// ExitError carries an exit code without an extra message.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit %d", e.Code) }

// Execute runs the command line and returns the process exit code.
func Execute() int {
	opts := &Options{}
	root := &cobra.Command{
		Use:           "fam",
		Short:         "FAM installs working tools, skills, and agent instructions on every machine of your agent fleet",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&opts.Base, "base", ".", "project base directory")
	pf.StringVar(&opts.Config, "config", "", "entrypoint Pkl file (default <base>/config.pkl)")
	pf.StringSliceVar(&opts.Machines, "machine", nil, "run only on this machine (repeat for more)")
	pf.BoolVar(&opts.Serial, "serial", false, "run machines one at a time")
	pf.BoolVar(&opts.JSON, "json", false, "write the result as JSON")
	pf.BoolVar(&opts.TOON, "toon", false, "write the result as TOON")
	pf.StringVar(&opts.SudoPasswordFile, "sudo-password-file", "", "file with the sudo password of the cockpit, for runs without a terminal")
	pf.StringVar(&opts.SSHConfig, "ssh-config", os.Getenv("FAM_SSH_CONFIG"), "ssh config file that FAM gives to `ssh -F` (default: the normal ssh configuration)")

	root.AddCommand(newCmd(opts), upgradeCmd(opts), updateCmd(), versionCmd(), internalCmd())
	addRunCommands(root, opts)

	if err := root.ExecuteContext(context.Background()); err != nil {
		var ee ExitError
		if errors.As(err, &ee) {
			return ee.Code
		}
		fmt.Fprintln(os.Stderr, "fam: "+err.Error())
		return 1
	}
	return 0
}

func (o *Options) resolve() error {
	if o.JSON && o.TOON {
		return errors.New("use --json or --toon, not both")
	}
	abs, err := filepath.Abs(o.Base)
	if err != nil {
		return err
	}
	o.Base = abs
	if o.Config == "" {
		o.Config = filepath.Join(o.Base, "config.pkl")
	} else if o.Config, err = filepath.Abs(o.Config); err != nil {
		return err
	}
	for _, d := range []*string{&o.InstructionsDir, &o.SkillsDir, &o.SudoPasswordFile, &o.SSHConfig} {
		if *d != "" {
			if *d, err = filepath.Abs(*d); err != nil {
				return err
			}
		}
	}
	return nil
}

func newCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "new",
		Short: "Create the project tree",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.resolve(); err != nil {
				return err
			}
			written, err := project.New(project.NewOptions{Base: opts.Base})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Created a FAM project in %s:\n", opts.Base)
			for _, w := range written {
				fmt.Fprintf(out, "  %s\n", w)
			}
			fmt.Fprintln(out, "Next: add machines to machines.pkl, then run `fam plan`.")
			return nil
		},
	}
}

func upgradeCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Update .fam/ to the schema of this FAM version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.resolve(); err != nil {
				return err
			}
			old, err := project.Upgrade(opts.Base)
			if err != nil {
				return err
			}
			if old == "" {
				old = "none"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated %s from FAM %s to FAM %s\n", filepath.Join(opts.Base, ".fam"), old, version.Version)
			return nil
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the FAM version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "fam %s (pkl %s)\n", version.Version, version.PklVersion)
		},
	}
}

func internalCmd() *cobra.Command {
	c := &cobra.Command{Use: "internal", Hidden: true}
	c.AddCommand(&cobra.Command{
		Use:   "pkl-path",
		Short: "Download the pinned pkl binary when necessary and print its path",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := pklbin.Path(cmd.Context(), os.Stderr)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), p)
			return nil
		},
	})
	return c
}

func joinNames(names []string) string { return strings.Join(names, ", ") }
