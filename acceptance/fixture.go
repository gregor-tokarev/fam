package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Test secret values. The scenarios search all output and the run logs for them.
var testSecrets = map[string]string{
	"FAM_TEST_TOKEN":  "tok-7f3a91c2-acceptance-secret",
	"FAM_TEST_QUOTED": "pa'ss \"wo$rd\" `x` 42-acceptance",
}

// machineEntry is one machine of the test project.
type machineEntry struct {
	Name   string
	Target Target
}

// writeProject writes the test project into dir. `fam new` writes PklProject before.
func writeProject(dir, secretsFile string, machines []machineEntry) error {
	var m strings.Builder
	m.WriteString("amends \"@fam/Machines.pkl\"\n\nmachines {\n")
	for _, e := range machines {
		fmt.Fprintf(&m, "  [%q] { tags { \"worker\" }; address = %q; user = \"fam\" }\n", e.Name, alias(e.Target.Name))
	}
	m.WriteString("}\n")
	files := map[string]string{
		"machines.pkl": m.String(),
		"config.pkl": fmt.Sprintf(`amends "@fam/Config.pkl"
import "@fam/std.pkl"

bundles {
  import("bundles/tool/bundle.pkl")
  import("bundles/global.pkl")
  import("bundles/fleet-skills.pkl")
}

machines = import("machines.pkl").machines

secrets {
  delivery = "copy-file"
  file = %q
}

tags {
  ["worker"] { bundles { import("bundles/worker.pkl") } }
}
`, secretsFile),
		"agents.pkl": `amends "@fam/Agents.pkl"
agents {}
`,
		// Scenario 5: the global bundle "tool" becomes absent.
		"config-absent.pkl": `amends "config.pkl"
import "@fam/base.pkl"

bundles {
  [0] = new base.BundleRef { bundle = import("bundles/tool/bundle.pkl"); state = "absent" }
}
`,
		// Scenario 6: a machine entry removes the global bundle "global" from one machine.
		"config-machine-absent.pkl": fmt.Sprintf(`amends "config.pkl"

machines {
  [%q] {
    bundles {
      new { bundle = import("bundles/global.pkl"); state = "absent" }
    }
  }
}
`, machines[0].Name),
		// Scenario 8: a failing check and a failing apply.
		"config-fail.pkl": `amends "config.pkl"

tags {
  ["worker"] { bundles { import("bundles/failing.pkl") } }
}
`,
		// Scenario 9: a slow script, so that the harness can stop the network during the run.
		"config-slow.pkl": `amends "config.pkl"

tags {
  ["worker"] { bundles { import("bundles/slow.pkl") } }
}
`,
		// Scenario 12: Claude Code and Codex. The "old" entrypoint pins older versions, so that
		// the normal entrypoint must update them.
		"config-agents-old.pkl": `amends "config.pkl"

agents {
  ["claude"] { version = "2.1.311"; settings { autoMemoryEnabled = false } }
  ["codex"] { version = "0.168.0"; settings { check_for_update_on_startup = false } }
}
`,
		"config-agents.pkl": `amends "config.pkl"

agents {
  ["claude"] { settings { autoMemoryEnabled = false; env { FAM_ACCEPTANCE = "1" } } }
  ["codex"] { settings { check_for_update_on_startup = false; tui { notifications = true } } }
}
`,
		// Alpine: systemd tasks without a guard must fail with a clear error.
		"config-systemd.pkl": `amends "config.pkl"

tags {
  ["worker"] { bundles { import("bundles/systemd.pkl") } }
}
`,
		// The standard bundle agentBrowser, with Node.js from its upstream binaries.
		"config-std.pkl": `amends "config.pkl"
import "@fam/std.pkl"

bundles {
  import("bundles/node.pkl")
  std.computer.agentBrowser()
}
`,
		"bundles/tool/bundle.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam

name = "tool"
env {
  ["FAM_TOOL_MODE"] = "acceptance"
}
tasks {
  new fam.File {
    path = "~/.local/bin/fam-tool"
    content = read("fam-tool.sh").text
    mode = "0755"
  }
  new fam.AgentSkill { source = "tool-skill" }
  new fam.AgentSkill { source = "notes"; name = "tool-notes" }
  new fam.Symlink { path = "~/.local/bin/fam-tool-link"; target = "~/.local/bin/fam-tool" }
  new fam.Merge {
    path = "~/.config/fam-tool/config.json"
    format = "json"
    values {
      mode = "acceptance"
      nested { level = 2 }
    }
  }
}
verify {
  new { name = "fam-tool runs"; event = "fresh_shell"; script = #"fam-tool | grep -q 'mode=acceptance'"# }
  new { name = "variables after reconnect"; event = "reconnect"; script = #"test "$FAM_TOOL_MODE" = acceptance && test -n "$FAM_TEST_TOKEN""# }
}
`,
		"bundles/tool/fam-tool.sh":               "#!/bin/sh\necho \"fam-tool mode=$FAM_TOOL_MODE\"\n",
		"bundles/tool/tool-skill/SKILL.md":       "---\nname: tool-skill\ndescription: The skill of the test tool.\n---\n\nRun `fam-tool`.\n",
		"bundles/tool/tool-skill/scripts/run.sh": "#!/bin/sh\nfam-tool\n",
		"bundles/tool/notes/SKILL.md":            "---\nname: tool-notes\ndescription: A second skill of the test tool.\n---\n\nNotes.\n",
		"bundles/fleet-skills.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam

name = "fleet-skills"
tasks {
  new fam.AgentSkill { source = "../skills/fleet-skill" }
}
`,
		"bundles/global.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam

name = "global"
tasks {
  new fam.File { path = "~/.local/share/fam-global/marker"; content = "global\n" }
}
verify {
  new { event = "fresh_shell"; script = "test -f ~/.local/share/fam-global/marker" }
}
`,
		"bundles/node.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam
import "@fam/facts.pkl"

name = "node"
tasks {
  when (facts.distro == "alpine") {
    new fam.Package { binary = "npm"; sudo = true; install = "apk add --no-cache nodejs npm"; uninstall = "apk del nodejs npm" }
    // Global npm packages go to ~/.local, as with the upstream binaries.
    new fam.Script {
      name = "npm prefix"
      check = #"[ "$(npm config get prefix)" = "$HOME/.local" ] && exit 0 || exit 100"#
      apply = #"npm config set prefix "$HOME/.local""#
    }
  } else {
    new fam.Package {
      binary = "npm"
      install = #"""
        set -e
        arch=$(uname -m); case "$arch" in x86_64) a=x64 ;; aarch64|arm64) a=arm64 ;; esac
        os=$(uname -s | tr A-Z a-z)
        v=$(curl -fsSL https://nodejs.org/dist/index.json | grep -o '"version":"v24[^"]*"' | head -1 | cut -d'"' -f4)
        mkdir -p "$HOME/.local"
        curl -fsSL "https://nodejs.org/dist/$v/node-$v-$os-$a.tar.gz" | tar -xz -C "$HOME/.local" --strip-components=1
        """#
    }
  }
}
`,
		// Source packages need compilers only inside this scenario's disposable VM.
		"bundles/build-tools.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam
import "@fam/facts.pkl"

name = "build-tools"
env {
  ["GOTOOLCHAIN"] = "auto"
  ["CARGO_BUILD_JOBS"] = "2"
  ["GOFLAGS"] = "-p=2"
  ["TMPDIR"] = "/var/tmp"
  ["CARGO_TARGET_DIR"] = "\(facts.home)/.cache/fam-build-target"
}
tasks {
  when (facts.os == "linux") {
    for (p in List("cloud-guest-utils", "e2fsprogs", "xfsprogs", "btrfs-progs")) {
      new fam.Package {
        package = new fam.PackageRef {
          package = p
          manager = "system"
          when (p == "cloud-guest-utils") {
            names { ["dnf"] = "cloud-utils-growpart"; ["apk"] = "cloud-utils-growpart" }
          }
        }
      }
    }
    new fam.Script {
      name = "Build disk space"
      sudo = true
      check = "df -Pk / | awk 'NR == 2 { exit !($2 >= 60 * 1024 * 1024) }' && exit 0 || exit 100"
      apply = #"""
        set -eu
        volume=$(df -P / | awk 'NR == 2 {print $1}')
        case "$volume" in
          /dev/vda[0-9]*) growpart /dev/vda "${volume#/dev/vda}" || [ "$?" -eq 1 ] ;;
          *) echo 'Unsupported VM root volume' >&2; exit 1 ;;
        esac
        fs=$(awk '$2 == "/" { print $3 }' /proc/mounts)
        case "$fs" in
          ext2|ext3|ext4) resize2fs "$volume" ;;
          xfs) xfs_growfs / ;;
          btrfs) btrfs filesystem resize max / ;;
          *) echo 'Unsupported VM root filesystem' >&2; exit 1 ;;
        esac
        """#
    }
  }
  for (p in List("gcc", "g++", "make", "pkg-config", "libssl-dev")) {
    new fam.Package {
      package = new fam.PackageRef {
        package = p
        manager = "system"
        names {
          when (p == "g++") { ["dnf"] = "gcc-c++"; ["pacman"] = "gcc"; ["brew"] = "gcc" }
          when (p == "pkg-config") { ["dnf"] = "pkgconf-pkg-config"; ["pacman"] = "pkgconf"; ["apk"] = "pkgconf"; ["brew"] = "pkgconf" }
          when (p == "libssl-dev") { ["dnf"] = "openssl-devel"; ["pacman"] = "openssl"; ["apk"] = "openssl-dev"; ["brew"] = "openssl@3" }
        }
      }
    }
  }
  when (facts.distro == "alpine") {
    for (p in List("musl-dev", "linux-headers")) {
      new fam.Package { package = new fam.PackageRef { package = p; manager = "system" } }
    }
  }
  new fam.Script {
    name = "Rust compiler prerequisite"
    check = "RUSTUP_AUTO_INSTALL=0 rustup run stable rustc --version >/dev/null 2>&1 && exit 0 || exit 100"
    apply = "rustup default stable"
  }
}
`,
		// Scenario 17: system packages, manager-specific names, and a GitHub release.
		"bundles/pkgtest.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam
import "@fam/facts.pkl"

name = "pkgtest"
tasks {
  new fam.Package { package = new fam.PackageRef { package = "tree"; manager = "system" } }
  new fam.Package { package = new { package = "fd"; manager = "system"; names { ["apt"] = "fd-find" } }; binary = if (facts.distro == "ubuntu" || facts.distro == "debian") "fdfind" else "fd" }
  new fam.GitHubRelease {
    repo = "junegunn/fzf"
    binary = "fzf"
    assets {
      ["linux-amd64"] { name = "fzf-{version}-linux_amd64.tar.gz"; format = "tar.gz"; file = "fzf" }
      ["linux-arm64"] { name = "fzf-{version}-linux_arm64.tar.gz"; format = "tar.gz"; file = "fzf" }
      ["darwin-arm64"] { name = "fzf-{version}-darwin_arm64.tar.gz"; format = "tar.gz"; file = "fzf" }
    }
  }
}
verify {
  new { event = "fresh_shell"; script = "tree --version" }
  new { event = "fresh_shell"; script = "fzf --version" }
  new { event = "fresh_shell"; script = if (facts.distro == "ubuntu" || facts.distro == "debian") "fdfind --version" else "fd --version" }
}
`,
		"config-pkg.pkl": `amends "config.pkl"

bundles {
  import("bundles/pkgtest.pkl")
}
`,
		"config-pkg-absent.pkl": `amends "config-pkg.pkl"
import "@fam/base.pkl"

bundles {
  [2] = new base.BundleRef { bundle = import("bundles/pkgtest.pkl"); state = "absent" }
}
`,
		"bundles/worker.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam
import "@fam/facts.pkl"

name = "worker"

tasks {
  new fam.File {
    path = "~/.local/lib/fleet/gate"
    content = read("files/gate.sh").text
    mode = "0755"
  }
  new fam.Merge {
    path = "~/.cargo/config.toml"
    format = "toml"
    values {
      build { jobs = facts.cpus ~/ 2 }
      profile { dev { debug = "line-tables-only" } }
    }
  }
  when (facts.init == "systemd") {
    new fam.Linger {}
    new fam.Service {
      name = "fam-demo.service"
      content = read("files/fam-demo.service").text
    }
    new fam.File {
      path = "~/.config/fam-demo/env"
      content = "MODE=acceptance\n"
      restarts { "fam-demo.service" }
    }
  }
  when (facts.init == "launchd") {
    // A restart request before the Service task: the bootstrap of the new job satisfies it,
    // so FAM must not restart the job again at the end of the bundle.
    new fam.File {
      path = "~/.config/fam-demo/env"
      content = "MODE=acceptance\n"
      restarts { "dev.fam.demo" }
    }
    new fam.Service {
      name = "dev.fam.demo"
      content = read("files/dev.fam.demo.plist").text
    }
  }
  new fam.Script {
    name = "marker"
    creates = "~/.local/share/fam-marker"
    apply = "mkdir -p ~/.local/share && touch ~/.local/share/fam-marker"
  }
  new fam.Script {
    name = "system marker"
    sudo = true
    check = #"[ -f /etc/fam-acceptance ] && exit 0 || exit 100"#
    apply = "echo fam > /etc/fam-acceptance"
  }
}
`,
		"bundles/files/gate.sh":          "#!/bin/sh\necho gate\n",
		"bundles/files/fam-demo.service": "[Unit]\nDescription=FAM acceptance demo\n\n[Service]\nEnvironmentFile=-%h/.config/fam-demo/env\nExecStart=/bin/sh -c 'while true; do sleep 3600; done'\n\n[Install]\nWantedBy=default.target\n",
		"bundles/files/dev.fam.demo.plist": `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>dev.fam.demo</string>
  <key>ProgramArguments</key><array><string>/bin/sleep</string><string>86400</string></array>
</dict>
</plist>
`,
		"bundles/failing.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam

name = "failing"

tasks {
  new fam.Script {
    name = "check returns 1"
    check = "exit 1"
    apply = "touch ~/fam-check-error-applied"
  }
}
`,
		"bundles/failing2.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam

name = "failing2"

tasks {
  new fam.Script {
    name = "apply fails"
    check = "exit 100"
    apply = "echo failing on purpose >&2; exit 7"
  }
  new fam.File { path = "~/fam-after-failure"; content = "x\n" }
}
`,
		"config-fail2.pkl": `amends "config.pkl"

tags {
  ["worker"] { bundles { import("bundles/failing2.pkl") } }
}
`,
		"bundles/slow.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam

name = "slow"

tasks {
  new fam.Script {
    name = "slow"
    check = "[ -f ~/fam-slow-done ] && exit 0 || exit 100"
    apply = "sleep 60; touch ~/fam-slow-done"
  }
}
`,
		"bundles/systemd.pkl": `amends "@fam/Bundle.pkl"
import "@fam/tasks.pkl" as fam

name = "systemd"

tasks {
  new fam.Service { name = "fam-demo.service"; content = "[Service]\nExecStart=/bin/true\n" }
  new fam.Linger {}
}
`,
		"instructions/AGENTS.md":      "# Machine {{ .Machine.Name }}\n\nOS: {{ .Facts.OS }} {{ .Facts.Distro }} {{ .Facts.DistroVersion }}, {{ .Facts.CPUs }} CPUs.\n",
		"instructions/CLAUDE.md":      "Claude-only acceptance line.\n",
		"instructions-bad/AGENTS.md":  "Agent: {{ .Agent }}\n",
		"skills/fleet-skill/SKILL.md": "---\nname: fleet-skill\ndescription: A project skill of the acceptance harness.\n---\n\nFleet skill.\n",
		// A file name with a backslash and spaces: the digest on the machine must read it back.
		"skills/fleet-skill/notes/a\\b c .md": "Notes.\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			return err
		}
	}
	return nil
}

func writeSecrets(path string) error {
	var b strings.Builder
	b.WriteString("# acceptance secrets\n")
	b.WriteString("FAM_TEST_TOKEN=" + testSecrets["FAM_TEST_TOKEN"] + "\n")
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`").Replace(testSecrets["FAM_TEST_QUOTED"])
	b.WriteString("FAM_TEST_QUOTED=\"" + q + "\"\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
