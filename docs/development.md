# Development

## Layout

| Path | Content |
|---|---|
| `cmd/fam` | The `fam` command. |
| `internal/schema/fam` | The Pkl schema and the standard bundles: the files of the Pkl package `fam`. FAM embeds this folder and builds the package from it (`internal/schema`). `@FAM_VERSION@` becomes the FAM version. |
| `internal/pklgen` | Go structs generated from the schema. Do not edit. |
| `internal/pklbin` | Download and cache of the pinned `pkl` binary. |
| `internal/project` | `fam new`, `fam migrate`, and the embedded project guide in `guide.md`. Only `{{version}}` is substituted. |
| `internal/engine/check.go` | Static project validation through the config validators and task builders. It uses the embedded package and cached dependencies. It blocks network reads. |
| `internal/stdlib` | Catalogue reflection, selected bundle evaluation, and generated documentation. |
| `internal/cli` | Command registration, project diagnostics, and environment checks. |
| `agentbench` | Agent tasks, guide variants, measured results, and reports. See [the benchmark](agent-benchmark.md). |

## Build

```sh
make build          # builds ./fam with the version of the Makefile
go build ./cmd/fam  # the same, with the default version
```

`VERSION=0.3.0 make build` sets the FAM version.

FAM builds the `fam` package from the embedded files: a zip file with fixed dates and sorted entries, and its metadata. The same files and version give the same bytes, so the checksum in `PklProject.deps.json` of a project matches the package that the release publishes. A project accepts a binary of another patch release of its minor version, so **a patch release must not change the schema or a standard bundle**. A change to the schema or a standard bundle needs a new minor version.

A development build with changed schema files has the same version but another checksum: FAM asks for `fam migrate` in the project.

## Releases

Push a tag on `main` to publish a release. First pin the new version in the example project, and merge that change:

```sh
VERSION=0.9.0 make build && ./fam migrate --base examples/fleet
```

Then tag the merge commit:

```sh
git tag v0.9.0
git push origin v0.9.0
```

`.github/workflows/release.yml` runs `scripts/build-release.sh <version>`. It builds `fam_<os>_<arch>.tar.gz` for linux/amd64, linux/arm64, and darwin/arm64 (the platforms of the pinned pkl), and `checksums.txt` with the SHA-256 of each archive. It also writes the files of the `fam` Pkl package with `fam internal package`: the metadata `fam@<version>` and `fam@<version>.zip`. Then it creates the GitHub release with all these files. The version in the tag goes into `internal/version.Version`, which is also the version of the package.

- `install.sh` downloads the archive for its platform from `releases/latest/download` (or `FAM_VERSION`), checks it against `checksums.txt`, and installs `fam` into `FAM_INSTALL_DIR` (default `~/.local/bin`).
- `fam update` (`internal/selfupdate`) reads the latest tag from the redirect of `releases/latest` (no API token), downloads the archive and `checksums.txt`, checks the SHA-256, and replaces its own file with a rename. `fam update --check` only reports, and `fam update --version X` installs a given release.
- `.github/workflows/ci.yml` builds, vets, and runs the release build for each pull request.

## Generated Go structs

After a change to the schema, run:

```sh
make pklgen
```

The script runs the `pkl-go` code generator (`package://pkg.pkl-lang.org/pkl-go/pkl.golang@0.14.0#/gen.pkl`) with the pinned `pkl` binary. Each schema module maps to its own Go package under `internal/pklgen` (`config`, `base`, `bundle`, `tasks`, `machines`, `agents`), because the generator writes a `Load` function for each module. The modules import each other without a cycle: `tasks` ← `Bundle` ← `base` ← `Config`. The schema has no `@go.Package` annotations, so projects do not need the `pkl-go` package at evaluation time.

## Pinned pkl

FAM downloads `pkl` 0.33.0 from the GitHub release on first use into `<user cache dir>/fam/pkl/0.33.0/` and checks the SHA-256 that `internal/pklbin` pins. Supported cockpits: Linux amd64 (glibc and musl), Linux arm64, macOS arm64. Pkl 0.33.0 has no native build for macOS amd64.

