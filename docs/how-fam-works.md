# How FAM works

## Project tools

Use these commands from a project folder. Set `--base <folder>` to select another project.
Set `--config <file>` to select another entrypoint inside that project.
`--json` and `--toon` select structured output for the commands below.

### Find standard bundles

`fam std list [group]` lists the embedded catalogue without network access after the pinned Pkl binary is cached.
Each group has a heading. Each bundle has one line: `std.<group>.<name>()  <summary>`.
`fam std show <group>.<name>` shows the full comment, bundle name, required secret names, skills, and verify scripts.
It also shows platform restrictions from representative Linux and macOS facts.
An unknown bundle exits with code `1` and lists the three closest names.
These commands need no project.

### Validate edits

Run `fam check` after each edit, or in CI.
It evaluates the project without SSH, task execution, downloads, or writes.
The pinned Pkl binary must already be cached. The FAM package comes from the binary.
Other remote Pkl dependencies must already be in the package cache.
Use `fam internal pkl-path` once if the pinned Pkl binary is missing.

The check uses representative Ubuntu amd64 and macOS arm64 facts for each machine.
It validates Pkl types, bundle names, role references, required secret names, local resources, skills, and instruction templates.
It also checks task definitions, the cockpit count, project folders, and the secrets source rules.
It warns when the package pin differs from the binary. Run `fam migrate` to update the pin.
The check cannot validate every condition that depends on remote facts. Run `fam plan` before apply.
It does not download GitHub skills or inspect remote files.

Each problem uses `error|warning <file>:<line> <message>` when FAM knows the position.
FAM omits unknown position fields. The last line is `check: <errors> errors, <warnings> warnings`.
Pkl errors also retain the original diagnostic on stderr, with source lines and the Pkl message.
JSON and TOON include that diagnostic in the `pkl` field instead.
Exit code `0` means no errors. Warnings alone do not fail the check.

### Diagnose the environment

`fam doctor` checks the binary version, project pins, editor state, both package caches, and the pinned Pkl binary.
It summarizes `fam check` and runs `true` over SSH on each worker, in parallel.
The cockpit needs no SSH. `--machine` selects machines.
`--ssh-config` and `FAM_SSH_CONFIG` select the SSH configuration, as for plan.
Each line uses `ok|warn|fail <check>: <detail>`.
A warning or failure has a `fix: <command>` line.
The release check times out after three seconds. FAM omits it when offline.
`fam doctor --offline` skips release and SSH checks. A failure gives exit code `1`.
For editor state problems, run `fam migrate`, then restart the editor language server.

## Project guide

`fam new` and `fam migrate` write a guide in the project base as `AGENTS.md`.
They create `CLAUDE.md` with the single line `@AGENTS.md` only when that file does not exist.
Claude Code imports the same guide through this line.

FAM owns only the section between `<!-- fam:begin -->` and `<!-- fam:end -->`.
Migration replaces that section and keeps text outside it.
If the file has no markers, FAM appends the section.
The guide explains project edits, bundle discovery, validation, and when to use plan and apply.
`instructions/AGENTS.md` serves a different purpose: FAM installs its content on fleet machines.

## Run order

`fam plan`, `fam apply`, and `fam verify` do these steps:

