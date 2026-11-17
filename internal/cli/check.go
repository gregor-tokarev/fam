package cli

import (
	"fmt"
	"path/filepath"

	"github.com/gregor-tokarev/fam/internal/engine"
	"github.com/gregor-tokarev/fam/internal/output"
	"github.com/spf13/cobra"
)

func (o *Options) project() *engine.Project {
	instructions, skills := o.InstructionsDir, o.SkillsDir
	if instructions == "" {
		instructions = filepath.Join(o.Base, "instructions")
	}
	if skills == "" {
		skills = filepath.Join(o.Base, "skills")
	}
	return &engine.Project{Base: o.Base, ConfigPath: o.Config, InstructionsDir: instructions, SkillsDir: skills}
}
func checkCmd(o *Options) *cobra.Command {
	return &cobra.Command{Use: "check", Short: "Validate the project without SSH or changes", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := o.resolve(); err != nil {
			return err
		}
		r := engine.CheckProject(cmd.Context(), o.project())
		if err := output.WriteValue(cmd.OutOrStdout(), r, o.format(), func() {
			for _, p := range r.Problems {
				position := p.File
				if p.Line > 0 {
					position += fmt.Sprintf(":%d", p.Line)
				}
				if position != "" {
					position += " "
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s%s\n", p.Severity, position, p.Message)
				if p.Pkl != "" {
					fmt.Fprintln(cmd.ErrOrStderr(), p.Pkl)
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "check: %d errors, %d warnings\n", r.Errors, r.Warnings)
		}); err != nil {
			return err
		}
		if !r.OK {
			return ExitError{1}
		}
		return nil
	}}
}
