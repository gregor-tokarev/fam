# Acceptance harness

`acceptance/` tests FAM against real VMs. FAM runs on the cockpit (the machine that runs the harness) and targets each VM over SSH, like a worker.

## Requirements

- Linux with KVM (`/dev/kvm`), `qemu-system-x86_64`, `qemu-img`, `cloud-localds`, `ssh`, `ssh-keygen`.
- The cloud images in `~/.cache/fam-acceptance/images/`:

| Target | File | Source |
|---|---|---|
| `ubuntu-26.04` | `ubuntu-26.04.qcow2` | `https://cloud-images.ubuntu.com/releases/26.04/release/ubuntu-26.04-server-cloudimg-amd64.img` |
| `ubuntu-24.04` | `ubuntu-24.04.qcow2` | `https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img` |
| `debian-13` | `debian-13.qcow2` | `https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2` |
| `fedora-45` | `fedora-45.qcow2` | `https://dl.fedoraproject.org/pub/fedora/linux/releases/45/Cloud/x86_64/images/Fedora-Cloud-Base-Generic-45-1.4.x86_64.qcow2` |
| `arch` | `arch.qcow2` | `https://geo.mirror.pkgbuild.com/images/latest/Arch-Linux-x86_64-cloudimg.qcow2` |
| `alpine-3.24` | `alpine-3.24.qcow2` | `https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/cloud/alpine-3.24.4-x86_64-cloudinit-r0.qcow2` |

## Commands

Run the harness from the root of the repository.

```sh
go run ./acceptance prepare <target>...     # provision a base image once (cloud-init, then power off)
go run ./acceptance run [-scenarios 1,2] [-std-groups cloud,data] <target>...
go run ./acceptance run -scenarios 13 <target> <target>   # two VMs in parallel, then --serial
go run ./acceptance up <target>             # boot a clean VM for manual work
go run ./acceptance windows                 # Windows: FAM must stop with "Windows is not supported"
go run ./acceptance clean                   # delete base images and runtime files
```

## How it works

- `prepare` converts the cloud image to a 20 GB qcow2 file and boots it once with a cloud-init seed. Provisioning creates the user `fam` with an SSH key and passwordless sudo, and installs the packages of a small developer machine (`curl`, `git`, `zsh`, `tar`, and on Alpine `bash` and `sudo`). Then the VM powers off, and the disk becomes the base image.
- Each scenario boots the base image with `qemu -snapshot`. QEMU writes all changes to a temporary file, so each scenario starts from a clean VM.
- Each VM has 4 GB of RAM and 3 vCPUs. The harness runs one VM for each scenario (two for scenario 13), so two harness processes stay within 12 GB and 6 vCPUs.
- The network is QEMU user networking with an SSH port on `127.0.0.1` (2261 to 2266). The harness writes `~/.cache/fam-acceptance/ssh_config` with one host alias for each target and gives it to FAM with `FAM_SSH_CONFIG`. FAM targets `fam@fam-<target>`.
- Scenario 9 stops the network card of the VM with the QEMU monitor (`set_link nic0 off`) while FAM runs a slow script.
- The test project, its secrets file, and the run logs of each scenario are in `~/.cache/fam-acceptance/run/`. Results go to `acceptance/results/<target>.json`.

Scenario 16 installs Node.js, npm, and uv inside the VM of the current run. The system package manager is also available. It installs no Go, Rust, or C compiler. The shared base images stay unchanged.

Scenario 16 discovers all standard bundles from the package. `-std-groups cloud,data` or `FAM_STD_GROUPS=cloud,data` limits it to those groups. Bundles with `supported = false` for the target facts are omitted. The scenario reads expected skills from the first plan, requires every verify to pass, and requires a satisfied second plan.

## Windows

`go run ./acceptance windows` runs Windows Server 2025 in `dockurr/windows` (QEMU/KVM in Docker) and removes the container and its disk at the end. It runs alone, never together with another VM.

