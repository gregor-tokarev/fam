<!-- fam:begin -->
## FAM project guide

This folder defines a fleet with FAM 0.15.0.
Edit the project files to describe the tools and instructions for each machine.

- `config.pkl` selects bundles, roles, agents, and the secrets source.
- `machines.pkl` lists machines, addresses, users, and roles.
- `agents.pkl` selects coding tools and their settings.
- `bundles/` holds local bundle files and their resources.
- `skills/` holds local skill folders. Each folder needs `SKILL.md`.
- `instructions/AGENTS.md` contains instructions that FAM installs on machines.
- The secrets source stays outside this folder. Never print secret values.

Import `@fam/std.pkl` and add `std.<group>.<name>()` to a `bundles` listing.
Use `fam std list` or `fam std list <group>` to find standard bundles.
Use `fam std show <group>.<name>` to read requirements, skills, and verify commands.
A local bundle file amends `@fam/Bundle.pkl`. Import it from a `bundles` listing.
To remove a bundle from machines, keep its entry and set `state = "absent"`.
For example: `new { bundle = std.computer.agentBrowser(); state = "absent" }`.
Deletion of an entry does not uninstall its bundle.

Run `fam check` after each edit. It validates the project without SSH or changes.
`fam plan` is read only. It connects to machines to report differences.
Never run `fam apply` unless the user asks.
Use `fam doctor` for environment and editor problems, or `fam doctor --offline` without network access.
Use `--json` or `--toon` for machine-readable output.
Use `--machine <name>` to limit runs to selected machines.
<!-- fam:end -->
