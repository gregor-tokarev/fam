<!-- fam:begin -->
FAM project: the tools, skills, and agent instructions of a fleet of machines (FAM 0.17.1).

Where things go:
- Every machine: `bundles` in `config.pkl`. A role: `roles { ["worker"] { bundles {...} } }`. One machine: `bundles` of its entry in `machines.pkl` (add `import "@fam/std.pkl"` there for std bundles).
- An entry with the name of an earlier bundle replaces it, so a role or a machine can change or remove a bundle of every machine.
- Machines: `machines.pkl`, `["name"] { role = "worker"; address = "host"; user = "me" }`. Roles: `"cockpit"` (this machine), `"worker"`.
- Agent settings: `agents.pkl`, `["claude"] { settings { key = value } }`.
- `instructions/AGENTS.md` goes to every agent on every machine. This file is only for this folder.
- `skills/<name>/SKILL.md` (front matter `name`, `description`) goes to every machine.
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
    new fam.GitHubSkill { repo = "owner/repo"; path = "skills/name" }
  }
  ```
- Other tasks in `@fam/tasks.pkl`: `Merge`, `Symlink`, `GitHubRelease`, `Skill`, `Service`, `Linger`, `Script`.
- Uninstall: keep the entry as `new { bundle = std.networking.cloudflared(); state = "absent" }`. A deleted line stays installed.

Commands: `fam check` after each edit (no network). `fam plan` is read only. Do not run `fam apply` unless the user asks. `fam doctor` for setup and editor problems. After `fam migrate`, restart the editor language server.
<!-- fam:end -->
