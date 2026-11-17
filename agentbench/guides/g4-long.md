This folder is a FAM project. It sets the tools, skills, and agent instructions of a fleet of machines. FAM {{version}} reads it.

Where things go:
- `config.pkl` `bundles`: every machine. `roles { ["worker"] { bundles {...} } }`: machines with that role. `bundles` of an entry in `machines.pkl`: one machine.
- An entry with the name of an earlier bundle replaces it. So a role or a machine can change or remove a bundle of every machine.
- `machines.pkl`: `["name"] { role = "worker"; address = "host"; user = "me" }`. Roles are `"cockpit"` (this machine) and `"worker"`.
- `agents.pkl`: agent CLIs (`"claude"`, `"codex"`), and settings that FAM merges into their settings files: `["claude"] { settings { key = value } }`.
- `instructions/AGENTS.md`: goes to every agent on every machine. This file is only for work in this folder.
- `skills/<name>/SKILL.md` (with `name` and `description` front matter): goes to every machine.
- `secrets { file = "..." }` in `config.pkl`: `KEY=value` lines that every shell gets. Bundles list the names they need in `requiredSecrets`. Never print secret values.

Bundles:
- Standard bundles: add `std.<group>.<name>()`. List them with `fam std list [group]`. Details and required secrets: `fam std show <group>.<name>`.
- Own bundle: `bundles/<name>.pkl`, then `import("bundles/<name>.pkl")` in a `bundles` list:
  ```pkl
  amends "@fam/Bundle.pkl"
  import "@fam/tasks.pkl" as fam
  name = "acme"
  env { ["EDITOR"] = "nvim" }
  requiredSecrets { "ACME_TOKEN" }
  tasks {
    new fam.File { path = "~/.config/acme/a.toml"; content = "x = 1\n" }
    new fam.Package { package = new { package = "ripgrep"; manager = "system" } }
    new fam.GitHubSkill { repo = "owner/repo"; path = "skills/name" }
  }
  verify { new { script = "acme --version" } }
  ```
- Task types in `@fam/tasks.pkl`: `File`, `Merge` (JSON or TOML keys), `Symlink`, `Package` (npm by default, or `manager` = `system`, `brew`, `apt`, `uv`, `go`, `cargo`, ...), `GitHubRelease`, `Skill` (a folder of the bundle), `GitHubSkill`, `Service`, `Linger`, `Script` (needs `check` or `creates`).
- To uninstall, keep the bundle with state absent: `new { bundle = import("bundles/x.pkl"); state = "absent" }` or `new { bundle = std.computer.agentBrowser(); state = "absent" }`. A deleted line stays installed.
- Only on one machine: in its entry in `machines.pkl`, `bundles { new { bundle = std.computer.agentBrowser(); state = "absent" } }` (add `import "@fam/std.pkl"`).

Commands:
- `fam check` after each edit. It needs no network and changes nothing.
- `fam plan` changes nothing. Do not run `fam apply` unless the user asks.
- `fam doctor` for setup and editor problems. After `fam migrate`, restart the language server of the editor.
- `--json` or `--toon` give machine-readable output. `--machine <name>` limits a run.
