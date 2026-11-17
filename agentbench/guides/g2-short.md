This folder is a FAM project. It sets the tools, skills, and agent instructions of a fleet of machines.

- Bundles for every machine: `bundles` in `config.pkl`. For a role: `roles { ["worker"] { bundles {...} } }`. For one machine: `bundles` of its entry in `machines.pkl`.
- Standard bundles: add `std.<group>.<name>()`. List them with `fam std list`. Details: `fam std show <group>.<name>`.
- To uninstall a bundle, keep it with state absent: `new { bundle = std.computer.agentBrowser(); state = "absent" }`. A deleted line stays installed.
- `instructions/AGENTS.md` goes to every agent on every machine. This file is only for work in this folder.
- `skills/<name>/SKILL.md` goes to every machine.
- Run `fam check` after each edit. `fam plan` changes nothing. Do not run `fam apply` unless the user asks.
