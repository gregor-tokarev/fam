package main

import (
	"os"
	"path/filepath"
	"strings"
)

// writeFixture populates a benchmark FAM project inside dir and writes its
// secrets source file outside the project, inside home.
func writeFixture(dir, home string) error {
	secretsPath := filepath.Join(home, ".secrets", "bench.env")
	if err := os.MkdirAll(filepath.Dir(secretsPath), 0o700); err != nil {
		return err
	}
	secretsData := "# Benchmark secrets\nFAM_BENCH_TOKEN=bench-token-0123456789\nFAM_BENCH_ENDPOINT=https://bench.invalid\n"
	if err := os.WriteFile(secretsPath, []byte(secretsData), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(secretsPath, 0o600); err != nil {
		return err
	}

	files := map[string]string{
		"machines.pkl": `amends "@fam/Machines.pkl"

machines {
  ["cockpit"] {
    cockpit = true
    tags { "bench" }
  }
}
`,
		"agents.pkl": `amends "@fam/Agents.pkl"

agents {}
`,
		"config.pkl": `amends "@fam/Config.pkl"

bundles {
  import("bundles/tools/bundle.pkl")
  import("bundles/config-files.pkl")
}

machines = import("machines.pkl").machines
agents = import("agents.pkl").agents

secrets {
  delivery = "copy-file"
  file = "~/.secrets/bench.env"
}

tags {
  ["bench"] {
    bundles {
      import("bundles/bench-tag.pkl")
    }
  }
}
`,
		"bundles/tools/bundle.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam

name = "tools"

env {
  ["FAM_BENCH_ENV"] = "bench-ok"
}

requiredSecrets {
  "FAM_BENCH_TOKEN"
}

tasks {
  new fam.File {
    path = "~/.local/bin/fam-bench-tool"
    content = read("fam-bench-tool.sh").text
    mode = "0755"
  }
  new fam.Symlink {
    path = "~/.local/bin/fam-bench-link"
    target = "~/.local/bin/fam-bench-tool"
  }
  new fam.AgentSkill {
    source = "tool-skill"
  }
  new fam.Package {
    binary = "fam-bench-pkg"
    install = #"""
      mkdir -p "$HOME/.local/bin"
      printf '#!/bin/sh\necho "fam-bench-pkg 1.0.0"\n' > "$HOME/.local/bin/fam-bench-pkg"
      chmod 0755 "$HOME/.local/bin/fam-bench-pkg"
      """#
    update = "true"
    uninstall = #"rm -f "$HOME/.local/bin/fam-bench-pkg""#
  }
}

verify {
  new { name = "tool runs"; event = "fresh_shell"; script = #"fam-bench-tool | grep -q 'env=bench-ok'"# }
  new { name = "secret loaded"; event = "fresh_shell"; script = #"test -n "$FAM_BENCH_TOKEN""# }
  new { name = "pkg version"; event = "fresh_shell"; script = #"fam-bench-pkg --version | grep -q '1.0.0'"# }
}
`,
		"bundles/tools/fam-bench-tool.sh": "#!/bin/sh\necho \"fam-bench-tool env=${FAM_BENCH_ENV:-unset}\"\n",
		"bundles/tools/tool-skill/SKILL.md": "---\nname: tool-skill\ndescription: Benchmark skill for FAM tool bundle.\n---\n\nRun `fam-bench-tool`.\n",
		"bundles/tools/tool-skill/scripts/helper.sh": "#!/bin/sh\nfam-bench-tool\n",
		"bundles/config-files.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam
import "@fam/facts.pkl"

name = "config-files"

tasks {
  new fam.Merge {
    path = "~/.config/fam-bench/settings.json"
    format = "json"
    values {
      os = facts.os
      cpus = facts.cpus
      nested {
        enabled = true
        level = 2
      }
    }
  }
  new fam.Merge {
    path = "~/.config/fam-bench/settings.toml"
    format = "toml"
    values {
      host {
        arch = facts.arch
        distro = facts.distro
      }
    }
  }
  new fam.File {
    path = "~/.config/fam-bench/notes.txt"
    content = "benchmark config file\n"
  }
  new fam.Script {
    name = "marker creates"
    creates = "~/.local/share/fam-bench/created"
    apply = #"mkdir -p "$HOME/.local/share/fam-bench" && touch "$HOME/.local/share/fam-bench/created""#
  }
  new fam.Script {
    name = "marker state"
    check = #"[ -f "$HOME/.local/share/fam-bench/state" ] && [ "$(cat "$HOME/.local/share/fam-bench/state")" = "ready" ] && exit 0 || exit 100"#
    apply = #"mkdir -p "$HOME/.local/share/fam-bench" && printf 'ready\n' > "$HOME/.local/share/fam-bench/state""#
  }
}

verify {
  new { name = "merged configs"; event = "fresh_shell"; script = #"test -f "$HOME/.config/fam-bench/settings.json" && test -f "$HOME/.config/fam-bench/settings.toml""# }
  new { name = "script markers"; event = "fresh_shell"; script = #"test -f "$HOME/.local/share/fam-bench/created" && test -f "$HOME/.local/share/fam-bench/state""# }
}
`,
		"bundles/bench-tag.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam

name = "bench-tag"

tasks {
  new fam.File {
    path = "~/.local/share/fam-bench/tag-marker"
    content = "bench\n"
  }
  new fam.AgentSkill {
    source = "../skills/bench-skill"
  }
}

verify {
  new { name = "tag marker after reconnect"; event = "reconnect"; script = #"test -f "$HOME/.local/share/fam-bench/tag-marker" && test "$FAM_BENCH_ENV" = bench-ok"# }
}
`,
		"skills/bench-skill/SKILL.md": "---\nname: bench-skill\ndescription: Benchmark skill from tag bundle.\n---\n\nBenchmark tag skill.\n",
		"instructions/AGENTS.md":      "# Machine {{ .Machine.Name }}\n\nOS: {{ .Facts.OS }} {{ .Facts.Distro }} {{ .Facts.Arch }} ({{ .Facts.CPUs }} CPUs).\n",
		"instructions/CLAUDE.md":      "Claude benchmark instructions for {{ .Machine.Name }}.\n",
	}

	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			return err
		}
	}
	return nil
}
