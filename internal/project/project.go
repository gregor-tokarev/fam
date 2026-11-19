// Package project creates and migrates FAM project trees.
package project

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gregor-tokarev/fam/internal/schema"
	"github.com/gregor-tokarev/fam/internal/version"
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
	for _, f := range []string{"config.pkl", "machines.pkl", "agents.pkl", schema.ProjectFile} {
		if _, err := os.Stat(filepath.Join(base, f)); err == nil {
			return nil, fmt.Errorf("%s already exists. `fam new` does not change an existing project. Use `fam migrate` to change the FAM version of a project", filepath.Join(base, f))
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
	if err := pinPackage(base); err != nil {
		return written, err
	}
	written = append(written, schema.ProjectFile, schema.DepsFile, "AGENTS.md")
	return written, nil
}

// Migrate pins the fam package of this FAM version in the PklProject of a project.
func Migrate(base string) (old string, err error) {
	if _, err := os.Stat(filepath.Join(base, "config.pkl")); err != nil {
		return "", fmt.Errorf("%s has no config.pkl. Run `fam migrate` in a FAM project, or set --base", base)
	}
	old, _, _ = schema.Pinned(base)
	return old, pinPackage(base)
}

// pinPackage writes PklProject and PklProject.deps.json for the package of this binary, puts
// the package into the Pkl cache, and writes the project state of the Pkl language server, so
// that editors find the package at once, without "Sync Projects".
func pinPackage(base string) error {
	p, err := schema.Current()
	if err != nil {
		return err
	}
	if err := schema.SeedAll(p); err != nil {
		return fmt.Errorf("write the fam package into the Pkl cache: %w", err)
	}
	if err := schema.WriteProject(base, p); err != nil {
		return err
	}
	if err := schema.WriteEditorState(base, p); err != nil {
		return fmt.Errorf("write the editor state in .pkl-lsp: %w", err)
	}
	return WriteGuide(base)
}

//go:embed guide.md
var guide string

// WriteGuide updates only the FAM section of AGENTS.md.
func WriteGuide(base string) error {
	const begin, end = "<!-- fam:begin -->", "<!-- fam:end -->"
	path := filepath.Join(base, "AGENTS.md")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	section := begin + "\n" + strings.TrimSpace(strings.ReplaceAll(guide, "{{version}}", version.Version)) + "\n" + end
	text := string(old)
	start, stop := strings.Index(text, begin), strings.Index(text, end)
	if start >= 0 || stop >= 0 {
		if start < 0 || stop < start || strings.Count(text, begin) != 1 || strings.Count(text, end) != 1 {
			return fmt.Errorf("%s has invalid FAM guide markers. Keep one complete section", path)
		}
		text = text[:start] + section + text[stop+len(end):]
	} else {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if text != "" {
			text += "\n"
		}
		text += section + "\n"
	}
	return os.WriteFile(path, []byte(text), 0o644)
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
	b.WriteString("amends \"@fam/Agents.pkl\"\n\n")
	b.WriteString("// FAM installs each agent CLI, updates it with `fam apply --update`,\n")
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

const configPkl = `amends "@fam/Config.pkl"
import "@fam/std.pkl"

// Bundles for every machine.
bundles {
  import("bundles/hello.pkl")
  // std.computer.agentBrowser()
}

machines = import("machines.pkl").machines
agents = import("agents.pkl").agents

// secrets {
//   delivery = "copy-file"
//   file = "~/.config/fam/secrets.env"
// }
// Or read the secrets from Infisical:
// secrets {
//   infisical {
//     projectId = "<project ID>"
//     environment = "prod"
//     credentials = "~/.config/fam-secrets/infisical.env"
//   }
// }

// Bundles for every machine with a tag, after the bundles of every machine.
// A machine can have many tags: they come in the order of its own tags.
tags {
  ["worker"] { bundles { import("bundles/worker-setup.pkl") } }
}
`

const machinesPkl = `amends "@fam/Machines.pkl"

machines {
  ["cockpit"] {
    cockpit = true
  }
  // ["worker1"] {
  //   tags { "worker" }
  //   address = "worker1.example"
  //   user = "me"
  // }
}
`

const helloBundle = `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam

name = "hello"

env {
  ["FAM_HELLO_GREETING"] = "Hello from FAM"
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

const workerSetup = `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam
import "@fam/facts.pkl"

name = "worker-setup"

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
