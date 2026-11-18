package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/gregor-tokarev/fam/internal/engine"
	"github.com/gregor-tokarev/fam/internal/output"
	"github.com/gregor-tokarev/fam/internal/progress"
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
		cmd := &cobra.Command{
			Use:   string(mode),
			Short: c.short,
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runMode(cmd, opts, mode)
			},
		}
		cmd.Flags().BoolVar(&opts.NoProgress, "no-progress", false, "do not show the progress on stderr")
		cmd.Flags().BoolVar(&opts.RefreshSecrets, "refresh-secrets", false, "read the secrets from Infisical, also when the cache is fresh")
		root.AddCommand(cmd)
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
	var rep *progress.Reporter
	if !opts.NoProgress {
		rep = newReporter(opts.JSON || opts.TOON)
		defer rep.Stop()
	}
	sudoPassword := func(ctx context.Context) ([]byte, error) {
		// The password prompt must not mix with the live lines.
		resume := rep.Pause()
		defer resume()
		return src.Password(ctx)
	}
	runID := time.Now().UTC().Format("20060102T150405Z") + fmt.Sprintf("-%s-%d", mode, os.Getpid())
	eo := engine.Options{
		Mode:           mode,
		Project:        proj,
		Machines:       opts.Machines,
		Serial:         opts.Serial,
		SSHConfig:      opts.SSHConfig,
		SudoPassword:   sudoPassword,
		Progress:       progressWriter(rep),
		Reporter:       rep,
		RunID:          runID,
		RefreshSecrets: opts.RefreshSecrets,
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
	rep.Stop()
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

// newReporter shows live lines when stderr is a terminal, and plain lines in other cases.
// With JSON or TOON output and no terminal, it returns nil: a program reads the output, and
// often reads stderr together with stdout.
func newReporter(machineOutput bool) *progress.Reporter {
	fd := int(os.Stderr.Fd())
	tty := term.IsTerminal(fd)
	if machineOutput && !tty {
		return nil
	}
	live := tty && os.Getenv("TERM") != "dumb" && os.Getenv("CI") == ""
	return progress.New(os.Stderr, live, func() int {
		w, _, err := term.GetSize(fd)
		if err != nil {
			return 0
		}
		return w
	})
}

func progressWriter(rep *progress.Reporter) io.Writer {
	if rep == nil {
		return os.Stderr
	}
	return rep
}
