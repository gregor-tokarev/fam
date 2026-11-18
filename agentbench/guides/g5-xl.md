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

More examples:
- Merge keys into a JSON or TOML file: `new fam.Merge { path = "~/.config/x/settings.json"; format = "json"; values { theme = "dark" } }`.
- A link: `new fam.Symlink { path = "~/.local/bin/x"; target = "~/tools/x/bin/x" }`.
- A release binary from GitHub: `new fam.GitHubRelease { repo = "owner/tool"; binary = "tool"; assets { ["linux-amd64"] {...} } }` (see `@fam/tasks.pkl`).
- A user service: `new fam.Service { name = "x"; content = "[Unit]..." }`. It needs systemd or launchd.
- A script: `new fam.Script { name = "x"; apply = "..."; creates = "~/.local/bin/x" }`. FAM cannot undo a script, so add `uninstall` tasks to the bundle.
- Platform limits: `supported = facts.os == "linux"` with `import "@fam/facts.pkl"`. FAM skips unsupported bundles.
- Conditional tasks: `when (facts.os == "darwin") { new fam.File {...} }`.
- Another package manager for a std bundle: `std.computer.agentBrowser().withManager("bun")` in `config.pkl`.

Rules:
- Edit only files of this folder. FAM puts files on the machines.
- Keep secret values out of Pkl files and out of git.
- FAM stops on files on a machine that it did not make. Do not delete them by hand: tell the user.
- When `fam check` reports a Pkl error, read the line and column in its message. The error is often a type error in the line above.
