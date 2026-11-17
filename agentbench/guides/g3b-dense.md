FAM project: the tools, skills, and agent instructions of a fleet of machines (FAM {{version}}).

Where things go:
- Every machine: `bundles` in `config.pkl`. A role: `roles { ["worker"] { bundles {...} } }`. One machine: `bundles` of its entry in `machines.pkl`.
- Machines: `machines.pkl`, `["name"] { role = "worker"; address = "host"; user = "me" }`.
- Agent settings: `agents.pkl`, `["claude"] { settings { key = value } }`.
- `instructions/AGENTS.md` goes to every agent on every machine. This file is only for this folder.
- `skills/<name>/SKILL.md` (front matter `name`, `description`) goes to every machine.
- Secrets: `KEY=value` lines in the file of `secrets` in `config.pkl`. Never print values.

Bundles:
- Standard: `std.<group>.<name>()`. Find with `fam std list`. Details and secrets: `fam std show <group>.<name>`.
- Own: `bundles/<name>.pkl` with `amends "@fam/Bundle.pkl"`, `import "@fam/tasks.pkl" as fam`, `name`, `env {}`, `tasks { new fam.File { path = "~/x"; content = "..." } }`. Add `import("bundles/<name>.pkl")` to a `bundles` list.
- Uninstall: keep the entry as `new { bundle = <bundle>; state = "absent" }`. A deleted line stays installed. An entry with the same name in a role or machine replaces the global one.

Commands: `fam check` after each edit (no network). `fam plan` is read only. Do not run `fam apply` unless the user asks. `fam doctor` for setup and editor problems. After `fam migrate`, restart the editor language server.
