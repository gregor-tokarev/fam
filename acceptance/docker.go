package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The Windows target runs in a dockur/windows container (Windows in QEMU/KVM inside Docker).
// It runs alone, never together with other VMs. FAM must stop with "Windows is not supported".

const (
	windowsContainer = "fam-acceptance-windows"
	windowsPort      = 2271
	windowsAlias     = "fam-windows"
	windowsGuestIP   = "172.30.0.2" // the first DHCP address of dockur NAT networking
	windowsISOURL    = "https://go.microsoft.com/fwlink/?linkid=2293312&clcid=0x409&culture=en-us&country=us"
)

func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "sudo", append([]string{"-n", "docker"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// windowsSSHConfig reaches the guest sshd through the container: the published port of the
// Docker bridge does not pass the cockpit firewall, `docker exec … nc` does.
func (w *Work) windowsSSHConfig() (string, error) {
	p := filepath.Join(w.Dir, "ssh_config_windows")
	cfg := fmt.Sprintf("Host %s\n  User Docker\n  ProxyCommand sudo -n docker exec -i %s nc %s %d\n  IdentityFile %s\n  IdentitiesOnly yes\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n  LogLevel ERROR\n", windowsAlias, windowsContainer, windowsGuestIP, windowsPort, w.Key)
	return p, os.WriteFile(p, []byte(cfg), 0o644)
}

// runWindows installs Windows Server 2025 with OpenSSH, runs `fam plan` against it,
// and removes the container and its disk.
func runWindows(ctx context.Context, w *Work, fam string, keep bool) error {
	if n := countVMs(); n > 0 {
		return fmt.Errorf("%d other VMs run. The Windows VM must run alone", n)
	}
	storage := filepath.Join(w.Dir, "windows", "storage")
	oem := filepath.Join(w.Dir, "windows", "oem")
	for _, d := range []string{storage, oem} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	pub, err := os.ReadFile(w.Key + ".pub")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(oem, "administrators_authorized_keys"), pub, 0o644); err != nil {
		return err
	}
	// dockur copies /oem to C:\OEM and runs install.bat at the end of the installation.
	// sshd listens on windowsPort, because QEMU user networking forwards the same port number.
	bat := `@echo off
powershell -NoProfile -ExecutionPolicy Bypass -Command "Get-WindowsCapability -Online -Name OpenSSH.Server* | Where-Object State -ne 'Installed' | Add-WindowsCapability -Online"
powershell -NoProfile -ExecutionPolicy Bypass -Command "Set-Service -Name sshd -StartupType Automatic; Start-Service sshd; Stop-Service sshd"
powershell -NoProfile -ExecutionPolicy Bypass -Command "(Get-Content C:\ProgramData\ssh\sshd_config) -replace '^#?Port 22$','Port 2271' | Set-Content C:\ProgramData\ssh\sshd_config"
powershell -NoProfile -ExecutionPolicy Bypass -Command "New-NetFirewallRule -Name fam-sshd -DisplayName 'OpenSSH Server 2271' -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort 2271"
copy /Y C:\OEM\administrators_authorized_keys C:\ProgramData\ssh\administrators_authorized_keys
icacls C:\ProgramData\ssh\administrators_authorized_keys /inheritance:r /grant "Administrators:F" /grant "SYSTEM:F"
powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Service sshd"
`
	if err := os.WriteFile(filepath.Join(oem, "install.bat"), []byte(strings.ReplaceAll(bat, "\n", "\r\n")), 0o644); err != nil {
		return err
	}
	// dockur cannot resolve host names in host network mode on this cockpit, so the harness
	// uses an ISO that it downloads on the host (the Windows Server 2025 evaluation from Microsoft).
	iso := filepath.Join(w.Dir, "iso", "win2025.iso")
	if _, err := os.Stat(iso); err != nil {
		return fmt.Errorf("no Windows ISO at %s. Download the Windows Server 2025 evaluation ISO there: %s", iso, windowsISOURL)
	}
	_, _ = docker(ctx, "rm", "-f", windowsContainer)
	// The Docker bridge has no internet on this cockpit (the host firewall drops forwarded
	// traffic), so the ISO is local and the guest needs no download.
	if _, err := docker(ctx, "run", "-d", "--name", windowsContainer,
		"-e", "VERSION=2025", "-e", "RAM_SIZE=6G", "-e", "CPU_CORES=4", "-e", "DISK_SIZE=48G",
		"-e", fmt.Sprintf("USER_PORTS=%d", windowsPort), "-e", "DISPLAY=none",
		"--device=/dev/net/tun", "--cap-add", "NET_ADMIN",
		"--device=/dev/kvm",
		"-v", storage+":/storage", "-v", oem+":/oem", "--stop-timeout", "120",
		"-v", iso+":/boot.iso:ro",
		"docker.io/dockurr/windows"); err != nil {
		return err
	}
	if !keep {
		defer func() {
			_, _ = docker(context.Background(), "rm", "-f", windowsContainer)
			_ = exec.Command("sudo", "-n", "rm", "-rf", filepath.Join(w.Dir, "windows")).Run()
		}()
	}
	cfgPath, err := w.windowsSSHConfig()
	if err != nil {
		return err
	}
	fmt.Println("windows: installing Windows Server 2025 (download and unattended setup can take an hour)")
	deadline := time.Now().Add(150 * time.Minute)
	for {
		out, err := exec.CommandContext(ctx, "ssh", "-F", cfgPath, "-o", "ConnectTimeout=5", "-o", "BatchMode=yes", windowsAlias, "ver").CombinedOutput()
		if err == nil && strings.Contains(string(out), "Windows") {
			fmt.Printf("windows: SSH works: %s\n", strings.TrimSpace(string(out)))
			break
		}
		if time.Now().After(deadline) {
			logs, _ := docker(ctx, "logs", "--tail", "30", windowsContainer)
			return fmt.Errorf("windows: no SSH after the installation:\n%s", logs)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
	dir := filepath.Join(w.Dir, "run", "projects", "windows")
	_ = os.RemoveAll(dir)
	if out, err := exec.Command(fam, "new", "--base", dir).CombinedOutput(); err != nil {
		return fmt.Errorf("fam new: %v: %s", err, out)
	}
	machines := fmt.Sprintf("amends \".fam/Machines.pkl\"\nmachines {\n  [\"win\"] { role = \"worker\"; address = %q; user = \"Docker\" }\n}\n", windowsAlias)
	if err := os.WriteFile(filepath.Join(dir, "machines.pkl"), []byte(machines), 0o644); err != nil {
		return err
	}
	c := &Ctx{ctx: ctx, w: w, fam: fam, dir: dir, logDir: filepath.Join(w.Dir, "run", "state", "windows")}
	_ = os.MkdirAll(c.logDir, 0o755)
	cmd := exec.CommandContext(ctx, fam, "plan", "--json", "--ssh-config", cfgPath)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+c.logDir)
	out, _ := cmd.Output()
	text := exec.CommandContext(ctx, fam, "plan", "--ssh-config", cfgPath)
	text.Dir = dir
	tout, _ := text.CombinedOutput()
	res := ScenarioResult{Target: "windows", N: 1, Name: "Windows: FAM stops with a clear error"}
	if strings.Contains(string(out), "Windows is not supported") && strings.Contains(string(out), `"errorKind": "unsupported"`) {
		res.Result = "pass"
		res.Detail = firstLineS(extractLine(string(tout), "error"))
	} else {
		res.Result = "fail"
		res.Detail = string(tout)
	}
	fmt.Printf("windows %s: %s\n", strings.ToUpper(res.Result), res.Detail)
	if err := saveResults(filepath.Join("acceptance", "results"), "windows", []ScenarioResult{res}); err != nil {
		return err
	}
	if res.Result != "pass" {
		return fmt.Errorf("the Windows scenario failed")
	}
	return nil
}

func extractLine(s, prefix string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, prefix) {
			return strings.TrimSpace(l)
		}
	}
	return strings.TrimSpace(s)
}

func countVMs() int {
	out, _ := exec.Command("pgrep", "-x", "qemu-system-x86").Output()
	n := 0
	for _, pid := range strings.Fields(string(out)) {
		if b, err := os.ReadFile("/proc/" + pid + "/status"); err == nil && !strings.Contains(string(b), "zombie") {
			n++
		}
	}
	return n
}
