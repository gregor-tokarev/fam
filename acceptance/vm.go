package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Target is one operating system of the matrix.
type Target struct {
	Name     string
	Image    string // file name in the image cache
	Port     int    // SSH port on 127.0.0.1
	MemoryMB int
	CPUs     int
	Systemd  bool
	// Packages to install in the base image (the "dev machine" baseline).
	Packages []string
	// Extra provisioning commands, as root.
	RunCmd []string
	// BootScript boots a prepared VM with QEMU -snapshot instead of the cloud-image path
	// (macOS: acceptance/macos-boot.sh). Its QMP socket is QMP.
	BootScript string
	QMP        string
}

var targets = []Target{
	{Name: "ubuntu-26.04", Image: "ubuntu-26.04.qcow2", Port: 2261, Systemd: true,
		Packages: []string{"curl", "zsh", "git", "ca-certificates", "tar"}},
	{Name: "ubuntu-24.04", Image: "ubuntu-24.04.qcow2", Port: 2262, Systemd: true,
		Packages: []string{"curl", "zsh", "git", "ca-certificates", "tar"}},
	{Name: "debian-13", Image: "debian-13.qcow2", Port: 2263, Systemd: true,
		Packages: []string{"curl", "zsh", "git", "ca-certificates", "tar", "sudo", "dbus-user-session"}},
	{Name: "fedora-45", Image: "fedora-45.qcow2", Port: 2264, Systemd: true,
		Packages: []string{"curl", "zsh", "git", "tar"}},
	{Name: "arch", Image: "arch.qcow2", Port: 2265, Systemd: true,
		Packages: []string{"curl", "zsh", "git", "tar", "sudo"},
		// cloud-init cannot install packages on the Arch image before the pacman keyring is ready.
		RunCmd: []string{"systemctl start pacman-init.service; for i in 1 2 3 4 5 6; do pacman -Syu --noconfirm --needed zsh git sudo curl tar && break; sleep 10; done"}},
	{Name: "alpine-3.24", Image: "alpine-3.24.qcow2", Port: 2266, Systemd: false,
		Packages: []string{"curl", "zsh", "bash", "git", "tar", "sudo", "coreutils"},
		// With cloud-init disabled, Alpine needs its own network and SSH configuration.
		RunCmd: []string{
			"printf 'auto lo\\niface lo inet loopback\\n\\nauto eth0\\niface eth0 inet dhcp\\n' > /etc/network/interfaces",
			"rc-update add networking boot",
			"rc-update add sshd default",
			// sshd without PAM rejects a locked account ("!"): "*" has no password and is not locked.
			"sed -i 's/^fam:!:/fam:*:/' /etc/shadow",
		}},
	// macOS Sonoma with OSX-KVM, installed by hand once (see docs/acceptance.md).
	{Name: "macos", Port: 2267, Systemd: false, BootScript: "acceptance/macos-boot.sh",
		QMP: filepath.Join(os.Getenv("HOME"), ".cache", "fam-acceptance", "macos", "qmp.sock")},
}

func findTarget(name string) (Target, error) {
	for _, t := range targets {
		if t.Name == name {
			if t.MemoryMB == 0 {
				t.MemoryMB = 4096
			}
			if t.CPUs == 0 {
				t.CPUs = 3
			}
			return t, nil
		}
	}
	return Target{}, fmt.Errorf("unknown target %q", name)
}

// Work holds the paths of the harness.
type Work struct {
	Dir    string // ~/.cache/fam-acceptance
	Images string
	Bases  string
	Key    string
	SSHCfg string
}

func newWork() (*Work, error) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cache", "fam-acceptance")
	w := &Work{Dir: dir, Images: filepath.Join(dir, "images"), Bases: filepath.Join(dir, "bases"), Key: filepath.Join(dir, "id_ed25519"), SSHCfg: filepath.Join(dir, "ssh_config")}
	for _, d := range []string{w.Images, w.Bases, filepath.Join(dir, "run")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(w.Key); err != nil {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "fam-acceptance", "-f", w.Key).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("ssh-keygen: %s", out)
		}
	}
	var b strings.Builder
	for _, t := range targets {
		fmt.Fprintf(&b, "Host %s\n  HostName 127.0.0.1\n  Port %d\n  User fam\n  IdentityFile %s\n  IdentitiesOnly yes\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n  LogLevel ERROR\n\n", alias(t.Name), t.Port, w.Key)
	}
	if err := os.WriteFile(w.SSHCfg, []byte(b.String()), 0o644); err != nil {
		return nil, err
	}
	return w, nil
}

// alias is the SSH host alias of a target, for example "fam-ubuntu-26-04".
func alias(name string) string {
	return "fam-" + strings.NewReplacer(".", "-").Replace(name)
}

// VM is one running QEMU process.
type VM struct {
	T       Target
	cmd     *exec.Cmd
	qmpPath string
	runDir  string
	w       *Work
	done    chan struct{}
}

func (w *Work) basePath(t Target) string { return filepath.Join(w.Bases, t.Name+".qcow2") }

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
	return nil
}

