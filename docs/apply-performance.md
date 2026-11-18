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

- **Checks run at the same time.** A check changes nothing. Plan, the check before the apply loop, and the check of the shell blocks run up to 8 checks of a machine at the same time. The default `MaxSessions` of sshd is 10. When sshd refuses a session, `ssh` opens a separate connection for it, so the command is slower but does not fail. After a connection error, FAM starts no more checks and lets the running checks end. A check must not depend on another check, for example through a lock that only one check can hold.
- **One check for a satisfied task.** A task with a satisfied check before the apply loop does not check again until a task changes the machine. A task that changes the machine (an apply or an update) always checks right before it.
- **Updates make FAM check again.** An update script can change files of other tasks, also without a new version. After an update, FAM checks the next tasks again at the same time, up to the next package with an update command or the next task that was not satisfied in the first pass. Each of these results, also an error, is the check of its task until a task changes the machine. After an apply or a restart, each next task checks again before it runs.
- **Fewer waits for each package update.** The commands are the same as before, and each has its own timeout. After the update, the search for the program and its version run at the same time: both only read. Together they are the full check of the task, so the third search is not necessary. An update now waits for four round trips, not six.
- **Verify scripts run at the same time.** Up to 8 `fresh_shell` scripts of a machine run at the same time. The two shells of one script run one after the other. The report keeps the order of the config. The `reconnect` scripts run after them, one at a time. Two verify scripts must not share a fixed port or a file.
- **Machines evaluate the Pkl config at the same time.**

The output of plan and apply does not change.

## Results

Test setup: one Ubuntu 26.04 VM of the acceptance harness (3 vCPU, 4 GB) with the example fleet: 35 tasks, 8 packages with an update command, 3 remote skills, and 24 verify shells. The machine was already applied, so each run changed nothing. A proxy added 50 ms in each direction (100 ms round trip).

| Command | Network | Before | After |
|---|---|---|---|
| `fam apply` | 100 ms round trip | 45.3 s, 45.2 s, 45.2 s | 22.9 s, 21.6 s, 21.3 s |
| `fam plan` | 100 ms round trip | 12.1 s, 11.9 s, 11.8 s, 12.1 s, 11.9 s | 3.5 s, 3.6 s, 3.6 s, 3.5 s |
| SSH commands in one apply | | 160 | 144 |
| SSH commands in one plan | | 46 | 46 |

One more plan run of the new binary took 7.9 s: the skill download from GitHub took 5.4 s in that run. Both binaries do this download in the same way.

Time of each step of one apply with a 100 ms round trip:

| Step | Before | After |
|---|---|---|
| Facts and Pkl evaluation | 1.4 s | 1.2 s |
| Skill downloads and task build | 1.1 s | 1.0 s |
| Checks before the apply loop | 9.5 s | 1.4 s |
| Apply loop (updates and checks) | 25.7 s | 16.5 s |
| Shell blocks | 1.1 s | 0.2 s |
| Verify | 6.5 s | 1.4 s |

The update commands themselves (`claude update`, `rustup update`, `vp upgrade`, and others) take approximately 8 s of the apply loop, one update after the other. The other 8.6 s of the loop is the network wait of the checks and version commands around them (17.8 s before). Their time changes from run to run with the network and the package registries.

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
