# Apply performance

This page records the time of `fam apply` and `fam plan` before and after the performance change of FAM 0.20.x. It also tells how to measure them again.

## Where the time goes

FAM sends each check, apply, update, and verify step to a worker as one `ssh` command. All commands share one ControlMaster connection. A new session on that connection costs approximately two network round trips.

The fleet VPN has a round trip of 60 ms to 210 ms. With 100 ms, one command costs approximately 0.22 s, also when the command itself takes 1 ms. A steady-state apply (nothing to change) of the example fleet sent 160 commands, one after the other. The network wait was larger than all other work together.

The old apply did these steps one after the other:

- It ran the check of each task two times: once before the apply loop (to find conflicts) and once in the apply loop.
- It ran each check and each verify shell alone.
- A package update used six commands, one after the other: find, version, update, find, version, find.
- It evaluated the Pkl config of one machine at a time.

## What changed

- **Checks run at the same time.** A check changes nothing. Plan, the check before the apply loop, and the check of the shell blocks run up to 8 checks of a machine at the same time. The default `MaxSessions` of sshd is 10. When sshd refuses a session, `ssh` opens a separate connection for it, so the command is slower but does not fail. After a connection error, FAM starts no more checks and lets the running checks end.
- **One check for a satisfied task.** A task with a satisfied check before the apply loop does not check again until a task changes the machine. A task that changes the machine (an apply or an update) always checks right before it.
- **Updates make FAM check again.** An update script can change files of other tasks, also without a new version. After an update, FAM checks the next tasks again at the same time, up to the next package with an update command. A connection error in these checks fails the task and stops the machine. After an apply or a restart, each next task checks again before it runs.
- **Fewer waits for each package update.** The commands are the same as before, and each has its own timeout. Commands that only read run at the same time: the check with the version before the update, and the search with the version after it. The search and the version after the update are the full check of the task, so the third search is not necessary. An update now waits for three round trips, not six.
- **Verify scripts run at the same time.** Up to 8 `fresh_shell` scripts of a machine run at the same time. The two shells of one script run one after the other. The report keeps the order of the config. The `reconnect` scripts run after them, one at a time. Two verify scripts must not share a fixed port or a file.
- **Machines evaluate the Pkl config at the same time.**

The output of plan and apply does not change.

## Results

Test setup: one Ubuntu 26.04 VM of the acceptance harness (3 vCPU, 4 GB) with the example fleet: 35 tasks, 8 packages with an update command, 3 remote skills, and 24 verify shells. The machine was already applied, so each run changed nothing. A proxy added 50 ms in each direction (100 ms round trip).

| Command | Network | Before | After |
|---|---|---|---|
| `fam apply` | 100 ms round trip | 48.4 s, 54.6 s, 48.0 s | 18.7 s, 18.0 s, 18.0 s |
| `fam plan` | 100 ms round trip | 12.4 s | 3.2 s |
| SSH commands in one apply | | 160 | 108 |
| SSH commands in one plan | | 46 | 40 |

Time of each step of one apply with a 100 ms round trip:

| Step | Before | After |
|---|---|---|
| Facts and Pkl evaluation | 1.4 s | 1.2 s |
| Skill downloads and task build | 1.6 s | 0.8 s |
| Checks before the apply loop | 9.8 s | 1.1 s |
| Apply loop (updates and checks) | 28.0 s | 14.1 s |
| Shell blocks | 1.1 s | 0.2 s |
| Verify | 6.5 s | 1.3 s |

The update commands themselves (`claude update`, `rustup update`, `vp upgrade`, and others) take approximately 10 s of the apply loop, one update after the other. Their time changes from run to run with the network and the package registries.

The first apply of a new machine takes approximately 28 minutes in the VM. The downloads inside the VM take almost all of that time, for example 17 minutes for rustup. The change does not make these downloads faster.

## Measure again

Prerequisites:

- A base image of the acceptance harness (see [the acceptance harness](acceptance.md)).
- A test project that uses the VM as its only machine. Copy `examples/fleet`, and change `machines.pkl` and the secrets file.

1. Boot a VM:

   ```sh
   go run ./acceptance up ubuntu-26.04
   ```

2. Write an SSH config with a slow alias. The `HostName` of the slow alias must be different, because the ControlMaster socket name contains it:

   ```
   Host fam-ubuntu-26-04-slow
     HostName slow-vm
     ProxyCommand python3 scripts/bench/delayproxy.py 127.0.0.1 2261 50

   Host fam-ubuntu-26-04 fam-ubuntu-26-04-slow
     HostName 127.0.0.1
     Port 2261
     User fam
     IdentityFile ~/.cache/fam-acceptance/id_ed25519
     IdentitiesOnly yes
     StrictHostKeyChecking no
     UserKnownHostsFile /dev/null
     LogLevel ERROR
   ```

3. Set `address = "fam-ubuntu-26-04-slow"` in `machines.pkl` of the test project.
4. Run apply one time to install the tools. Then measure a steady-state apply:

   ```sh
   python3 scripts/bench/tsrun.py fam apply --ssh-config <ssh config>
   ```

5. Read the run log for the number of commands, their time, and the slowest commands:

   ```sh
   python3 scripts/bench/analyze.py ~/.local/state/fam/runs/<run>.jsonl
   ```

The run log writes each command when it ends, with its duration in `durationMs`.