// Prepare builds the base image of a target: the cloud image, provisioned once with cloud-init.
func (w *Work) Prepare(ctx context.Context, t Target, force bool) error {
	base := w.basePath(t)
	if _, err := os.Stat(base); err == nil && !force {
		return nil
	}
	src := filepath.Join(w.Images, t.Image)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("no cloud image %s: %w", src, err)
	}
	tmp := base + ".tmp"
	_ = os.Remove(tmp)
	if err := run("qemu-img", "convert", "-O", "qcow2", src, tmp); err != nil {
		return err
	}
	if err := run("qemu-img", "resize", tmp, "20G"); err != nil {
		return err
	}
	pub, err := os.ReadFile(w.Key + ".pub")
	if err != nil {
		return err
	}
	seedDir := filepath.Join(w.Dir, "run", t.Name+"-seed")
	_ = os.RemoveAll(seedDir)
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		return err
	}
	userData := cloudConfig(t, strings.TrimSpace(string(pub)))
	if err := os.WriteFile(filepath.Join(seedDir, "user-data"), []byte(userData), 0o644); err != nil {
		return err
	}
	meta := fmt.Sprintf("instance-id: fam-%s\nlocal-hostname: %s\n", t.Name, alias(t.Name))
	if err := os.WriteFile(filepath.Join(seedDir, "meta-data"), []byte(meta), 0o644); err != nil {
		return err
	}
	seed := filepath.Join(seedDir, "seed.iso")
	if err := run("cloud-localds", seed, filepath.Join(seedDir, "user-data"), filepath.Join(seedDir, "meta-data")); err != nil {
		return err
	}
	vm, err := w.start(t, tmp, seed, false)
	if err != nil {
		return err
	}
	// cloud-init powers the VM off at the end of provisioning.
	select {
	case <-time.After(25 * time.Minute):
		vm.Kill()
		return fmt.Errorf("%s: provisioning did not finish in 25 minutes (see %s)", t.Name, filepath.Join(vm.runDir, "console.log"))
	case <-vm.done:
	}
	// Keep the console log of provisioning: the next boot uses a new run folder.
	provLog := filepath.Join(w.Dir, "run", t.Name+"-provision-console.log")
	if b, err := os.ReadFile(filepath.Join(vm.runDir, "console.log")); err == nil {
		_ = os.WriteFile(provLog, b, 0o644)
	}
	// Check that provisioning wrote its marker.
	vm2, err := w.start(t, tmp, "", false)
	if err != nil {
		return err
	}
	defer vm2.Kill()
	if err := vm2.WaitSSH(ctx, 5*time.Minute); err != nil {
		return err
	}
	out, err := vm2.SSH(ctx, "cat /var/lib/fam-acceptance-ready")
	if err != nil || !strings.Contains(out, "ready") {
		return fmt.Errorf("%s: provisioning failed: %v %s (see %s)", t.Name, err, out, filepath.Join(vm.runDir, "console.log"))
	}
	if _, err := vm2.SSH(ctx, "sudo -n sync; sudo -n poweroff || sudo -n halt -p"); err != nil {
		_ = err
	}
	vm2.WaitExit(2 * time.Minute)
	return os.Rename(tmp, base)
}

func cloudConfig(t Target, pub string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("users:\n  - default\n  - name: fam\n    sudo: \"ALL=(ALL) NOPASSWD:ALL\"\n    shell: /bin/bash\n    lock_passwd: true\n")
	fmt.Fprintf(&b, "    ssh_authorized_keys:\n      - %s\n", pub)
	b.WriteString("package_update: true\npackages:\n")
	for _, p := range t.Packages {
		fmt.Fprintf(&b, "  - %s\n", p)
	}
	b.WriteString("runcmd:\n")
	cmds := append([]string{}, t.RunCmd...)
	cmds = append(cmds,
		// The sudoers rule for every distribution (Alpine and Arch have no default include of the cloud-init rule).
		"mkdir -p /etc/sudoers.d && echo 'fam ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-fam && chmod 0440 /etc/sudoers.d/90-fam",
		"command -v zsh >/dev/null && command -v bash >/dev/null && command -v sudo >/dev/null && echo ready > /var/lib/fam-acceptance-ready",
		"touch /etc/cloud/cloud-init.disabled",
	)
	for _, c := range cmds {
		q, _ := json.Marshal(c)
		fmt.Fprintf(&b, "  - [sh, -c, %s]\n", q)
	}
	b.WriteString("power_state:\n  mode: poweroff\n  timeout: 60\n  condition: true\n")
	return b.String()
}

// Start boots a clean VM from the base image. QEMU writes all changes to a temporary file
// (-snapshot), so each boot starts from the clean base.
func (w *Work) Start(ctx context.Context, t Target) (*VM, error) {
	vm, err := w.start(t, w.basePath(t), "", true)
	if err != nil {
		return nil, err
	}
	wait := 4 * time.Minute
	if t.BootScript != "" {
		wait = 8 * time.Minute
		// The OpenCore picker waits for a choice: select the second entry (the macOS disk).
		time.Sleep(30 * time.Second)
		for _, k := range []string{"right", "ret"} {
			if err := vm.qmp("send-key", map[string]any{"keys": []map[string]string{{"type": "qcode", "data": k}}}); err != nil {
				vm.Kill()
				return nil, fmt.Errorf("select the macOS disk in OpenCore: %w", err)
			}
			time.Sleep(time.Second)
		}
	}
	if err := vm.WaitSSH(ctx, wait); err != nil {
		vm.Kill()
		return nil, err
	}
	return vm, nil
}