1. **Validate.** FAM puts its `fam` package into the Pkl cache and checks the version that `PklProject.deps.json` pins (see [The fam package](#the-fam-package)), and that the project has the directories that the config needs. It evaluates only the `machines` mapping and `secrets` (Pkl is lazy, so nothing that depends on the facts of a machine), checks that at most one machine is the cockpit, and reads the secrets from the file or from Infisical.
2. **Collect facts.** FAM runs one shell script on each machine: over SSH for workers, locally for the cockpit. Machines run in parallel. `--serial` runs them one at a time. FAM stops on a Windows machine.
3. **Evaluate.** FAM evaluates the full config for each machine, with the facts of that machine as the Pkl properties `fam.<name>`, and checks the rules that Pkl cannot check: bundle names and scripts with a check.
4. **Plan.** FAM runs the check of every task. A check changes nothing.
5. **Apply** (`fam apply` only). FAM stops on a machine with a conflict before it changes anything. Then for each task: check, apply when the check finds a difference, and check again. A task that fails stops the tasks after it on that machine. FAM never runs apply after a check error.
6. **Restart.** At the end of each present bundle, FAM restarts each unit that a changed task names in `restarts` once. FAM does not restart units after a failure, and not for a bundle with `state = "absent"`.
7. **Verify.** FAM runs the verify scripts of the bundles and the agent checks in new shells.

## The fam package

The schema (`Config.pkl`, `Bundle.pkl`, `tasks.pkl`, …) and the standard bundles (`std.pkl`) are the Pkl package `fam`. A project declares it in `PklProject`, as a Go project declares its modules in `go.mod`, and imports it with `@fam/`:

```
PklProject            # dependencies { ["fam"] { uri = "package://github.com/gregor-tokarev/fam/releases/download/v<version>/fam@<version>" } }
PklProject.deps.json  # the checksum of the package
config.pkl            # amends "@fam/Config.pkl"
bundles/tool.pkl      # amends "@fam/Bundle.pkl"
```

- The package is in the FAM binary. `fam new`, `fam migrate`, `fam plan`, `fam apply`, and `fam verify` put it into the Pkl cache (`~/.cache/pkl`, or `$XDG_CACHE_HOME/pkl`, and `~/.pkl/cache` for editors with an older Pkl). So FAM never downloads its own schema.
- Editors with the Pkl plugin read `PklProject` and the same cache, so completion and go-to-definition work for `@fam/…` and `std.…`. On a machine without FAM, the editor downloads the package once from the GitHub release.
- `fam new` and `fam migrate` also write `.pkl-lsp/projects.json`, the project state that the Pkl language server writes on "Sync Projects". So the editor needs no sync after these commands, also in editors that have no "Sync Projects" command, such as Zed. Restart the language server after `fam migrate`. The folder has its own `.gitignore`.
- The project pins one FAM version. A binary of another patch release of the same minor version works with it: patch releases do not change the schema. Otherwise FAM asks for `fam migrate` (an older project) or `fam update` (a newer project).
- `fam migrate` changes the version in `PklProject` and the checksum in `PklProject.deps.json`. It keeps the other dependencies of the project.
- A standard bundle comes from the package, so it has no folder: it cannot use `fam.Skill` with a local folder.

## Task order on a machine

1. Environment: `~/.config/fam/secrets.env`, `~/.config/fam/env.sh`, and the loader blocks.
2. Agents: the CLI package, then the settings merge, for each agent. They come after the environment, so that the installers see its variables (for example a proxy).
3. Bundles: first the `bundles` of `config.pkl` (every machine), then the bundles of the machine's role (`roles` in `config.pkl`), then the `bundles` of the machine entry, each in list order. An entry with the name of an earlier bundle replaces it at its position, so a role or a machine can change or remove (`state = "absent"`) a bundle of every machine.
4. The skills of `skills/`, then the removal of old managed skills. (Bundle skills are tasks of their bundle.)
5. Instructions.

## Check exit codes

| Code | Meaning |
|---|---|
| `0` | Satisfied |
| `100` | Changes required |
| Any other | Error. FAM never runs apply after an error. |
| `255` from SSH | Connection error. FAM classifies it before the script result. |

## Transport

One interface (`internal/transport`) with an SSH and a local implementation. Its operations: run a command, read a file, write a file atomically (a temporary file in the same directory, then a rename), and get file information. Both implementations run the same POSIX shell scripts. FAM selects OS-specific commands from the facts, for example `sha256sum` or `shasum -a 256`, and `stat -c` or `stat -f`.

SSH uses the system `ssh` with `ControlMaster=auto`, `ControlPersist=120`, `BatchMode=yes`, and `ServerAliveInterval=5` (`ServerAliveCountMax=3`). The control sockets are in `/tmp/fam-<uid>/`, independent of `TMPDIR`. FAM requires a directory owned by the current user, with mode `0700`, and rejects symlinks. `--ssh-config <file>` (or `FAM_SSH_CONFIG`) gives a config file to `ssh -F`. Without it, `ssh` reads `~/.ssh/config`.

`sudo`:

- On workers, FAM runs `sudo -n`. The SSH user needs passwordless sudo for tasks with `sudo = true`.
- On the cockpit, FAM first tries `sudo -n true`. When sudo needs a password, FAM asks once in the terminal with hidden input and tests it with `sudo -v`. Without a terminal, FAM reads `--sudo-password-file`. FAM keeps the password in memory and gives it to `sudo -k -S` (`-k` makes sudo read the password every time, so the password never reaches the command). It never writes the password.
- A command with `sudo` never reads data from stdin. To write a file with `sudo`, FAM writes a temporary file as the user first.

User scripts (`Script`, `Package`) run in Bash when the machine has Bash, else in `sh`. Before the script, FAM loads `~/.config/fam/env.sh` and puts `~/.local/bin` in `PATH`.

## Paths on each machine

| Path | Content |
|---|---|
| `~/.config/fam/env.sh` | Bundle variables, `~/.local/bin` in `PATH`, and a line that loads the secrets. |
| `~/.config/fam/secrets.env` | `export NAME='value'` lines, mode `0600`. |
| `~/.zshenv`, `~/.bashrc`, `~/.profile` | A marked block (`# >>> fam >>>`) that loads `env.sh`. In `~/.bashrc` the block is the first lines, before any line that stops noninteractive shells. FAM also writes the block into `~/.profile` (login shells), and into `~/.bash_profile` and `~/.bash_login` when they exist, because Bash then does not read `~/.profile`. In the other files the block is the last lines. An installer in the same apply (for example the Vite+ installer) can add lines after it, so FAM checks the blocks again at the end of the apply and moves them back. |
| `~/.ssh/environment` and `/etc/ssh/sshd_config.d/50-fam-bash-env.conf` | Only on Linux machines where the login shell is Bash and Bash does not read `~/.bashrc` for noninteractive SSH commands (Bash built without `SSH_SOURCE_BASHRC`, for example on Arch Linux and Alpine). A marked block in `~/.ssh/environment` sets `BASH_ENV` to `~/.config/fam/env.sh`, and the sshd drop-in allows only that variable (`PermitUserEnvironment BASH_ENV`). FAM tests the sshd configuration before the reload, and opens a new connection before verify. FAM finds such a Bash from the facts: a Bash with this feature contains the string `SSH2_CLIENT`. |
| `~/.agents/AGENTS.md` | The rendered `instructions/AGENTS.md`. |
| `~/.agents/CLAUDE.md` | The same content, then the rendered `instructions/CLAUDE.md`. |
| `~/.codex/AGENTS.md` | A link to `~/.agents/AGENTS.md`. |
| `~/.claude/CLAUDE.md` | A link to `~/.agents/CLAUDE.md`. |
| `~/.agents/skills/<name>/` | A copy of each skill, with a `.fam-managed` marker file. |
| `~/.claude/skills/<name>` | A link to `~/.agents/skills/<name>`. |
| `~/.local/bin/<binary>` | A binary from a `GitHubRelease` task. |
| `~/.local/state/fam/releases/<binary>.json` | The repository and SHA-256 of a binary that FAM installed. |

FAM uses markers and release records to find what it manages:

- Instruction files start with a marker line that holds the SHA-256 of the rest of the file. A file without the marker, or with a different digest, was not written by FAM (or was changed by hand). FAM saves a copy (`<file>.fam-backup-<time>`) before it replaces such a file.
- A skill folder with `.fam-managed` belongs to FAM. A skill folder or a Claude skill link that FAM did not make is a conflict: the plan stops, and `fam apply` changes nothing on that machine. FAM removes managed skills that the config no longer lists.
- A release binary without a FAM record is a conflict. Plan stops, and apply changes nothing on that machine. Remove the file, then apply to let FAM manage it. FAM replaces a managed binary after a hand change. An absent bundle removes only its managed binary and record.

## Instruction templates

`instructions/AGENTS.md` and `instructions/CLAUDE.md` are Go `text/template` files with `missingkey=error`. An unknown variable stops FAM. The data:

| Variable | Content |
|---|---|
| `.Machine.Name`, `.Machine.Role`, `.Machine.Address`, `.Machine.User` | The machine entry. |
| `.Facts.OS`, `.Facts.Distro`, `.Facts.DistroVersion`, `.Facts.Arch`, `.Facts.Libc`, `.Facts.CPUs`, `.Facts.Home`, `.Facts.User`, `.Facts.Hostname`, `.Facts.Init`, `.Facts.Shell`, `.Facts.Machine`, `.Facts.Role` | The facts. |

There is no `.Agent` variable. Bundles add no instructions: a bundle's skill tells the agents about its tool.

## Skills

FAM copies each folder of `skills/` (resolved relative to `--base`, or set with `--skills`) after the bundles. The skill name is the folder name.

Bundles install skills with tasks, as many as they need, in task order: `new fam.Skill { path }` for a folder of the bundle, and `new fam.GitHubSkill { repo; path; ref }` for a folder in a public GitHub repository. A skill task copies the skill and links `~/.claude/skills/<name>`. The name is the folder name (a bundle folder named `skill` gets the bundle name), or `name`. With `state = "absent"`, or in an absent bundle, the task removes the skill, but only one that FAM made. Two skills with the same name are an error. FAM removes managed skills that no bundle and no folder of `skills/` lists any more.

For a GitHub skill, the cockpit downloads the repository archive from `codeload.github.com` and copies the folder like a local one. All skills of one repository and ref share one download. FAM keeps the archive in `<user cache dir>/fam/github/` with its ETag. Once in each run, also for `fam plan`, FAM asks GitHub with this ETag, and GitHub sends the archive again only when it changed (a new commit on a branch, or a moved tag). FAM removes an archive that no run used for 30 days. Without a `ref`, FAM uses the tag of the latest release (the redirect of `github.com/<repo>/releases/latest`, no API token). FAM tries a request up to three times when the network fails. A link in the folder becomes a copy of the file that it points to, which must be in the same folder. FAM reads at most 1 GiB of the archive and keeps only the folder (at most 16 MiB for each file, 64 MiB and 5000 files in total).

## Secrets

The secrets come from one source: a file on the cockpit (`file`) or a folder of an Infisical project (`infisical`). FAM writes them into the same `~/.config/fam/secrets.env` on each machine.

### Secrets file

The secrets file on the cockpit has `NAME=value` lines. Empty lines and lines that start with `#` are comments. `export ` before the name is allowed. A value can be in single quotes, or in double quotes with `\"`, `\\`, `\$`, and `` \` `` escapes. FAM stops on:

- a file with a mode other than `0600`, or a file inside the project base,
- a value with a new line or another control character, an open quote, or `\n`.

FAM writes the values as `export NAME='value'` with `'\''` for a single quote. The plan shows the names. The run log, the text output, JSON, and TOON replace each secret value with `[secret]`. The secrets file goes to the machine through stdin, so it is never in a command line or a log. FAM also hides the old values that it finds in the secrets file of a machine (after a rotation, scripts still load them), and the forms in which a value appears in output: without its outer spaces, and with `'\''` for a single quote.

### Infisical

```pkl
secrets {
  infisical {
    siteUrl = "https://app.infisical.com" // the default; or https://eu.infisical.com, or a self-hosted site
    projectId = "<project ID>"
    environment = "prod"
    path = "/"                            // the default
    recursive = false                     // true also reads the subfolders
    includeImports = true                 // a secret of the folder wins over an imported one
    credentials = "~/.config/fam-secrets/infisical.env"
    cache = 5.min                         // the default
  }
}
```

FAM signs in with a machine identity. The `credentials` file has `INFISICAL_UNIVERSAL_AUTH_CLIENT_ID` and `INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET` (Universal Auth), or `INFISICAL_TOKEN` (an access token). The file has the rules of a secrets file: mode `0600`, outside the project base. Without `credentials`, FAM reads these variables from its environment. When the file is set, FAM ignores the environment, so a token in the shell for other tools does not change what FAM reads.

The site must use HTTPS. Plain HTTP is allowed only for localhost, for example a local mock. FAM does not follow redirects, so that the client secret cannot go to another address.

Each secret name must be a shell variable name, and each value must be on one line. FAM stops on another secret, and gives its name and folder. FAM also stops on a secret whose value the identity cannot read (Infisical sends a placeholder), on a secret without a value, on an answer without a list of secrets, and on secrets of another project, environment, or folder (a service token scoped to another environment makes Infisical answer for that scope). Keep such secrets in a folder that FAM does not read. With `recursive = true`, a name must occur in only one folder.

FAM keeps the answers of Infisical in `<user cache dir>/fam/infisical/`: a directory with mode `0700` and files with mode `0600`, one for each site, credentials, project, environment, and folder. New credentials, also a new client secret, start with no cache. FAM uses only cache files of its own user, and checks the cached names and values again. When two FAM runs read Infisical at the same time, a lock file for each cache file makes sure that the cache keeps the answer of the later read. FAM also keeps the access token of a login there until one minute before it expires. When Infisical rejects the token, FAM signs in again once.

| Command | Infisical |
|---|---|
| `fam plan`, `fam apply`, `fam verify` | FAM uses the cache when it is younger than `cache`. Else FAM reads Infisical and saves the cache. `cache = 0.s` reads Infisical on each run. |
| `--refresh-secrets` | FAM reads Infisical also when the cache is fresh. Use it after a rotation. |
| Infisical does not answer (network, HTTP 429, HTTP 5xx) | FAM uses the cache of any age. Each machine shows a warning with the age. Without a cache, the run stops. |
| Infisical rejects the request (HTTP 4xx) | The run stops. FAM does not use the cache. |
| `fam check` | FAM never calls Infisical. It uses the cache of any age. It needs the credentials to find the cache. Without a cache, it shows a warning and does not compare the required secret names. |

The output hides the client secret, the token, and the access token like the secret values. An error never shows the text of an Infisical answer: a proxy or a server can put anything into it. FAM shows the HTTP status and its own message, for example "Infisical has no such project, environment, or folder" for HTTP 404. The `siteUrl` must not contain a user name or a password.

On the cockpit, the credentials file must not be `~/.config/fam/secrets.env`, because apply writes that file.

### Required secrets

A bundle can list `requiredSecrets`, for example `requiredSecrets { "SENTRY_AUTH_TOKEN" }`.
Plan and apply warn when the secrets of the config do not supply a required name.
Text, JSON, and TOON show the warning. It does not change the exit code.
Absent bundles do not need secrets.

Without `secrets` in the config, FAM removes `~/.config/fam/secrets.env` only when its first line is the FAM header. On the cockpit, the source file must not be `~/.config/fam/secrets.env` itself.

## Bundles

- A bundle entry is a bundle module or `new { bundle = …; state = … }`.
- The `name` identifies a bundle. Two files with the same name are an error.
- A machine entry wins over the global entry with the same name.
- `supported` defaults to `true`. Set it from the machine facts, for example `supported = facts.libc != "musl"`.
- When `supported` is `false`, FAM skips a present bundle and its verify scripts. Plan, apply, and verify show the skip. Text, JSON, and TOON show its reason. A skip does not fail the run.
- `unsupportedReason: String?` gives the reason, for example `"no musl build"`. Without a reason, FAM says the bundle does not support the machine.
- A skipped bundle adds no variables and no PATH entries to `~/.config/fam/env.sh`. FAM replaces this file on each apply, so earlier environment entries from the skipped bundle disappear.
- Files and managed skills from an earlier installation stay on the machine. FAM does not install, update, or remove the skipped bundle. Use `state = "absent"` to remove it. Absent bundles ignore `supported`.
- An absent bundle undoes its typed tasks in reverse order (`File`, `Symlink`, `Merge` keys, `Service`, `Linger`, `Package` with an `uninstall` command, `GitHubRelease`), then runs its `uninstall` tasks. FAM cannot undo a `Script`, so it skips scripts. FAM also removes the skill, the Claude link, and the variables of the bundle.

## Standard bundles

`@fam/std.pkl` has ready bundles for agent tools, in groups: `bundles { std.networking.portless(); std.media.hyperframes() }`. Each bundle is a function, so that it can take options later. Each bundle uses `fam.Package` when a suitable package exists. It selects the system package for the distribution, or an official npm, uv, Go, or Cargo package. Release downloads remain as fallbacks when no suitable package exists. Skills come from GitHub. CLI bundles do not install browsers or configure services. Language bundles install their named toolchain or manager. Other bundles require their documented runtimes or compilers. A skill without a tool is not a standard bundle: add it with `new fam.GitHubSkill { repo; path }` in a bundle of your own.

See the [standard bundle catalogue](std.md) for descriptions, install methods, skills, and required secrets.

Source packages need their build prerequisites before their bundle. Go packages need Go. Cargo packages need a current Rust toolchain and a C compiler. The Rustup bundle installs the manager, without a default compiler. Select the toolchain separately. Source builds need adequate temporary disk space. Set `TMPDIR` to a disk directory if `/tmp` has a small quota.

A bundle removes its previous FAM release binary before a package replaces it. FAM checks the repository and SHA-256 receipt first. A changed binary or receipt stops that task. Downloads from old custom scripts can lack a receipt. Remove those binaries manually if they hide a system package. The Go bundle recognizes and removes its previous FAM toolchain links.

## Services

- systemd: user units in `~/.config/systemd/user/`, system units in `/etc/systemd/system/` (system units need `sudo = true`). FAM sets `XDG_RUNTIME_DIR` for `systemctl --user`. A user unit that must run without a login session also needs a `Linger` task.
- launchd: LaunchAgents in `~/Library/LaunchAgents/` load into the GUI domain of the user (`gui/<uid>`), which exists only while the user is logged in at the screen. Without a GUI session, FAM stops the task with a clear error. Use `scope = "system"` (a LaunchDaemon in `/Library/LaunchDaemons/`) for a service without a login.
- A machine without systemd or launchd (for example Alpine with OpenRC) stops `Service` and `Linger` tasks with a clear error.

## Packages

A `Package` task names a `package`, and FAM installs, updates, and removes it with a package manager. A string is an npm package; an object selects the manager:

```pkl
new fam.Package { package = "agent-browser" }                                       // npm
new fam.Package { package = new { package = "tree"; manager = "apt" } }
```

`packageManagers` in `config.pkl` (and in a machine entry, which wins) replaces the manager for a package name in every bundle, also in the standard bundles: `packageManagers { ["agent-browser"] = "vite" }`. A bundle cannot be amended in the config for this, because Pkl cannot give the location of an amended module object (`reflect.Module(module)` fails).

| Manager | Is it installed? | Install | Update | Remove | Runs as |
|---|---|---|---|---|---|
| `npm` | `npm ls -g` (no global folder yet: not installed) | `npm install -g` | `npm install -g <p>@latest` | `npm uninstall -g` | user |
| `bun` | `bun pm ls -g` | `bun add -g` | `bun add -g <p>@latest` | `bun remove -g` | user |
| `vite` | `vp list -g` | `vp install -g` | `vp update -g` | `vp uninstall -g` | user |
| `uv` | `uv tool list` | `uv tool install` | `uv tool upgrade` | `uv tool uninstall` | user |
| `cargo` | Cargo install records | `cargo install --locked` | `cargo install --locked` | `cargo uninstall` | user |
| `go` | `go version -m` | `go install <module>@latest` | the install command | remove the matching binary | user |
| `system` | the selected manager | brew on macOS, or apt, dnf, pacman, apk on Linux | the selected manager | the selected manager | as selected |
| `brew` | `brew list` | `brew install` | `brew upgrade` | `brew uninstall` | user |
| `apt` | `dpkg-query` | `apt-get install` | `apt-get install --only-upgrade` | `apt-get remove` | sudo |
| `dnf` | `rpm -q` | `dnf install` | `dnf upgrade` | `dnf remove` | sudo |
| `pacman` | `pacman -Q` | `pacman -S --needed` | (none: no partial upgrades; `pacman -Syu` updates the system) | `pacman -R` | sudo |
| `yay` | `pacman -Q` | `yay -S --needed` | (as pacman) | `yay -R` | user (yay runs sudo) |
| `apk` | `apk info -e` | `apk add` | `apk add --upgrade` | `apk del` | sudo |

FAM asks the package manager, not `PATH`, whether the package is installed. When the manager itself is missing, the plan shows a change (an earlier bundle can install it), and apply stops with a clear error if it is still missing. `binary` defaults to the package name without its npm scope. Own `install`, `update`, and `uninstall` commands win over the commands of the manager.

Cargo uses `--root ~/.local`. Go uses `GOBIN=~/.local/bin`. A `PackageRef` can give `names` for each manager, for example `names { ["apt"] = "fd-find" }`. FAM uses the generic `package` name when no name matches the selected manager.

A `GitHubRelease` task installs a release binary into `~/.local/bin`, without sudo. It supports bare binaries, tar.gz, tar.xz, tar.bz2, and zip. Plan compares the installed version with the latest tag, or a pinned `tag`. FAM checks SHA-256 checksum files when present. See [Package tasks](development.md#package-tasks) for the schema and an example.

On Linux, FAM detects glibc or musl from the machine. On musl, `assets` uses `linux-amd64-musl` or `linux-arm64-musl` when present. Otherwise, FAM uses the plain Linux key. Prefer static or musl builds for the plain Linux keys when available, because they run everywhere.

## Agents

| Agent | Install | Update | Pin | Settings |
|---|---|---|---|---|
| Claude Code | `curl -fsSL https://claude.ai/install.sh \| bash` | `claude update` | `bash -s <version>`, then `claude install <version>` | `~/.claude/settings.json` (JSON merge) |
| Codex | `curl -fsSL https://chatgpt.com/codex/install.sh \| CODEX_NON_INTERACTIVE=1 sh` | the install command again | `sh -s -- --release <version>` | `~/.codex/config.toml` (TOML merge) |

Sources: https://code.claude.com/docs/en/setup and https://developers.openai.com/codex/cli. Both install into `~/.local/bin`. An update that does not change the output of `--version` is "unchanged".

A TOML merge writes the file again with sorted keys and without comments. A JSON merge keeps the key order.

## Output and run logs

Text, `--json`, and `--toon` print the same result structure (`internal/output`). Each run writes a JSON lines file to `~/.local/state/fam/runs/<run id>.jsonl` (or `$XDG_STATE_HOME/fam/runs/`): every command with its script, exit code, duration, stdout, and stderr, then the result. FAM does not log the content of files that it reads, nor stdin data.

Exit code of `fam`: `0` when every machine is OK (a plan with changes is OK), `1` otherwise.
