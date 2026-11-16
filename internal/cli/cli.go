// Package cli implements the `fam` command line.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gregor-tokarev/fam/internal/pklbin"
	"github.com/gregor-tokarev/fam/internal/project"
	"github.com/gregor-tokarev/fam/internal/schema"
	"github.com/gregor-tokarev/fam/internal/stdlib"
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

	root.AddCommand(newCmd(opts), migrateCmd(opts), updateCmd(), versionCmd(), internalCmd())
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

func migrateCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Pin the fam package of this FAM version in the PklProject of a project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.resolve(); err != nil {
				return err
			}
			old, err := project.Migrate(opts.Base)
			if err != nil {
				return err
			}
			if old == "" {
				old = "none"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Migrated %s from FAM %s to FAM %s\n", opts.Base, old, version.Version)
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
	var check bool
	docs := &cobra.Command{Use: "std-docs [file]", Short: "Write the standard bundle catalogue", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		dest := "docs/std.md"
		if len(args) > 0 {
			dest = args[0]
		}
		data, err := stdlib.Docs(cmd.Context())
		if err != nil {
			return err
		}
		if check {
			old, err := os.ReadFile(dest)
			if err != nil {
				return err
			}
			if !bytes.Equal(old, data) {
				return fmt.Errorf("%s is not current. Run go run ./cmd/fam internal std-docs", dest)
			}
			return nil
		}
		return os.WriteFile(dest, data, 0o644)
	}}
	docs.Flags().BoolVar(&check, "check", false, "fail when the catalogue is not current")
	c.AddCommand(docs)
	c.AddCommand(&cobra.Command{
		Use:   "package <dir>",
		Short: "Write the files of the fam Pkl package that a release publishes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := schema.Current()
			if err != nil {
				return err
			}
			return schema.WriteFiles(args[0], p)
		},
	})
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
	c.AddCommand(&cobra.Command{
		Use:   "install-pkl <dir>",
		Short: "Copy the pinned pkl binary to <dir>/pkl, for editors with the Pkl plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := pklbin.Path(cmd.Context(), os.Stderr)
			if err != nil {
				return err
			}
			dest, err := installPkl(src, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Installed pkl %s to %s\n", version.PklVersion, dest)
			return nil
		},
	})
	return c
}

// installPkl copies the pkl binary src to dir/pkl. It writes a new file next to the target, then
// renames it, so that a running pkl keeps working.
func installPkl(src, dir string) (string, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".pkl.new-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, "pkl")
	return dest, os.Rename(tmp.Name(), dest)
}

func joinNames(names []string) string { return strings.Join(names, ", ") }
