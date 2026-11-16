package engine

import (
	"fmt"
	"strings"

	"github.com/gregor-tokarev/fam/internal/config"
	"github.com/gregor-tokarev/fam/internal/transport"
)

// agentDef holds the paths and the official install and update methods of one agent.
//
// Claude Code: https://code.claude.com/docs/en/setup — native installer into ~/.local/bin,
// `claude update`, `claude install <version>`; Homebrew installs update with
// `brew upgrade --cask claude-code`; npm installs with `npm install -g @anthropic-ai/claude-code@latest`.
// DISABLE_AUTOUPDATER=1 in the settings env keeps a pinned version.
//
// Codex: https://developers.openai.com/codex/cli — standalone installer into ~/.local/bin; the same
// command updates it; `--release <version>` pins it. Homebrew: `brew upgrade --cask codex`;
// npm: `npm install -g @openai/codex@latest`.
type agentDef struct {
	binary     string
	settings   string
	format     string
	install    func(version string) string
	update     func(version string) string
	versionCmd string
	// pinSettings are settings that keep a pinned version (no automatic update).
	pinSettings func() *omap
}

// download runs an installer script only after a complete download. `curl … | sh` would
// run an empty script with exit code 0 when the download fails.
func download(url, runner, args string) string {
	return fmt.Sprintf(`t=$(mktemp "${TMPDIR:-/tmp}/fam-installer.XXXXXX") || exit 1
if ! curl -fsSL %s -o "$t"; then rm -f "$t"; echo "download of %s failed" >&2; exit 1; fi
%s "$t"%s; rc=$?; rm -f "$t"; exit $rc`, transport.Quote(url), url, runner, args)
}

// installMethod finds how the binary was installed: npm (any node_modules path, also under
// Homebrew), a Homebrew cask (with its name, for example claude-code@latest), a Homebrew
// formula, an OS package (apk, dpkg, rpm: the signed Anthropic repositories), or the native installer.
func installMethod(bin string) string {
	return fmt.Sprintf(`p=$(command -v %[1]s); r=$(readlink -f "$p" 2>/dev/null || echo "$p")
cask=$(printf '%%s\n' "$r" | sed -n 's|.*/Caskroom/\([^/]*\)/.*|\1|p')
formula=$(printf '%%s\n' "$r" | sed -n 's|.*/Cellar/\([^/]*\)/.*|\1|p')
case "$r" in
  */node_modules/*) m=npm ;;
  *) if [ -n "$cask" ]; then m=cask; elif [ -n "$formula" ]; then m=formula
     elif command -v apk >/dev/null 2>&1 && apk info --who-owns "$r" >/dev/null 2>&1; then m=apk
     elif command -v dpkg >/dev/null 2>&1 && dpkg -S "$r" >/dev/null 2>&1; then m=dpkg
     elif command -v rpm >/dev/null 2>&1 && rpm -qf "$r" >/dev/null 2>&1; then m=rpm
     else m=native; fi ;;
esac`, bin)
}

var agentDefs = map[string]agentDef{
	"claude": {
		binary:   "claude",
		settings: "~/.claude/settings.json",
		format:   "json",
		install: func(v string) string {
			if v != "" {
				return download("https://claude.ai/install.sh", "bash", " "+transport.Quote(v))
			}
			return download("https://claude.ai/install.sh", "bash", "")
		},
		update: func(v string) string {
			if v != "" {
				return fmt.Sprintf(`cur=$(claude --version 2>/dev/null | awk '{print $1}'); [ "$cur" = %[1]s ] || claude install %[1]s`, transport.Quote(v))
			}
			return installMethod("claude") + `
case "$m" in
  cask) brew upgrade --cask "$cask" ;;
  formula) brew upgrade "$formula" ;;
  npm) npm install -g @anthropic-ai/claude-code@latest ;;
  apk) echo "FAM-ROOT: apk update && apk upgrade claude-code"; exit 42 ;;
  dpkg) echo "FAM-ROOT: apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --only-upgrade claude-code"; exit 42 ;;
  rpm) echo "FAM-ROOT: dnf upgrade -y claude-code"; exit 42 ;;
  *) claude update ;;
esac`
		},
		versionCmd: "claude --version",
		pinSettings: func() *omap {
			env := newOmap()
			env.set("DISABLE_AUTOUPDATER", "1")
			m := newOmap()
			m.set("env", env)
			return m
		},
	},
	"codex": {
		binary:   "codex",
		settings: "~/.codex/config.toml",
		format:   "toml",
		install: func(v string) string {
			args := ""
			if v != "" {
				args = " --release " + transport.Quote(v)
			}
			return "CODEX_NON_INTERACTIVE=1; export CODEX_NON_INTERACTIVE\n" + download("https://chatgpt.com/codex/install.sh", "sh", args)
		},
		update: func(v string) string {
			if v != "" {
				// Always run the installer with the pinned release: it also resets the update
				// settings of an earlier standalone install. The version does not change, so the
				// result stays "unchanged".
				return "CODEX_NON_INTERACTIVE=1; export CODEX_NON_INTERACTIVE\n" + download("https://chatgpt.com/codex/install.sh", "sh", " --release "+transport.Quote(v))
			}
			// The official update method of the standalone install is the install command again.
			return installMethod("codex") + `
case "$m" in
  cask) brew upgrade --cask "$cask" ;;
  formula) brew upgrade "$formula" ;;
  npm) npm install -g @openai/codex@latest ;;
  *) CODEX_NON_INTERACTIVE=1; export CODEX_NON_INTERACTIVE
` + download("https://chatgpt.com/codex/install.sh", "sh", "") + ` ;;
esac`
		},
		versionCmd: "codex --version",
		pinSettings: func() *omap {
			m := newOmap()
			m.set("check_for_update_on_startup", false)
			return m
		},
	},
}

