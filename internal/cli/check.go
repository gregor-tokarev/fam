package cli

import (
	"fmt"
	"path/filepath"

	"github.com/gregor-tokarev/fam/internal/engine"
	"github.com/gregor-tokarev/fam/internal/output"
	"github.com/spf13/cobra"
)

func (o *Options) project() *engine.Project {
	instructions := o.InstructionsDir
	if instructions == "" {
		instructions = filepath.Join(o.Base, "instructions")
	}
	return &engine.Project{Base: o.Base, ConfigPath: o.Config, InstructionsDir: instructions}
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
			fmt.Fprintf(cmd.OutOrStdout(), "check: %s, %s\n", plural(r.Errors, "error"), plural(r.Warnings, "warning"))
		}); err != nil {
			return err
		}
		if !r.OK {
			return ExitError{1}
		}
		return nil
	}}
}

// plural gives "1 error" or "2 errors".
func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