- The Docker bridge of the cockpit has no internet (the cockpit firewall drops forwarded traffic), so the harness needs the evaluation ISO in `~/.cache/fam-acceptance/iso/win2025.iso` (from `https://go.microsoft.com/fwlink/?linkid=2293312&clcid=0x409&culture=en-us&country=us`).
- An OEM script turns on OpenSSH Server on port 2271 and installs the harness key for the user `Docker`.
- SSH goes through `docker exec … nc 172.30.0.2 2271` (a `ProxyCommand`), because the published port of the bridge does not pass the cockpit firewall either.
- The scenario: `fam plan` must stop with the error kind `unsupported` and "Windows is not supported".

## macOS

The macOS target is macOS Sonoma in plain QEMU with [OSX-KVM](https://github.com/kholia/OSX-KVM) (`acceptance/macos-boot.sh`). `dockurr/macos` cannot download the recovery image through the Docker bridge on this cockpit. It runs alone, never together with another VM.

Preparation, once (in `~/.cache/fam-acceptance/macos`):

1. Clone OSX-KVM. Run `python3 -I OSX-KVM/fetch-macOS-v2.py -s sonoma` in `dl/`, then `dmg2img dl/BaseSystem.dmg dl/BaseSystem.img`. Create `mac_hdd.qcow2` (80 GB).
2. `acceptance/macos-boot.sh install` and install macOS over VNC (`127.0.0.1:5931`): Disk Utility (erase the 80 GB disk as APFS), Reinstall macOS, then the setup assistant with the account `fam`.
3. In Terminal: passwordless sudo for `fam` (`/etc/sudoers.d/fam`), `launchctl enable system/com.openssh.sshd` and `launchctl bootstrap system /System/Library/LaunchDaemons/ssh.plist`, and the harness key in `~/.ssh/authorized_keys`.
4. Automatic login for `fam` (`/etc/kcpassword` and `autoLoginUser`): LaunchAgents load only in a GUI session.
5. Shut down. The disk is the clean base: each scenario boots it with `-snapshot`, and the harness selects the macOS disk in the OpenCore picker with QMP `send-key`.

FAM as the cockpit inside the Mac: the Mac is an Intel (x86-64) VM, and pkl 0.33.0 has no native macOS x86-64 binary, so `fam plan` stops with "pkl 0.33.0 has no native binary for darwin/amd64".

## Scenarios

| # | Scenario |
|---|---|
| 1 | `fam plan` shows the expected changes and changes nothing (a fingerprint of the home directory and the system files before and after). |
| 2 | `fam apply`, then a second plan shows only "satisfied", and a second apply changes nothing. Units restart once (on macOS, a restart request before the launchd Service task does not restart the job again). A skill file name with a backslash and spaces stays "satisfied". |
| 3 | `fam verify` passes `fresh_shell` (noninteractive and interactive) and `reconnect`. |
| 4 | Drift: hand changes to a file, a merged key, a skill file, an instruction file, and a mode. Plan finds them, apply repairs them. |
| 5 | A bundle becomes `absent`: FAM removes its file, link, merged keys, skill, Claude link, and variables. |
| 6 | A machine entry removes a global bundle from one machine. |
| 7 | Secrets: six shells export both test secrets. The values are not in the plan, `--json`, `--toon`, or the run logs. |
| 8 | A check with exit code 1 never starts its apply. A failed apply stops the tasks after it. |
| 9 | The network stops during a run: FAM reports exit code 255 as a connection error. |
| 10 | A foreign skill folder with the same name: the plan stops with a conflict, and apply changes nothing. |
| 11 | An unknown template variable (`{{ .Agent }}`) stops FAM. |
| 12 | Claude Code and Codex: pinned older versions, then an update to the latest; settings merged into existing files; skills and instructions at the agent paths. |
| 13 | Two VMs in parallel, then `--serial`. Text output is grouped by machine. |
| 14 | The standard bundle `agentBrowser`, with Node.js from a test bundle: the CLI, and its skill from the latest GitHub release. FAM installs no browser and sets no `AGENT_BROWSER_*` variables. |
| 15 | Machines without systemd (Alpine): systemd tasks fail with a clear error. |
| 16 | All standard bundles, with expected skills from the plan, every verify passed, and a satisfied second plan. |
| 17 | A bundle names only a package (`tree`); `system` selects the OS package manager. A second package uses a manager-specific name (`fd-find` on apt). A GitHub release installs fzf with its checksum. The next plan is satisfied. An absent bundle removes the packages and binary. |
