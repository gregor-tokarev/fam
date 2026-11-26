# FAM Infisical, Templates, Conflicts, and Operations

## Infisical (`config.pkl`)

```pkl
secrets {
  infisical {
    siteUrl = "https://app.infisical.com" // HTTPS, or http:// on localhost/127.0.0.1 only
    projectId = "<id>"; environment = "prod"
    path = "/"; recursive = false; includeImports = true // defaults
    credentials = "/abs/path/infisical.env" // optional, omit for env
    cache = 5.min // default, 0.s disables cache
  }
}
```
- `credentials`: mode `0600`, outside `--base`, not `~/.config/fam/secrets.env`. Vars: `INFISICAL_UNIVERSAL_AUTH_CLIENT_ID` + `INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET`, or `INFISICAL_TOKEN`.
- `fam check` reads cache only (never calls Infisical, warns if uncached -> run `fam plan`). `--refresh-secrets` bypasses cache. 5xx/429/network falls back to cache with warning. 4xx stops.

## Instruction Templates (`instructions/`)

Both `instructions/AGENTS.md` and `instructions/CLAUDE.md` use Go `text/template` with `missingkey=error` (unknown fields abort FAM):
- `.Machine`: `.Name`, `.Cockpit`, `.Tags`, `.Address`, `.User`
- `.Facts`: `.OS`, `.Distro`, `.DistroVersion`, `.Arch`, `.CPUs`, `.Home`, `.User`, `.Hostname`, `.Init`, `.Shell`, `.Machine`, `.Cockpit`, `.Tags`
- `.Agent` and `.Facts.Libc` do NOT exist. Hand-edited remote files auto-backup as `*.fam-backup-<ts>`.

## Adoption Conflicts and Agent Paths

FAM halts before writing on conflicts:
- **Unmanaged skill** (`~/.agents/skills/<n>` without `.fam-managed`): move/delete via SSH or rename skill.
- **Legacy `skills/`**: FAM no longer auto-copies `skills/`. Use `new fam.AgentSkill { source = "../skills/<n>" }` in a bundle or `state = "absent"`.
- **Antigravity `~/.gemini/config/skills` dir**: FAM symlinks it to `~/.agents/skills`. Move needed skills into a bundle and remove the real dir via SSH.
- **Unmanaged `GitHubRelease` binary**: `~/.local/bin/<bin>` without `~/.local/state/fam/releases/<bin>.json`. Delete binary.
- **Agent paths**: `claude` (`~/.claude/{settings.json,CLAUDE.md,skills/<n>}`), `codex` (`~/.codex/{config.toml,AGENTS.md}`), `antigravity` (`~/.gemini/antigravity-cli/settings.json`, `~/.gemini/{AGENTS.md,config/skills}`).

## Platform and Logs

- **Arch/Alpine Bash**: without `SSH_SOURCE_BASHRC`, FAM writes `~/.ssh/environment` (`BASH_ENV`) and `/etc/ssh/sshd_config.d/50-fam-bash-env.conf` (sudo) and reconnects.
- **Install cockpit**: `curl -fsSL https://raw.githubusercontent.com/gregor-tokarev/fam/main/install.sh | sh`.
- **Run logs**: `~/.local/state/fam/runs/<run-id>.jsonl`.