func (w *Work) start(t Target, disk, seed string, snapshot bool) (*VM, error) {
	runDir := filepath.Join(w.Dir, "run", t.Name)
	_ = os.RemoveAll(runDir)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	if t.BootScript != "" {
		cmd := exec.Command(t.BootScript, "snapshot")
		logf, _ := os.Create(filepath.Join(runDir, "qemu.log"))
		cmd.Stdout = logf
		cmd.Stderr = logf
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		vm := &VM{T: t, cmd: cmd, qmpPath: t.QMP, runDir: runDir, w: w, done: make(chan struct{})}
		go func() { _ = cmd.Wait(); close(vm.done) }()
		return vm, nil
	}
	qmp := filepath.Join(runDir, "qmp.sock")
	args := []string{
		"-machine", "q35,accel=kvm", "-cpu", "host",
		"-m", fmt.Sprint(t.MemoryMB), "-smp", fmt.Sprint(t.CPUs),
		"-display", "none", "-serial", "file:" + filepath.Join(runDir, "console.log"),
		"-drive", "file=" + disk + ",if=virtio,format=qcow2",
		"-netdev", fmt.Sprintf("user,id=n0,hostfwd=tcp:127.0.0.1:%d-:22", t.Port),
		"-device", "virtio-net-pci,netdev=n0,id=nic0",
		"-qmp", "unix:" + qmp + ",server=on,wait=off",
		"-device", "virtio-rng-pci",
	}
	if seed != "" {
		args = append(args, "-drive", "file="+seed+",if=virtio,format=raw,readonly=on")
	}
	if snapshot {
		args = append(args, "-snapshot")
	}
	cmd := exec.Command("qemu-system-x86_64", args...)
	logf, _ := os.Create(filepath.Join(runDir, "qemu.log"))
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	vm := &VM{T: t, cmd: cmd, qmpPath: qmp, runDir: runDir, w: w, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(vm.done) }()
	return vm, nil
}

func (vm *VM) sshArgs(extra ...string) []string {
	return append([]string{"-F", vm.w.SSHCfg, "-o", "ConnectTimeout=5", "-o", "BatchMode=yes", alias(vm.T.Name)}, extra...)
}

// SSH runs a command in the VM (not through FAM).
func (vm *VM) SSH(ctx context.Context, script string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", vm.sshArgs("--", script)...).CombinedOutput()
	return string(out), err
}

// WaitSSH waits until SSH works.
func (vm *VM) WaitSSH(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", vm.T.Port), 2*time.Second)
		if err == nil {
			c.Close()
			if out, err := vm.SSH(ctx, "echo up"); err == nil && strings.Contains(out, "up") {
				return nil
			}
		}
		select {
		case <-vm.done:
			return fmt.Errorf("%s: QEMU exited", vm.T.Name)
		default:
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("%s: no SSH after %s (see %s)", vm.T.Name, timeout, filepath.Join(vm.runDir, "console.log"))
}

func (vm *VM) qmp(command string, args map[string]any) error {
	c, err := net.DialTimeout("unix", vm.qmpPath, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	r := bufio.NewReader(c)
	if _, err := r.ReadString('\n'); err != nil { // greeting
		return err
	}
	enc := json.NewEncoder(c)
	if err := enc.Encode(map[string]any{"execute": "qmp_capabilities"}); err != nil {
		return err
	}
	if _, err := r.ReadString('\n'); err != nil {
		return err
	}
	msg := map[string]any{"execute": command}
	if args != nil {
		msg["arguments"] = args
	}
	if err := enc.Encode(msg); err != nil {
		return err
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return err
		}
		if strings.Contains(line, `"return"`) {
			return nil
		}
		if strings.Contains(line, `"error"`) {
			return fmt.Errorf("qmp %s: %s", command, line)
		}
	}
}

// SetLink turns the network card of the VM on or off.
func (vm *VM) SetLink(up bool) error {
	return vm.qmp("set_link", map[string]any{"name": "nic0", "up": up})
}

// WaitExit waits for QEMU to stop.
func (vm *VM) WaitExit(timeout time.Duration) {
	select {
	case <-vm.done:
	case <-time.After(timeout):
		vm.Kill()
	}
}

// Kill stops QEMU. With -snapshot, all changes are lost.
func (vm *VM) Kill() {
	_ = vm.qmp("quit", nil)
	time.Sleep(500 * time.Millisecond)
	if vm.cmd.Process != nil {
		_ = vm.cmd.Process.Kill()
	}
	select {
	case <-vm.done:
	case <-time.After(10 * time.Second):
	}
	_ = exec.Command("ssh", "-F", vm.w.SSHCfg, "-O", "exit", alias(vm.T.Name)).Run()
}