func agentsSection(env *Env, spec *config.Spec) (Section, error) {
	sec := Section{Name: "agents"}
	for _, a := range spec.Agents {
		def, ok := agentDefs[a.Name]
		if !ok {
			continue
		}
		g := base{group: "agent " + a.Name}
		v := ""
		if a.Agent.Version != nil {
			v = *a.Agent.Version
		}
		sec.Tasks = append(sec.Tasks, &PackageTask{base: g, Binary: def.binary, Install: def.install(v), UpdateSh: def.update(v), VersionCmd: def.versionCmd, Pin: v})
		settings := newOmap()
		vals, err := fromPkl(a.Agent.Settings)
		if err != nil {
			return sec, fmt.Errorf("the settings of agent %s: %w", a.Name, err)
		}
		switch m := vals.(type) {
		case *omap:
			settings = m
		case nil:
		default:
			return sec, fmt.Errorf("the settings of agent %s must be keys and values (settings { key = value }), not a list or a single value", a.Name)
		}
		if v != "" && def.pinSettings != nil {
			// A pinned version must not update itself. The user settings win.
			pinned := def.pinSettings()
			var changed []string
			mergeInto(pinned, settings, "", &changed)
			settings = pinned
		}
		if len(settings.keys) > 0 {
			sec.Tasks = append(sec.Tasks, &MergeTask{base: g, Path: def.settings, Format: def.format, Values: settings, Mode: 0o644, Label: def.settings + " (settings)"})
		}
	}
	return sec, nil
}

func agentVerify(name string, skills []string) []VerifySpec {
	def, ok := agentDefs[name]
	if !ok {
		return nil
	}
	return []VerifySpec{
		{Bundle: "agent " + name, Name: def.versionCmd, Event: "fresh_shell", Script: def.versionCmd},
		{Bundle: "agent " + name, Name: "instructions and skills visible", Event: "fresh_shell", Script: agentPathsScript(name, skills)},
	}
}

// agentPathsScript checks that the agent finds its instructions and each skill of the config
// at its own path.
func agentPathsScript(name string, skills []string) string {
	var b strings.Builder
	switch name {
	case "claude":
		b.WriteString(`test -r "$HOME/.claude/CLAUDE.md" || { echo "no ~/.claude/CLAUDE.md" >&2; exit 1; }` + "\n")
		for _, n := range skills {
			fmt.Fprintf(&b, `test -r "$HOME/.claude/skills/"%[1]s/SKILL.md || { echo "Claude cannot read the skill "%[1]s >&2; exit 1; }`+"\n", transport.Quote(n))
		}
	case "codex":
		b.WriteString(`test -r "$HOME/.codex/AGENTS.md" || { echo "no ~/.codex/AGENTS.md" >&2; exit 1; }` + "\n")
		for _, n := range skills {
			fmt.Fprintf(&b, `test -r "$HOME/.agents/skills/"%[1]s/SKILL.md || { echo "Codex cannot read the skill "%[1]s >&2; exit 1; }`+"\n", transport.Quote(n))
		}
	default:
		b.WriteString("true\n")
	}
	return b.String()
}
