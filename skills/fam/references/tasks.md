# FAM Tasks and Facts

## Bundle (`amends "@fam/Bundle.pkl"`)

- `name` (`[a-z0-9][a-z0-9._-]*`, unique), `env`, `path` (`~/.config/fam/env.sh`), `requiredSecrets`.
- `supported: Boolean = true`, `unsupportedReason: String?`: derive from `facts`. When `false`, skips present bundle, `env`, `path`, and `verify` (ignored when `state = "absent"`).
- `tasks`: ordered. Typed `StateTask` reverses on `state = "absent"`.
- `uninstall`: runs after reversed `tasks` on absent (use to undo `Script`).
- `verify`: `new { name = "..."; event = "fresh_shell"|"reconnect"; script = "..." }`. `fresh_shell` runs in noninteractive and login shells and skips package updates unless `--update`.

## Facts (`import "@fam/facts.pkl"`)

`machine`, `cockpit`, `tags` (`.contains("gpu")`), `os` (`linux|darwin`), `distro` (`ubuntu|debian|fedora|arch|alpine|macos`), `distroVersion`, `arch` (`amd64|arm64`), `libc` (`glibc|musl|""`), `cpus`, `home`, `user`, `hostname`, `init` (`systemd|launchd|none`), `shell`.

## Tasks (`import "@fam/tasks.pkl" as fam`)

Common: `sudo = false`, `restarts { "unit.service" }`, `state = "present"|"absent"` (typed tasks).

- **`fam.File`**: `path`, `content` (or `read("files/x").text`), `mode = "0644"`.
- **`fam.Merge`**: `path`, `format = "json"|"toml"`, `values { workers = facts.cpus }` (recursive merge, lists replace).
- **`fam.Symlink`**: `path`, `target` (fails if `path` is an existing file or dir).
- **`fam.Package`**: `package = "npm-pkg"`, or `package = new { package = "fd"; manager = "system"; names { ["apt"] = "fd-find" } }`. Custom (omit `package`): `binary`, `install` (required), `update`, `uninstall`, `versionCommand`. `system` -> `brew|apt|dnf|pacman|apk` (OS managers use `sudo`).
- **`fam.GitHubRelease`**: installs `~/.local/bin/<binary>`, no sudo, receipt `~/.local/state/fam/releases/<binary>.json`. Fields: `repo`, `binary`, optional `tag`, `versionCommand`, `assets { ["linux-amd64"] { name = "t-{version}.tar.gz"; format = "tar.gz"; file = "t" } }`. Keys: `linux-amd64`, `linux-arm64`, `linux-amd64-musl`, `linux-arm64-musl`, `darwin-arm64`. Formats: `binary` (default), `tar.gz`, `tar.xz`, `tar.bz2`, `zip` (archives need `file`). `{tag}` = full tag, `{version}` strips through last `/` and leading `v`.
- **`fam.Service`**: `name`, `scope = "user"|"system"` (default `"user"`, `"system"` requires `sudo = true`), optional `content`, `enabled = true`, `running = true`. Guard Linux with `when (facts.init == "systemd")`. Headless macOS needs `"system"`.
- **`fam.Linger`**: `sudo = true` default, optional `user`. Linux systemd only (`when (facts.init == "systemd")`).
- **`fam.Script`**: `name`, `apply`, plus `check` OR `creates`. `check` exits `0` (ok), `100` (run `apply` then re-check), other including `1` (error, aborts). Undo in `uninstall {}`.
- **`fam.AgentSkill`**: `source` (relative dir, GitHub URL or `/tree/<ref>/<path>`, archive/`.md` URL), optional `path`, `ref`, `name`. Never `~` or `sudo = true`.
