<h1 align="center">FAM</h1>

<p align="center">Fleet Agent Manager: the same working tools, skills, and agent instructions on every machine of your agent fleet.</p>

<p align="center">
  <a href="docs/how-fam-works.md">How FAM works</a>
  ·
  <a href="examples/fleet">Example fleet</a>
  ·
  <a href="https://github.com/gregor-tokarev/fam/issues">Feedback &amp; ideas</a>
</p>

You run coding agents on several machines. Each machine needs the same CLIs, the same secrets in every shell, the same skills, and the same instructions for Claude Code and Codex. FAM keeps all of this in one Pkl project on your control machine and puts it on every machine over SSH.

- **One config.** Machines, reusable bundles of tools and setup, agents, and secrets in typed Pkl files, with editor help.
- **Nothing on the machines.** Workers need only SSH. FAM checks the real state on each run, so a plan always shows the truth.
- **Ready bundles.** agent-browser, agent-desktop, portless, HyperFrames, and more agent tools with their skills, in one line each.
- **Proof, not hope.** Each bundle has checks that FAM runs in new shells after each apply.
- **Safe changes.** `fam plan` changes nothing. FAM never shows secret values, stops on files that it did not make, and saves a copy before it replaces your own instruction files.

## Install

On Linux (x86-64 or arm64) or a Mac with Apple Silicon:

```sh
curl -fsSL https://raw.githubusercontent.com/gregor-tokarev/fam/main/install.sh | sh
```

FAM goes to `~/.local/bin`, with `pkl` when you do not have it yet (for the Pkl plugin of your editor). Install it only on the machine that you work from: the other machines need only SSH.

To update FAM, run `fam update`.

## Use

```sh
fam new      # create a project
fam plan     # show what is different on each machine
fam apply    # make the machines match, then prove that the tools work
fam verify   # prove it again
fam migrate  # pin the schema of a new FAM version in a project
fam update   # update FAM itself
```

## Build from source

Install Go 1.26 or later.

```sh
git clone https://github.com/gregor-tokarev/fam.git
cd fam
make build
```
