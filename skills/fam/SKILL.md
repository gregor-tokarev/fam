---
name: fam
description: Create, modify, and operate FAM (Fleet Agent Manager) Pkl projects on Linux and macOS fleets. Use for fam CLI commands, std bundles, machines, tags, custom bundles, agents, skills, instructions, and file or Infisical secrets.
---

# FAM

Manage Linux and macOS tools, env, secrets, skills, and instructions over SSH from one cockpit Pkl project. Workers need SSH and passwordless `sudo -n` for `sudo = true`. Cockpits allow TTY or `--sudo-password-file`. Windows is unsupported.

## Commands

- `fam new [--base <dir>]` (scaffold, fails if present), `fam std list [group]` / `fam std show <g>.<n>` (always run `list` before guessing a group name).
- `fam check` (offline after every edit, cached Infisical only), `fam plan` (read-only SSH diff), `fam apply` (no prompt, run only when asked), `fam verify` (`fresh_shell` checks).
- `fam doctor [--offline]` (pins, `.pkl-lsp/projects.json`, config, SSH), `fam migrate` (updates `PklProject`, `PklProject.deps.json`, `.pkl-lsp/projects.json`, tell user to restart Pkl LSP).
- Flags: `--base <dir>`, `--config`, `--instructions`, `--machine <n>` (repeatable), `--serial` (shared host), `--ssh-config` / `FAM_SSH_CONFIG`, `--json` | `--toon`, `--update` (`apply` only: update passing packages/agents), `--refresh-secrets`, `--sudo-password-file` (cockpit only).

## Project Rules

- `config.pkl` (`amends "@fam/Config.pkl"`, `import "@fam/std.pkl"`): `bundles`, `tags`, `machines`, `agents`, optional `secrets`.
- `machines.pkl` (`amends "@fam/Machines.pkl"`, import `@fam/std.pkl` for std bundles): cockpit (max 1, no `address`) `["c"] { cockpit = true }`, worker `["w"] { tags { "t" }; address = "h"; user = "u" }` (`user` optional via SSH config). Declare every tag in `config.pkl` `tags { ["t"] { bundles { ... } } }` (`["t"] {}` if empty).
- Order: `config.pkl` `bundles` -> `tags` (machine order) -> machine `bundles`. Same `name` replaces in place. Duplicate `name` across files fails. Remove: `new { bundle = ...; state = "absent" }` (deleting leaves installed). Manager: `std.<g>.<n>().withManager("vite")` (`npm|bun|vite|uv|cargo|go|system|brew|apt|dnf|pacman|yay|apk`).
- `agents.pkl` (`amends "@fam/Agents.pkl"`): `agents { ["claude"] { version = "2.1.0"; settings { autoMemoryEnabled = false } }; ["codex"] {}; ["antigravity"] {} }`. Never pin `version` on `"antigravity"`.
- Instructions: `instructions/AGENTS.md` (all) + optional `instructions/CLAUDE.md` (Claude). Never put fleet rules in root `AGENTS.md`. Read `references/advanced.md` before Go template vars.
- Skills: only `new fam.AgentSkill { source }` in a bundle (relative dir with `SKILL.md` `name` + `description`, or GitHub/archive/`.md` URL, no `~` or `sudo`).
- Secrets: `secrets { file = "/abs/path.env" }` OR `infisical` (never both). Mode `0600`, outside `--base`, never `~/.config/fam/secrets.env` on cockpit. Never print values.

## Custom Bundle (`bundles/<name>.pkl`, load via `import("bundles/<name>.pkl")`)

```pkl
amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam // alias required
name = "my-tool"; env { ["K"] = "v" }; path { "~/.local/bin" }
tasks {
  new fam.File { path = "~/.local/bin/x"; content = "..."; mode = "0755" }
  new fam.AgentSkill { source = "../skills/my-skill" }
}
verify { new { event = "fresh_shell"; script = "x" } }
```

## References (read ONLY when needed)

- `references/tasks.md`: `Merge`, `Symlink`, `Package`, `GitHubRelease`, `Service`, `Linger`, `Script` (`check` codes, `uninstall {}`), `facts.pkl`, `supported`, `restarts`.
- `references/advanced.md`: `secrets.infisical`, Go template vars, conflicts (unmanaged skills/binaries, legacy `skills/`, `~/.gemini/config/skills`), Arch/Alpine Bash, logs.
