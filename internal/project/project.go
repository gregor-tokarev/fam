// Package project creates and upgrades FAM project trees.
package project

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gregor-tokarev/fam/internal/schema"
)

// NewOptions configures `fam new`.
type NewOptions struct {
	Base string
	// Agents overrides agent detection when it is not nil.
	Agents []string
}

// New creates the project tree in opts.Base. It stops when the base already contains a project.
func New(opts NewOptions) ([]string, error) {
	base := opts.Base
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	for _, f := range []string{"config.pkl", "machines.pkl", "agents.pkl", schema.Dir} {
		if _, err := os.Stat(filepath.Join(base, f)); err == nil {
			return nil, fmt.Errorf("%s already exists. `fam new` does not change an existing project. Use `fam upgrade` to update %s", filepath.Join(base, f), schema.Dir)
		}
	}
	agents := opts.Agents
	if agents == nil {
		agents = DetectAgents()
	}
	files := map[string]string{
		"config.pkl":                 configPkl,
		"machines.pkl":               machinesPkl,
		"agents.pkl":                 agentsPkl(agents),
		"instructions/AGENTS.md":     "",
		"skills/.gitkeep":            "",
		"bundles/hello.pkl":          helloBundle,
		"bundles/files/fam-hello.sh": helloScript,
		"bundles/worker-setup.pkl":   workerSetup,
		".gitignore":                 "",
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var written []string
	for _, name := range names {
		p := filepath.Join(base, name)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return written, err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(files[name]), mode); err != nil {
			return written, err
		}
		written = append(written, name)
	}
	if err := schema.Write(base); err != nil {
		return written, err
	}
	written = append(written, schema.Dir+"/")
	return written, nil
}

// Upgrade replaces `.fam/` with the schema of this FAM version.
func Upgrade(base string) (old string, err error) {
	if _, err := os.Stat(filepath.Join(base, "config.pkl")); err != nil {
		return "", fmt.Errorf("%s has no config.pkl. Run `fam upgrade` in a FAM project, or set --base", base)
	}
	old, _ = schema.ProjectVersion(base)
	return old, schema.Write(base)
}

// DetectAgents returns the agents that it finds on this machine.
func DetectAgents() []string {
	home, _ := os.UserHomeDir()
	var found []string
	check := func(name, bin, dir string) {
		if _, err := exec.LookPath(bin); err == nil {
			found = append(found, name)
			return
		}
		if _, err := os.Stat(filepath.Join(home, ".local", "bin", bin)); err == nil {
			found = append(found, name)
			return
		}
		if _, err := os.Stat(filepath.Join(home, dir)); err == nil {
			found = append(found, name)
		}
	}
	check("claude", "claude", ".claude")
	check("codex", "codex", ".codex")
	return found
}

func agentsPkl(agents []string) string {
	var b strings.Builder
	b.WriteString("amends \".fam/Agents.pkl\"\n\n")
	b.WriteString("// FAM installs each agent CLI, updates it on each `fam apply`,\n")
	b.WriteString("// and merges `settings` into the settings file of the agent.\n")
	b.WriteString("agents {\n")
	if len(agents) == 0 {
		b.WriteString("  // [\"claude\"] { settings { autoMemoryEnabled = false } }\n")
		b.WriteString("  // [\"codex\"] {}\n")
	}
	for _, a := range agents {
		fmt.Fprintf(&b, "  [%q] {}\n", a)
	}
	b.WriteString("}\n")
	return b.String()
}

const configPkl = `amends ".fam/Config.pkl"
import ".fam/std.pkl"

// Bundles for every machine.
bundles {
  import("bundles/hello.pkl")
  // std.agentBrowser
}

machines = import("machines.pkl").machines
agents = import("agents.pkl").agents

// secrets {
//   delivery = "copy-file"
//   file = "~/.config/fam/secrets.env"
// }

// Bundles for every machine with a role, after the bundles of every machine.
roles {
  ["worker"] { bundles { import("bundles/worker-setup.pkl") } }
}
`

const machinesPkl = `amends ".fam/Machines.pkl"

machines {
  ["cockpit"] {
    role = "cockpit"
  }
  // ["worker1"] {
  //   role = "worker"
  //   address = "worker1.example"
  //   user = "me"
  // }
}
`

const helloBundle = `amends "../.fam/Bundle.pkl"
import "../.fam/tasks.pkl" as fam

name = "hello"
description = "An example bundle: one small script in ~/.local/bin"

env {
  ["FAM_HELLO_GREETING"] = "Hello from FAM"
}

instructions {
  "Run ` + "`fam-hello`" + ` to check that FAM bundles work on this machine."
}

tasks {
  new fam.File {
    path = "~/.local/bin/fam-hello"
    content = read("files/fam-hello.sh").text
    mode = "0755"
  }
}

verify {
  new { event = "fresh_shell"; script = "fam-hello" }
}
`

const helloScript = `#!/bin/sh
echo "${FAM_HELLO_GREETING:-Hello} ($(hostname))"
`

const workerSetup = `amends "../.fam/Bundle.pkl"
import "../.fam/tasks.pkl" as fam
import "../.fam/facts.pkl"

name = "worker-setup"
description = "An example setup for the worker machines"

tasks {
  when (facts.os == "linux") {
    new fam.Linger {}
  }
  new fam.Merge {
    path = "~/.config/fam-example/settings.json"
    format = "json"
    values {
      cpus = facts.cpus
    }
  }
}
`
