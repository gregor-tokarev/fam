// Package facts collects facts about a machine over its transport.
package facts

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gregor-tokarev/fam/internal/transport"
)

// Facts describe one machine. FAM gives them to Pkl as `fam.<name>` properties
// and to instruction templates as `.Facts`.
type Facts struct {
	Machine       string `json:"machine" toon:"machine"`
	Role          string `json:"role" toon:"role"`
	OS            string `json:"os" toon:"os"`
	Distro        string `json:"distro" toon:"distro"`
	DistroVersion string `json:"distroVersion" toon:"distroVersion"`
	Arch          string `json:"arch" toon:"arch"`
	Libc          string `json:"libc" toon:"libc"`
	CPUs          int    `json:"cpus" toon:"cpus"`
	Home          string `json:"home" toon:"home"`
	User          string `json:"user" toon:"user"`
	Hostname      string `json:"hostname" toon:"hostname"`
	Init          string `json:"init" toon:"init"`
	Shell         string `json:"shell" toon:"shell"`
	// BashSSHRC is true when Bash reads ~/.bashrc for noninteractive SSH commands
	// (Bash built with SSH_SOURCE_BASHRC, for example on Debian, Ubuntu, Fedora; not on Arch).
	// It is not a Pkl property.
	BashSSHRC bool `json:"bashReadsBashrcOverSSH" toon:"bashReadsBashrcOverSSH"`
}

// Properties returns the external Pkl properties.
func (f Facts) Properties() map[string]string {
	return map[string]string{
		"fam.machine":       f.Machine,
		"fam.role":          f.Role,
		"fam.os":            f.OS,
		"fam.distro":        f.Distro,
		"fam.distroVersion": f.DistroVersion,
		"fam.arch":          f.Arch,
		"fam.libc":          f.Libc,
		"fam.cpus":          strconv.Itoa(f.CPUs),
		"fam.home":          f.Home,
		"fam.user":          f.User,
		"fam.hostname":      f.Hostname,
		"fam.init":          f.Init,
		"fam.shell":         f.Shell,
	}
}

// ErrWindows means that the machine runs Windows.
var ErrWindows = errors.New("Windows is not supported. FAM manages Linux and macOS machines only")

const marker = "FAM-FACTS-V1"

const script = `echo ` + marker + `
k=$(uname -s 2>/dev/null)
echo "kernel=$k"
echo "machine=$(uname -m 2>/dev/null)"
if [ "$k" = Darwin ]; then
  echo "distro=macos"
  echo "distroVersion=$(sw_vers -productVersion 2>/dev/null)"
  echo "cpus=$(sysctl -n hw.ncpu 2>/dev/null)"
  echo "shell=$(dscl . -read "/Users/$(id -un)" UserShell 2>/dev/null | awk '{print $2}')"
elif [ -r /etc/os-release ]; then
  ( . /etc/os-release; echo "distro=${ID:-linux}"; echo "distroVersion=${VERSION_ID:-}" )
fi
if [ "$k" = Linux ]; then
  if getconf GNU_LIBC_VERSION >/dev/null 2>&1; then echo libc=glibc
  elif ldd --version 2>&1 | grep -qi musl; then echo libc=musl
  else
    for loader in /lib/ld-musl-*.so.1; do
      if [ -f "$loader" ]; then echo libc=musl; break; fi
    done
  fi
fi
if [ "$k" != Darwin ]; then
  echo "cpus=$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc 2>/dev/null)"
  echo "shell=$(getent passwd "$(id -un)" 2>/dev/null | cut -d: -f7)"
fi
echo "home=$HOME"
echo "user=$(id -un)"
echo "hostname=$(hostname 2>/dev/null || uname -n)"
if [ -d /run/systemd/system ]; then echo init=systemd
elif [ "$k" = Darwin ]; then echo init=launchd
else echo init=none; fi
echo "envshell=$SHELL"
b=$(command -v bash 2>/dev/null)
if [ -n "$b" ] && grep -a -q SSH2_CLIENT "$b" 2>/dev/null; then echo bashsshrc=yes; else echo bashsshrc=no; fi
`

// Collect runs the fact script on the machine.
func Collect(ctx context.Context, t transport.Transport, machine, role string) (Facts, error) {
	r, err := t.Run(ctx, transport.Cmd{Script: script, Timeout: 2 * time.Minute, Label: "collect facts"})
	if err != nil {
		return Facts{}, err
	}
	out := string(r.Stdout)
	if r.Code != 0 || !strings.Contains(out, marker) {
		if isWindows(ctx, t, out+"\n"+string(r.Stderr)) {
			return Facts{}, ErrWindows
		}
		return Facts{}, fmt.Errorf("collect facts: the machine did not run a POSIX shell script (exit code %d): %s", r.Code, firstLine(r.Err()+" "+r.Out()))
	}
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if ok {
			if _, seen := kv[k]; !seen || v != "" {
				kv[k] = v
			}
		}
	}
	kernel := kv["kernel"]
	if k := strings.ToUpper(kernel); strings.HasPrefix(k, "MINGW") || strings.HasPrefix(k, "MSYS") || strings.HasPrefix(k, "CYGWIN") || strings.Contains(k, "WINDOWS") {
		return Facts{}, ErrWindows
	}
	f := Facts{
		Machine:       machine,
		Role:          role,
		Distro:        kv["distro"],
		DistroVersion: kv["distroVersion"],
		Home:          kv["home"],
		User:          kv["user"],
		Hostname:      kv["hostname"],
		Init:          kv["init"],
		Shell:         kv["shell"],
		BashSSHRC:     kv["bashsshrc"] == "yes",
		Libc:          kv["libc"],
	}
	switch kernel {
	case "Linux":
		f.OS = "linux"
	case "Darwin":
		f.OS = "darwin"
	default:
		return Facts{}, fmt.Errorf("unsupported operating system %q. FAM manages Linux and macOS machines only", kernel)
	}
	switch m := kv["machine"]; m {
	case "x86_64", "amd64":
		f.Arch = "amd64"
	case "aarch64", "arm64":
		f.Arch = "arm64"
	default:
		f.Arch = m
	}
	f.CPUs, _ = strconv.Atoi(strings.TrimSpace(kv["cpus"]))
	if f.CPUs < 1 {
		f.CPUs = 1
	}
	if f.Shell == "" {
		f.Shell = kv["envshell"]
	}
	if f.Shell == "" {
		f.Shell = "/bin/sh"
	}
	if f.Home == "" || f.User == "" {
		return Facts{}, errors.New("collect facts: no home directory or user name")
	}
	if f.Distro == "" {
		f.Distro = f.OS
	}
	return f, nil
}

// isWindows recognizes Windows from the error of the fact script (cmd.exe or PowerShell
// cannot find `sh`), then runs `ver`, which cmd.exe (the default OpenSSH shell on Windows) knows.
func isWindows(ctx context.Context, t transport.Transport, out string) bool {
	low := strings.ToLower(out)
	for _, s := range []string{"windows", "is not recognized as an internal or external command", "is not recognized as the name of a cmdlet"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	r, err := t.Run(ctx, transport.Cmd{Script: "ver", LoginShell: true, Timeout: time.Minute, Label: "detect Windows"})
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(r.Stdout)+string(r.Stderr)), "microsoft windows")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
