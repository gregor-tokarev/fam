// Code generated from Pkl module `fam.base`. DO NOT EDIT.
package base

import "github.com/apple/pkl-go/pkl"

// The settings of one coding agent.
type Agent struct {
	// Keys that FAM merges into the settings file of the agent.
	//
	// FAM keeps all other keys of the file.
	// Claude Code: `~/.claude/settings.json`. Codex: `~/.codex/config.toml`.
	// Antigravity CLI: `~/.gemini/antigravity-cli/settings.json`.
	Settings pkl.Object `pkl:"settings"`

	// A pinned CLI version, for example `"2.0.14"`.
	//
	// When it is `null`, FAM installs the latest version. `fam apply` updates the CLI when
	// `<cli> --version` fails, and `fam apply --update` always updates it.
	//
	// The Antigravity CLI has no pinned versions: its installer gives only the latest version.
	Version *string `pkl:"version"`
}
