<!-- fam:begin -->
FAM project: the tools, skills, and agent instructions of a fleet of machines (FAM 0.20.1).

Where things go:
- Every machine: `bundles` in `config.pkl`. Machines with a tag: `tags { ["gpu"] { bundles {...} } }` in `config.pkl`. One machine: `bundles` of its entry in `machines.pkl` (add `import "@fam/std.pkl"` there for std bundles).
- An entry with the name of an earlier bundle replaces it, so a tag or a machine can change or remove a bundle of every machine. Tag bundles come in the order of the machine's `tags`.
- Machines: `machines.pkl`, `["name"] { tags { "worker"; "gpu" }; address = "host"; user = "me" }`. A machine can have any number of tags, and each tag must be in `tags` of `config.pkl` (`["name"] {}` for a tag without bundles). This machine: `["name"] { cockpit = true }`.
- Agent settings: `agents.pkl`, `["claude"] { settings { key = value } }`.
- `instructions/AGENTS.md` goes to every agent on every machine. This file is only for this folder.
- Skills: `new fam.AgentSkill { source }` tasks in bundles, so a bundle for every machine, a tag, or one machine selects them. `source` is a folder relative to the bundle file (with `SKILL.md`, front matter `name`, `description`), a GitHub repository URL (with `path` and `ref`), a `/tree/<ref>/<folder>` URL, a `.tar.gz` or `.zip` URL, or a `.md` URL.
- Secrets: `KEY=value` lines in the file of `secrets` in `config.pkl`, or an Infisical folder (`secrets { infisical { projectId; environment } }`). Bundles name theirs in `requiredSecrets`. Never print values.

Bundles:
- Standard: `std.<group>.<name>()`. Find with `fam std list`. Details and secrets: `fam std show <group>.<name>`.
- Own: `bundles/<name>.pkl`, added with `import("bundles/<name>.pkl")` in a `bundles` list:
  ```pkl
  amends "@fam/Bundle.pkl"
  import "@fam/tasks.pkl" as fam
  name = "tool"
  env { ["PAGER"] = "less" }
  tasks {
    new fam.File { path = "~/.config/tool/config.toml"; content = "key = 1\n" }
    new fam.Package { package = new { package = "htop"; manager = "system" } }
    new fam.AgentSkill { source = "https://github.com/owner/repo"; path = "skills/name" }
  }
  ```
- Other tasks in `@fam/tasks.pkl`: `Merge`, `Symlink`, `GitHubRelease`, `Service`, `Linger`, `Script`.
- Uninstall: keep the entry as `new { bundle = std.networking.cloudflared(); state = "absent" }`. A deleted line stays installed.

Commands: `fam check` after each edit (no network). `fam plan` is read only. Do not run `fam apply` unless the user asks. `fam doctor` for setup and editor problems. After `fam migrate`, restart the editor language server.
<!-- fam:end -->
