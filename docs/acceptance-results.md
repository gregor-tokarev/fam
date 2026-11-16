# Acceptance results

Final run of the VM harness (`go run ./acceptance run …`) with the code of the last review round. Details per scenario: `acceptance/results/<target>.json`.

| # | Scenario | ubuntu-26.04 | ubuntu-24.04 | debian-13 | fedora-45 | arch | alpine-3.24 | macos |
|---|---|---|---|---|---|---|---|---|
| 1 | plan shows the changes and changes nothing | pass | pass | pass | pass | pass | pass | pass |
| 2 | apply, then plan is satisfied and apply changes nothing | pass | pass | pass | pass | pass | pass | pass |
| 3 | verify: fresh_shell and reconnect | pass | pass | pass | pass | pass | pass | pass |
| 4 | drift is found and repaired | pass | pass | pass | pass | pass | pass | pass |
| 5 | absent bundle removes skill, links, files, instruction lines | pass | pass | pass | pass | pass | pass | pass |
| 6 | global bundle removed on one machine | pass | pass | pass | pass | pass | pass | pass |
| 7 | secrets in shells, never in output or logs | pass | pass | pass | pass | pass | pass | pass |
| 8 | check exit 1 never applies, failed task stops the rest | pass | pass | pass | pass | pass | pass | pass |
| 9 | network stop gives a connection error (255) | pass | pass | pass | pass | pass | pass | pass |
| 10 | foreign skill folder makes the plan stop with a conflict | pass | pass | pass | pass | pass | pass | pass |
| 11 | unknown template variable stops FAM | pass | pass | pass | pass | pass | pass | pass |
| 12 | Claude Code and Codex: install, update, settings, paths | pass | pass | pass | pass | pass | pass | pass |
| 14 | standard bundle agentBrowser | pass | pass | pass | pass | pass | pass | pass |
| 15 | systemd tasks fail with a clear error without systemd | skip | skip | skip | skip | skip | pass | skip |
| 16 | FAM as the cockpit inside the macOS VM (local execution) | - | - | - | - | - | - | blocked |
| 17 | package from the OS package manager, chosen in config.pkl, then removed | pass | pass | pass | pass | pass | pass | skip |

| # | Scenario | Result |
|---|---|---|
| 13 | two VMs in parallel, then --serial (ubuntu-26.04 + debian-13) | pass |

## Windows

Windows Server 2025 in dockur/windows: **pass**. Windows Server 2025 (10.0.26100.1742) in dockur/windows, OpenSSH with cmd.exe: fam plan exit 1, machine status error, errorKind unsupported: Windows is not supported. FAM manages Linux and macOS machines only

## Notes

- Scenario 15 runs only on a machine without systemd or launchd (Alpine). Other targets skip it.
- Scenario 16 (macOS): FAM as the cockpit inside the macOS VM is blocked. The VM is an Intel Mac, and the pinned pkl 0.33.0 has no native binary for macOS on x86-64, so FAM cannot evaluate the config there.
- macOS runs with automatic login, because LaunchAgents load only in a GUI session.
