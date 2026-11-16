package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/gregor-tokarev/fam/internal/engine"
	"github.com/gregor-tokarev/fam/internal/output"
	"github.com/gregor-tokarev/fam/internal/runlog"
	"github.com/gregor-tokarev/fam/internal/sudo"
)

// addRunCommands adds plan, apply, and verify.
func addRunCommands(root *cobra.Command, opts *Options) {
	pf := root.PersistentFlags()
	pf.StringVar(&opts.InstructionsDir, "instructions", "", "instructions directory (default <base>/instructions)")
	pf.StringVar(&opts.SkillsDir, "skills", "", "skills directory (default <base>/skills)")
	for _, c := range []struct {
		mode  engine.Mode
		short string
	}{
		{engine.ModePlan, "Show the changes. Change nothing."},
		{engine.ModeApply, "Apply the changes and verify. There is no confirmation prompt."},
		{engine.ModeVerify, "Run the verify scripts only"},
	} {
		mode := c.mode
		root.AddCommand(&cobra.Command{
			Use:   string(mode),
			Short: c.short,
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runMode(cmd, opts, mode)
			},
		})
	}
}

func runMode(cmd *cobra.Command, opts *Options, mode engine.Mode) error {
	if err := opts.resolve(); err != nil {
		return err
	}
	proj := &engine.Project{Base: opts.Base, ConfigPath: opts.Config, InstructionsDir: opts.InstructionsDir, SkillsDir: opts.SkillsDir}
	if proj.InstructionsDir == "" {
		proj.InstructionsDir = filepath.Join(opts.Base, "instructions")
	}
	if proj.SkillsDir == "" {
		proj.SkillsDir = filepath.Join(opts.Base, "skills")
	}
	src := &sudo.Source{PasswordFile: opts.SudoPasswordFile}
	runID := time.Now().UTC().Format("20060102T150405Z") + fmt.Sprintf("-%s-%d", mode, os.Getpid())
	eo := engine.Options{
		Mode:         mode,
		Project:      proj,
		Machines:     opts.Machines,
		Serial:       opts.Serial,
		SSHConfig:    opts.SSHConfig,
		SudoPassword: src.Password,
		Progress:     os.Stderr,
		RunID:        runID,
	}
	log, logErr := runlog.Open(runID)
	if logErr == nil {
		defer func() {
			if err := log.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "fam: the run log %s is not complete: %v\n", log.Path(), err)
			}
		}()
		eo.Observer = log.Observer()
		eo.OnSecrets = log.SetSecrets
	} else {
		fmt.Fprintf(os.Stderr, "fam: no run log: %v\n", logErr)
	}
	res := engine.Run(cmd.Context(), eo)
	if logErr == nil {
		res.LogFile = log.String(log.Path())
		log.Result(res)
	}
	format := output.Text
	switch {
	case opts.JSON:
		format = output.JSON
	case opts.TOON:
		format = output.TOON
	}
	if err := output.Write(cmd.OutOrStdout(), res, format); err != nil {
		return err
	}
	if !res.OK {
		return ExitError{Code: 1}
	}
	return nil
}