## Standard bundles

A standard bundle installs only its tool, and `fam.GitHubSkill` tasks get the skills from the tool's GitHub repository. FAM vendors no skills. A standard bundle uses `fam.Package` when a suitable package exists. Prefer the system package when the distribution supplies the correct tool. Use an official ecosystem package otherwise. Keep release downloads only where no suitable package exists. A CLI bundle does not install browsers or configure services. Language bundles install their named toolchain or manager. A standard bundle always installs a tool: a skill alone is not a standard bundle. Without a `ref`, the skills come from the latest GitHub release. That is usually the version that the package manager installs, but a registry can be ahead of or behind GitHub.

Acceptance scenario 16 applies all standard bundles. A new standard bundle goes into `bundles/<group>/<name>.pkl` and `std/<group>.pkl`. The group module declares one function per bundle, `function <id>(): Bundle = import(...)`, with a doc comment for editor completion. A function lets a bundle take options later. `std.pkl` imports the groups. Pkl cannot call a function that reflection finds, so FAM reads the function names with `pkl:reflect` and builds the calls in Go. Scenario 16 calls every function of the selected groups and reads the expected skills from its first plan. A bundle can set `supported` from machine facts when its tool needs a specific platform.

Generate the [standard bundle catalogue](std.md) after a bundle change:

```sh
go run ./cmd/fam internal std-docs
go run ./cmd/fam internal std-docs --check
```

The command evaluates the embedded package with fixed Ubuntu 26.04 amd64 facts. It does not download skills or install tools. CI runs the check.

## Acceptance harness

`acceptance/` is a VM acceptance harness. FAM runs on the cockpit and targets QEMU/KVM VMs over SSH. See `docs/acceptance.md`.

```sh
go run ./acceptance prepare ubuntu-26.04   # download is separate: put the cloud image in ~/.cache/fam-acceptance/images
go run ./acceptance run ubuntu-26.04
```

## Package tasks

`PackageRef` selects `uv`, `cargo`, or `go` for user tools. Cargo and Go install into `~/.local/bin`. The manager must exist first. `system` selects brew on macOS, or apt, dnf, pacman, or apk from the Linux facts. Unknown distributions need an explicit manager.

```pkl
new fam.Package {
  package = new fam.PackageRef {
    package = "fd"
    manager = "system"
    names { ["apt"] = "fd-find" }
  }
  binary = if (facts.distro == "ubuntu") "fdfind" else "fd"
}
```

`GitHubRelease` installs one binary without sudo. Assets can be bare binaries, tar.gz, tar.xz, tar.bz2, or zip archives. An archive needs the file path of its binary. Asset names and archive paths can use `{tag}` or `{version}`. `{version}` uses the part after the last `/`, without a leading `v`.

FAM accepts at most 512 MiB for a binary and 1 GiB for an asset or uncompressed tar archive. The version command can print its version to stdout or stderr.

```pkl
new fam.GitHubRelease {
  repo = "junegunn/fzf"
  binary = "fzf"
  assets {
    ["linux-amd64"] { name = "fzf-{version}-linux_amd64.tar.gz"; format = "tar.gz"; file = "fzf" }
    ["linux-arm64"] { name = "fzf-{version}-linux_arm64.tar.gz"; format = "tar.gz"; file = "fzf" }
    ["darwin-arm64"] { name = "fzf-{version}-darwin_arm64.tar.gz"; format = "tar.gz"; file = "fzf" }
  }
}
```

Set `tag` to pin a release. FAM finds the latest tag otherwise. FAM downloads on the cockpit and uses the GitHub cache with ETags, size limits, and retries. It checks SHA-256 checksum assets when present. Set `checksum` on an asset for a nonstandard checksum name. Plan compares `<binary> --version` with the release tag. Set `versionCommand` when the tool needs another command. Its output must contain the tag as a word, with or without the leading `v`. An absent task or bundle removes the binary.
