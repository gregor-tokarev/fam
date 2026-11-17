# Standard bundle package installation

Verification date: 2026-11-17.

Standard bundles use `fam.Package` when a suitable system or official ecosystem package exists. System package names refer to the intended upstream tool. Names alone do not establish identity.

Go now uses the distribution package, including `gofmt`. Source fallbacks use the official Go, Cargo, npm, or PyPI package. Their documented compilers and runtimes must exist first. Hurl declares its curl, XML, and libclang development packages. Coralogix declares its D-Bus development package. Cargo fallbacks on musl use dynamic linking so build scripts can load native libraries. 

Azure CLI permits its pinned prerelease dependencies in uv. This prevents older resolvers from selecting the obsolete 2.0.67 package.

## VM checks

All Linux checks use disposable QEMU snapshots with KVM. Shared base images stay unchanged. The Arch setup uses a full system upgrade for current repository metadata. The Ubuntu setup prevents concurrent automatic apt updates. 

The source builds use temporary 80 GiB disks, two Cargo jobs, and two Go build processes. The manual Ubuntu 26 VM also uses 8 GiB of swap after a Trivy compile exceeded its 4 GiB RAM limit. Scenario 16 now allocates at least 8 GiB of RAM for Linux source builds. Build files use disk storage because Ubuntu 26 mounts `/tmp` as a small tmpfs.

| Target | Native package installation and shell checks | Next plan | Repeat apply |
| --- | --- | --- | --- |
| Ubuntu 24.04 | Pass, 38 shell checks | Zero changes | Zero changes |
| Debian 13 | Pass, 46 shell checks | Zero changes | Zero changes |
| Fedora 45 | Pass, 74 shell checks | Zero changes | Zero changes |
| Arch | Pass, 102 shell checks | Zero changes | Zero changes |
| Alpine 3.24 | Pass, 84 shell checks | See full installation results below | Not repeated separately |

The final full plans report zero changes on both source-build VMs. No separate full repeat apply follows this plan. The complete apply already updates the packages from earlier installation runs.

The native selection includes bundles whose Package tasks all use the system manager. It excludes a source bundle that declares native build dependencies.

The full configurations receive incremental source installation, all bundle tasks, and startup checks.

| Target | Supported bundles | Startup checks | Final full plan |
| --- | --- | --- | --- |
| Ubuntu 26.04 | 144 | 288 passed | 540 satisfied, zero changes |
| Alpine 3.24 | 137 | 274 passed | 538 satisfied, zero changes |

Seven Alpine bundles remain unsupported by their upstream tools.

The Alpine full apply first exposes an obsolete Azure CLI selection. The corrected Package installs Azure CLI 2.93.0. A targeted repair retains all bundle environment and skills, then runs the full startup checks again.

The existing acceptance scenario 16 also passes on a clean Fedora VM with the `languages` filter. It verifies the expanded build disk, system Go and Rustup, stable Rust selection, language installation, skills, and a satisfied next plan.

## Migration and removal

An Ubuntu 24 VM first installs Go, ripgrep, and fd with the schema from the original repository commit. The new schema replaces these release installs with native packages. The next plan reports zero changes. A repeat apply reports zero changes and passes all six shell checks.

A separate check appends a byte to the old fd binary. Apply rejects that binary and preserves its checksum. The clean binary then migrates successfully. The fd alias also produces a valid migration plan before cleanup.

Removal passes for ripgrep, fd, jq, and Podman on apt, dnf, and pacman. Each removal produces a satisfied next plan. The Ubuntu 24 AWS fallback also passes installation, detached PGP signature verification, version checks, removal, and a satisfied next plan.

Ubuntu 26 and Alpine also pass removal for OpenAI CLI (Go), Hurl (Cargo and native build dependencies), Supabase (npm), and Ruff (uv). They also remove ripgrep, fd, jq, and Podman through apt or apk.

| Target | Removed executable names absent | Final removal plan |
| --- | --- | --- |
| Ubuntu 26.04 | 11 | 36 satisfied, zero changes |
| Alpine 3.24 | 10 | 48 satisfied, zero changes |

Older custom installers did not record ownership receipts. They include Docker, kubectl, Terraform, Helm, Linux Tailscale, musl Mise, and macOS AWS CLI. Existing binaries in `~/.local/bin` can take precedence over new system packages. Preserve or remove those custom files before migration. This migration leaves those files untouched because FAM cannot establish their ownership.

When the selected package manager changes, remove an earlier ecosystem installation with its previous manager. For example, an old npm Mise shim can take precedence over the new system package.

## Release fallbacks

These fallbacks apply only where the selected system manager has no suitable package.

| Tool | Reason for the release fallback |
| --- | --- |
| agg | The registry crate named `agg` is unrelated. Upstream source installation needs a Git checkout. |
| AWS CLI v2 on Ubuntu before 26 | The distribution package does not supply v2. The official ZIP has a verified PGP signature. |
| cloudflared, dtctl, newrelic, opentofu, pscale, pulumi, terraform, trufflehog | Upstream module replacement directives prevent versioned `go install`. |
| Daytona | Published npm and PyPI packages supply SDKs, rather than this CLI. |
| Docker on Ubuntu and Arch | The default Docker package also installs the engine. This bundle supplies only the client. |
| DuckDB on musl | The official `duckdb-cli` Python package supplies no musl wheel. |
| kubectl on Ubuntu | The default repositories have no suitable standalone client package. |
| Linear Release, and Zero outside Homebrew | Upstream npm workspace packages are private. Similarly named public packages are different tools. |
| migrate | Default Go installation omits the database drivers present in the release CLI. |
| Pixi | The published Cargo package is obsolete. Current upstream source installation needs a Git checkout. |
| Podman on Arch | The system package includes the engine. This bundle supplies only the remote client. |
| Render | The latest installable Go module resolves to an obsolete CLI major version. |
| Steel | The npm package only downloads an external binary. It does not manage updates or removal. The registry crate belongs to Solana. |
| Slack CLI | Go resolves an untagged development revision because the stable tags do not match the module path. |

The audit also rejects Apache Ant, Fedora's AI `goose`, the Solana `steel-cli` crate, and the Python implementation of `yq`. Each bundle selects its intended upstream package instead.

## Limits

The schema evaluates all 144 bundles for Ubuntu 24 and 26, Debian 13, Fedora 45, Arch, Alpine amd64 and arm64, and macOS arm64. Platform exclusions produce 137 supported bundles on Alpine amd64 and 136 on Alpine arm64.

macOS and arm64 package branches receive static evaluation only. No macOS or arm64 VM result is claimed. Shell checks cover installation, versions, and command startup. They do not verify authenticated cloud operations or browser sessions.

Host checks pass for `go build ./...`, `go vet ./...`, catalog generation, catalog validation, and `git diff --check`.

All disposable VMs stop after verification.

Raw command results and full run logs remain on the verification worker under `/tmp/fam-package-verification/` and `~/.local/state/fam/runs/`.
