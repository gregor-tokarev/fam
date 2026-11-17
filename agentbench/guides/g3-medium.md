This folder is a FAM project. It sets the tools, skills, and agent instructions of a fleet of machines. FAM {{version}} reads it.

Where things go:
- `config.pkl` `bundles`: every machine. `roles { ["worker"] { bundles {...} } }`: machines with that role. `bundles` of an entry in `machines.pkl`: one machine.
- `machines.pkl`: `["name"] { role = "worker"; address = "host"; user = "me" }`.
- `agents.pkl`: agent CLIs, and settings that FAM merges into their settings files: `["claude"] { settings { key = value } }`.
- `instructions/AGENTS.md`: goes to every agent on every machine. This file is only for work in this folder.
- `skills/<name>/SKILL.md` (with `name` and `description` front matter): goes to every machine.
- `secrets.file` in `config.pkl`: `KEY=value` lines for every shell. Never print secret values.

Bundles:
- Standard bundles: add `std.<group>.<name>()`. List them with `fam std list`. Details and required secrets: `fam std show <group>.<name>`.
- Own bundle: `bundles/<name>.pkl`, then `import("bundles/<name>.pkl")` in a `bundles` list:
  ```pkl
  amends "@fam/Bundle.pkl"
  import "@fam/tasks.pkl" as fam
  name = "acme"
  env { ["EDITOR"] = "nvim" }
  tasks { new fam.File { path = "~/.config/acme/a.toml"; content = "x = 1\n" } }
  ```
- To uninstall, keep the bundle with state absent: `new { bundle = import("bundles/x.pkl"); state = "absent" }`. A deleted line stays installed.

Commands:
- `fam check` after each edit. It needs no network.
- `fam plan` changes nothing. Do not run `fam apply` unless the user asks.
- `fam doctor` for setup and editor problems. After `fam migrate`, restart the language server of the editor.
