package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gregor-tokarev/fam/internal/output"
)

// Ctx is the context of one scenario on one VM.
type Ctx struct {
	ctx     context.Context
	w       *Work
	fam     string
	t       Target
	vm      *VM
	vms     []*VM
	dir     string
	logDir  string
	outputs []string // every fam output of the scenario, for the secret search
	notes   []string
}

// famRun is the result of one fam command.
type famRun struct {
	Stdout, Stderr string
	Code           int
	Res            *output.Run
}

func (c *Ctx) famCmd(args ...string) famRun {
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.fam, args...)
	cmd.Dir = c.dir
	cmd.Env = append(os.Environ(), "FAM_SSH_CONFIG="+c.w.SSHCfg, "XDG_STATE_HOME="+c.logDir)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	r := famRun{Stdout: so.String(), Stderr: se.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		r.Code = ee.ExitCode()
	} else if err != nil {
		r.Code = -1
		r.Stderr += err.Error()
	}
	c.outputs = append(c.outputs, r.Stdout, r.Stderr)
	for _, a := range args {
		if a == "--json" {
			var res output.Run
			if json.Unmarshal(so.Bytes(), &res) == nil {
				r.Res = &res
			}
		}
	}
	return r
}

// famJSON runs fam with --json and fails without a result.
func (c *Ctx) famJSON(args ...string) (famRun, error) {
	r := c.famCmd(append(args, "--json")...)
	if r.Res == nil {
		return r, fmt.Errorf("fam %s: no JSON result (exit %d): %s %s", strings.Join(args, " "), r.Code, tailS(r.Stdout, 20), tailS(r.Stderr, 20))
	}
	return r, nil
}

func tailS(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (c *Ctx) ssh(script string) (string, error) { return c.vm.SSH(c.ctx, script) }

func (c *Ctx) mustSSH(script string) (string, error) {
	out, err := c.ssh(script)
	if err != nil {
		return out, fmt.Errorf("ssh %q: %v: %s", script, err, out)
	}
	return out, nil
}

func machineOf(r *output.Run) (*output.MachineResult, error) {
	if r == nil || len(r.Machines) == 0 {
		return nil, errors.New("no machine in the result")
	}
	return &r.Machines[0], nil
}

func describe(r *output.Run) string {
	var b strings.Builder
	if r.Error != "" {
		b.WriteString("run error: " + r.Error + "\n")
	}
	for _, m := range r.Machines {
		fmt.Fprintf(&b, "%s: %s %s %s\n", m.Name, m.Status, m.ErrorKind, m.Error)
		for _, t := range m.Tasks {
			if t.Status != "satisfied" && t.Status != "unchanged" {
				fmt.Fprintf(&b, "  %s %s %s %s: %s %s\n", t.Status, t.Group, t.Kind, t.Name, t.Detail, t.Error)
			}
		}
		for _, v := range m.Verify {
			if v.Status != "passed" {
				fmt.Fprintf(&b, "  verify %s %s [%s]: %s\n", v.Status, v.Name, v.Shell, v.Output)
			}
		}
	}
	return b.String()
}

// applyOK runs fam apply and requires success.
func (c *Ctx) applyOK(args ...string) (*output.Run, error) {
	r, err := c.famJSON(append([]string{"apply"}, args...)...)
	if err != nil {
		return nil, err
	}
	if !r.Res.OK || r.Code != 0 {
		return r.Res, fmt.Errorf("fam apply %s failed (exit %d):\n%s", strings.Join(args, " "), r.Code, describe(r.Res))
	}
	return r.Res, nil
}

func findTask(m *output.MachineResult, kind, nameContains string) *output.TaskResult {
	for i := range m.Tasks {
		if m.Tasks[i].Kind == kind && strings.Contains(m.Tasks[i].Name, nameContains) {
			return &m.Tasks[i]
		}
	}
	return nil
}

// stateScript prints a fingerprint of the files that FAM can change.
// stateScript prints a fingerprint of the files that FAM can change. On macOS it skips the
// app state in ~/Library (a logged-in GUI session writes there all the time) but keeps
// ~/Library/LaunchAgents.
const stateScript = `{ P='( -path "$HOME/.ssh" -o -path "$HOME/.cache" -o -path "$HOME/Library" -o -path "$HOME/Pictures" -o -path "$HOME/Movies" -o -path "$HOME/Music" -o -path "$HOME/.Trash" )'
eval "find \"\$HOME\" -xdev $P -prune -o -print" 2>/dev/null | LC_ALL=C sort
eval "find \"\$HOME\" -xdev $P -prune -o -type f -print" 2>/dev/null | LC_ALL=C sort | while read -r f; do cksum "$f"; done
ls -la "$HOME/Library/LaunchAgents" /etc/fam-acceptance /var/lib/systemd/linger 2>&1; } | cksum`

type scenario struct {
	n    int
	name string
	run  func(c *Ctx) error
	// skip returns a reason when the scenario does not apply to the target.
	skip func(t Target) string
}

var scenarios = []scenario{
	{1, "plan shows the changes and changes nothing", s1Plan, nil},
	{2, "apply, then plan is satisfied and apply changes nothing", s2Apply, nil},
	{3, "verify: fresh_shell and reconnect", s3Verify, nil},
	{4, "drift is found and repaired", s4Drift, nil},
	{5, "absent bundle removes skill, links, files, merged keys, variables", s5Absent, nil},
	{6, "global bundle removed on one machine", s6MachineAbsent, nil},
	{7, "secrets in shells, never in output or logs", s7Secrets, nil},
	{8, "check exit 1 never applies, failed task stops the rest", s8Failures, nil},
	{9, "network stop gives a connection error (255)", s9Network, nil},
	{10, "foreign skill folder makes the plan stop with a conflict", s10Conflict, nil},
	{11, "unknown template variable stops FAM", s11Template, nil},
	{12, "Claude Code and Codex: install, update, settings, paths", s12Agents, nil},
	{14, "standard bundle agentBrowser", s14Std, nil},
	{16, "all standard bundles: install, verify, satisfied, archive cache", s16StdAll, nil},
	{17, "package from the OS package manager, chosen in config.pkl, then removed", s17Packages, func(t Target) string {
		if t.BootScript != "" {
			return "macOS has no OS package manager in the base image"
		}
		return ""
	}},
	{15, "systemd tasks fail with a clear error without systemd", s15NoSystemd, func(t Target) string {
		if t.Systemd {
			return "the target has systemd"
		}
		if t.BootScript != "" {
			return "the target has launchd"
		}
		return ""
	}},
}

func s1Plan(c *Ctx) error {
	before, err := c.mustSSH(stateScript)
	if err != nil {
		return err
	}
	r, err := c.famJSON("plan")
	if err != nil {
		return err
	}
	m, err := machineOf(r.Res)
	if err != nil {
		return err
	}
	if r.Code != 0 || m.Status != "changes" || m.Counts.Error != 0 || m.Counts.Conflict != 0 {
		return fmt.Errorf("plan: exit %d, status %s\n%s", r.Code, m.Status, describe(r.Res))
	}
	for _, want := range [][2]string{{"file", "~/.local/bin/fam-tool"}, {"merge", "~/.cargo/config.toml"}, {"skill", "tool-skill"}, {"skill", "tool-notes"}, {"skill", "fleet-skill"}, {"file", "~/.agents/AGENTS.md"}, {"script", "system marker"}} {
		t := findTask(m, want[0], want[1])
		if t == nil || t.Status != "change" {
			return fmt.Errorf("plan: no change for %s %s\n%s", want[0], want[1], describe(r.Res))
		}
	}
	after, err := c.mustSSH(stateScript)
	if err != nil {
		return err
	}
	if before != after {
		return fmt.Errorf("plan changed the machine: fingerprint %s -> %s", strings.TrimSpace(before), strings.TrimSpace(after))
	}
	c.notes = append(c.notes, fmt.Sprintf("%d changes, %d satisfied", m.Counts.Change, m.Counts.Satisfied))
	return nil
}

func s2Apply(c *Ctx) error {
	r1, err := c.applyOK()
	if err != nil {
		return err
	}
	m1, _ := machineOf(r1)
	if m1.Counts.Changed == 0 || m1.Counts.VerifyBad != 0 {
		return fmt.Errorf("first apply: %d changed, %d verify failures\n%s", m1.Counts.Changed, m1.Counts.VerifyBad, describe(r1))
	}
	if c.t.Systemd {
		restarted := false
		for _, rs := range m1.Restarts {
			if rs.Unit == "fam-demo.service" && rs.Status == "restarted" {
				restarted = true
			}
		}
		if !restarted {
			return fmt.Errorf("first apply did not restart fam-demo.service: %+v", m1.Restarts)
		}
		if out, err := c.ssh("systemctl --user is-active fam-demo.service; systemctl --user is-enabled fam-demo.service; test -f /var/lib/systemd/linger/fam && echo linger"); err != nil || !strings.Contains(out, "active") || !strings.Contains(out, "enabled") || !strings.Contains(out, "linger") {
			return fmt.Errorf("service or linger not set: %v %s", err, out)
		}
	}
	if c.t.Name == "macos" {
		// The Service task started the job after the restart request of the env file, so
		// the job runs and FAM did not restart it a second time. The plist has no RunAtLoad
		// and no KeepAlive: FAM itself must start the job.
		for _, rs := range m1.Restarts {
			if rs.Unit == "dev.fam.demo" {
				return fmt.Errorf("first apply restarted dev.fam.demo after its Service task started it: %+v", m1.Restarts)
			}
		}
		if out, err := c.ssh(`launchctl print "gui/$(id -u)/dev.fam.demo" | grep -E "state = running"`); err != nil {
			return fmt.Errorf("dev.fam.demo is not running: %v %s", err, out)
		}
	}
	p, err := c.famJSON("plan")
	if err != nil {
		return err
	}
	pm, _ := machineOf(p.Res)
	for _, t := range pm.Tasks {
		if t.Status != "satisfied" {
			return fmt.Errorf("second plan is not only satisfied:\n%s", describe(p.Res))
		}
	}
	r2, err := c.applyOK()
	if err != nil {
		return err
	}
	m2, _ := machineOf(r2)
	if m2.Counts.Changed != 0 || len(m2.Restarts) != 0 {
		return fmt.Errorf("second apply changed %d tasks, %d restarts:\n%s", m2.Counts.Changed, len(m2.Restarts), describe(r2))
	}
	c.notes = append(c.notes, fmt.Sprintf("first apply %d changed, second plan %d satisfied, second apply 0 changed", m1.Counts.Changed, pm.Counts.Satisfied))
	return nil
}

func s3Verify(c *Ctx) error {
	if _, err := c.applyOK(); err != nil {
		return err
	}
	r, err := c.famJSON("verify")
	if err != nil {
		return err
	}
	m, _ := machineOf(r.Res)
	shells := map[string]bool{}
	for _, v := range m.Verify {
		if v.Status != "passed" {
			return fmt.Errorf("verify failed:\n%s", describe(r.Res))
		}
		shells[v.Event+"/"+v.Shell] = true
	}
	for _, want := range []string{"fresh_shell/noninteractive", "fresh_shell/interactive", "reconnect/new connection"} {
		if !shells[want] {
			return fmt.Errorf("verify did not run %s: %v", want, shells)
		}
	}
	if r.Code != 0 {
		return fmt.Errorf("verify exit %d", r.Code)
	}
	c.notes = append(c.notes, fmt.Sprintf("%d checks passed", m.Counts.VerifyOK))
	return nil
}

func s4Drift(c *Ctx) error {
	if _, err := c.applyOK(); err != nil {
		return err
	}
	if _, err := c.mustSSH(`set -e
echo drift >> ~/.local/bin/fam-tool
sed -i.bak 's/^jobs = .*/jobs = 99/' ~/.cargo/config.toml && rm -f ~/.cargo/config.toml.bak
rm -f ~/.agents/skills/tool-skill/scripts/run.sh
echo "hand edit" >> ~/.agents/AGENTS.md
chmod 0600 ~/.local/lib/fleet/gate`); err != nil {
		return err
	}
	p, err := c.famJSON("plan")
	if err != nil {
		return err
	}
	pm, _ := machineOf(p.Res)
	for _, want := range [][2]string{{"file", "~/.local/bin/fam-tool"}, {"merge", "~/.cargo/config.toml"}, {"skill", "tool-skill"}, {"file", "~/.agents/AGENTS.md"}, {"file", "~/.local/lib/fleet/gate"}} {
		t := findTask(pm, want[0], want[1])
		if t == nil || t.Status != "change" {
			return fmt.Errorf("plan did not find the drift of %s %s:\n%s", want[0], want[1], describe(p.Res))
		}
	}
	if _, err := c.applyOK(); err != nil {
		return err
	}
	p2, err := c.famJSON("plan")
	if err != nil {
		return err
	}
	pm2, _ := machineOf(p2.Res)
	if pm2.Counts.Change != 0 || pm2.Counts.Error != 0 {
		return fmt.Errorf("plan after repair:\n%s", describe(p2.Res))
	}
	out, err := c.ssh(`ls ~/.agents/ | grep -c 'AGENTS.md.fam-backup-'; grep -c 'jobs = 99' ~/.cargo/config.toml; grep -c drift ~/.local/bin/fam-tool`)
	if err != nil && out == "" {
		return err
	}
	f := strings.Fields(out)
	if len(f) < 3 || f[0] == "0" || f[1] != "0" || f[2] != "0" {
		return fmt.Errorf("repair is not complete (backups, jobs=99, drift): %q", out)
	}
	c.notes = append(c.notes, "drift in 5 tasks found and repaired, copy of the hand-edited AGENTS.md saved")
	return nil
}

func s5Absent(c *Ctx) error {
	if _, err := c.applyOK(); err != nil {
		return err
	}
	if _, err := c.applyOK("--config", "config-absent.pkl"); err != nil {
		return err
	}
	out, _ := c.ssh(`for p in ~/.local/bin/fam-tool ~/.local/bin/fam-tool-link ~/.agents/skills/tool-skill ~/.claude/skills/tool-skill ~/.agents/skills/tool-notes ~/.claude/skills/tool-notes; do if [ -e "$p" ] || [ -L "$p" ]; then echo "present $p"; fi; done
echo "vars=$(grep -c FAM_TOOL_MODE ~/.config/fam/env.sh)"
cat ~/.config/fam-tool/config.json`)
	if strings.Contains(out, "present ") {
		return fmt.Errorf("absent bundle left items: %s", out)
	}
	if strings.Contains(out, `"mode"`) || strings.Contains(out, `"nested"`) {
		return fmt.Errorf("merged keys of the absent bundle stayed: %s", out)
	}
	if !strings.Contains(out, "vars=0") {
		return fmt.Errorf("bundle variables stayed in env.sh: %s", out)
	}
	if out2, _ := c.ssh(`test -d ~/.agents/skills/fleet-skill && echo kept`); !strings.Contains(out2, "kept") {
		return fmt.Errorf("the project skill was removed")
	}
	p, err := c.famJSON("plan", "--config", "config-absent.pkl")
	if err != nil {
		return err
	}
	pm, _ := machineOf(p.Res)
	if pm.Counts.Change != 0 {
		return fmt.Errorf("plan after removal:\n%s", describe(p.Res))
	}
	c.notes = append(c.notes, "file, link, merged keys, skill, Claude link, variables removed")
	return nil
}

func s6MachineAbsent(c *Ctx) error {
	if _, err := c.applyOK(); err != nil {
		return err
	}
	if out, _ := c.ssh("test -f ~/.local/share/fam-global/marker && echo there"); !strings.Contains(out, "there") {
		return errors.New("the global bundle was not applied")
	}
	if _, err := c.applyOK("--config", "config-machine-absent.pkl"); err != nil {
		return err
	}
	out, _ := c.ssh(`test -e ~/.local/share/fam-global/marker && echo still; test -x ~/.local/bin/fam-tool && echo tool`)
	if strings.Contains(out, "still") || !strings.Contains(out, "tool") {
		return fmt.Errorf("machine entry did not remove only the global bundle: %s", out)
	}
	c.notes = append(c.notes, "global bundle removed by the machine entry, other bundles kept")
	return nil
}

func s7Secrets(c *Ctx) error {
	before := listRuns(c.logDir)
	plan := c.famCmd("plan")
	planJ := c.famCmd("plan", "--json")
	planT := c.famCmd("plan", "--toon")
	if _, err := c.applyOK(); err != nil {
		return err
	}
	c.famCmd("apply", "--toon")
	c.famCmd("verify")
	if plan.Code != 0 || planJ.Code != 0 || planT.Code != 0 {
		return fmt.Errorf("plan failed: %d %d %d", plan.Code, planJ.Code, planT.Code)
	}
	if !strings.Contains(plan.Stdout, "FAM_TEST_TOKEN") {
		return fmt.Errorf("the plan does not name the secrets:\n%s", plan.Stdout)
	}
	tok, quoted := testSecrets["FAM_TEST_TOKEN"], testSecrets["FAM_TEST_QUOTED"]
	shells := map[string]string{
		"ssh (noninteractive bash)": `printf '%s|%s\n' "$FAM_TEST_TOKEN" "$FAM_TEST_QUOTED"`,
		"bash -ic":                  `bash -ic 'printf "%s|%s\n" "$FAM_TEST_TOKEN" "$FAM_TEST_QUOTED"' 2>/dev/null`,
		"bash -lc":                  `bash -lc 'printf "%s|%s\n" "$FAM_TEST_TOKEN" "$FAM_TEST_QUOTED"'`,
		"zsh -c":                    `zsh -c 'printf "%s|%s\n" "$FAM_TEST_TOKEN" "$FAM_TEST_QUOTED"'`,
		"zsh -ic":                   `zsh -ic 'printf "%s|%s\n" "$FAM_TEST_TOKEN" "$FAM_TEST_QUOTED"' 2>/dev/null`,
		"sh -lc":                    `env -i HOME="$HOME" sh -lc 'printf "%s|%s\n" "$FAM_TEST_TOKEN" "$FAM_TEST_QUOTED"'`,
	}
	names := make([]string, 0, len(shells))
	for n := range shells {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out, err := c.ssh(shells[n])
		if err != nil || !strings.Contains(out, tok+"|"+quoted) {
			return fmt.Errorf("%s does not export the secrets: %v %q", n, err, out)
		}
	}
	if out, _ := c.ssh(`stat -c %a ~/.config/fam/secrets.env 2>/dev/null || stat -f %Lp ~/.config/fam/secrets.env`); strings.TrimSpace(out) != "600" {
		return fmt.Errorf("secrets file mode on the machine: %q", out)
	}
	// Search all output and the new run logs.
	texts := append([]string{}, c.outputs...)
	newLogs := 0
	for f := range listRuns(c.logDir) {
		if before[f] {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		texts = append(texts, string(b))
		newLogs++
	}
	for _, s := range texts {
		for name, v := range testSecrets {
			if strings.Contains(s, v) {
				return fmt.Errorf("the value of %s appears in output or a run log", name)
			}
		}
	}
	if newLogs < 5 {
		return fmt.Errorf("expected at least 5 new run logs, found %d", newLogs)
	}
	c.notes = append(c.notes, fmt.Sprintf("%d shells export both secrets; values absent from %d outputs and %d run logs", len(shells), len(c.outputs), newLogs))
	return nil
}

func listRuns(logDir string) map[string]bool {
	out := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(logDir, "fam", "runs", "*.jsonl"))
	for _, f := range files {
		out[f] = true
	}
	return out
}

func s8Failures(c *Ctx) error {
	r, err := c.famJSON("apply", "--config", "config-fail.pkl")
	if err != nil {
		return err
	}
	m, _ := machineOf(r.Res)
	t := findTask(m, "script", "check returns 1")
	if r.Code == 0 || m.Status != "failed" || t == nil || t.Status != "failed" || !strings.Contains(t.Error, "exit code 1") {
		return fmt.Errorf("a check with exit code 1 did not fail the task (exit %d):\n%s", r.Code, describe(r.Res))
	}
	if out, _ := c.ssh("test -e ~/fam-check-error-applied && echo applied"); strings.Contains(out, "applied") {
		return errors.New("FAM ran apply after a check error")
	}
	r2, err := c.famJSON("apply", "--config", "config-fail2.pkl")
	if err != nil {
		return err
	}
	m2, _ := machineOf(r2.Res)
	tf := findTask(m2, "script", "apply fails")
	after := findTask(m2, "file", "~/fam-after-failure")
	if tf == nil || tf.Status != "failed" || after == nil || after.Status != "skipped" {
		return fmt.Errorf("a failed task did not stop the tasks after it:\n%s", describe(r2.Res))
	}
	if out, _ := c.ssh("test -e ~/fam-after-failure && echo created"); strings.Contains(out, "created") {
		return errors.New("FAM ran a task after a failed task")
	}
	c.notes = append(c.notes, "check exit 1: no apply; apply exit 7: next task skipped")
	return nil
}

func s9Network(c *Ctx) error {
	if _, err := c.applyOK(); err != nil {
		return err
	}
	type result struct {
		r   famRun
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := c.famJSON("apply", "--config", "config-slow.pkl")
		done <- result{r, err}
	}()
	// Wait until the slow script runs on the machine.
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, _ := c.ssh("pgrep -f 'sleep 60' >/dev/null && echo running")
		if strings.Contains(out, "running") {
			break
		}
		if time.Now().After(deadline) {
			return errors.New("the slow script did not start")
		}
		time.Sleep(time.Second)
	}
	if err := c.vm.SetLink(false); err != nil {
		return err
	}
	var res result
	select {
	case res = <-done:
	case <-time.After(4 * time.Minute):
		_ = c.vm.SetLink(true)
		return errors.New("fam did not stop after the network stopped")
	}
	_ = c.vm.SetLink(true)
	if res.err != nil {
		return res.err
	}
	m, _ := machineOf(res.r.Res)
	if m.ErrorKind != "connection" || !strings.Contains(m.Error, "255") || res.r.Code == 0 {
		return fmt.Errorf("no connection error (exit %d):\n%s", res.r.Code, describe(res.r.Res))
	}
	c.notes = append(c.notes, "error: "+firstLineS(m.Error))
	return nil
}

func firstLineS(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func s10Conflict(c *Ctx) error {
	if _, err := c.mustSSH("mkdir -p ~/.agents/skills/tool-skill && echo 'foreign skill' > ~/.agents/skills/tool-skill/SKILL.md"); err != nil {
		return err
	}
	p, err := c.famJSON("plan")
	if err != nil {
		return err
	}
	m, _ := machineOf(p.Res)
	t := findTask(m, "skill", "tool-skill")
	if p.Code == 0 || m.Status != "conflict" || t == nil || t.Status != "conflict" {
		return fmt.Errorf("plan did not stop with a conflict (exit %d):\n%s", p.Code, describe(p.Res))
	}
	a, err := c.famJSON("apply")
	if err != nil {
		return err
	}
	am, _ := machineOf(a.Res)
	if a.Code == 0 || am.Status != "conflict" {
		return fmt.Errorf("apply did not stop with a conflict:\n%s", describe(a.Res))
	}
	if out, _ := c.ssh("test -e ~/.local/bin/fam-tool && echo changed; cat ~/.agents/skills/tool-skill/SKILL.md"); strings.Contains(out, "changed") || !strings.Contains(out, "foreign skill") {
		return fmt.Errorf("apply changed the machine after a conflict: %s", out)
	}
	c.notes = append(c.notes, "conflict: "+firstLineS(t.Error))
	return nil
}

func s11Template(c *Ctx) error {
	r, err := c.famJSON("plan", "--instructions", "instructions-bad")
	if err != nil {
		return err
	}
	m, _ := machineOf(r.Res)
	if r.Code == 0 || !strings.Contains(m.Error, `"Agent"`) {
		return fmt.Errorf("an unknown template variable did not stop FAM (exit %d):\n%s", r.Code, describe(r.Res))
	}
	c.notes = append(c.notes, "error: "+firstLineS(m.Error))
	return nil
}

func s12Agents(c *Ctx) error {
	if _, err := c.mustSSH(`mkdir -p ~/.claude ~/.codex && printf '{\n  "theme": "dark"\n}\n' > ~/.claude/settings.json && printf 'model = "gpt-test"\n' > ~/.codex/config.toml`); err != nil {
		return err
	}
	r1, err := c.applyOK("--config", "config-agents-old.pkl")
	if err != nil {
		return err
	}
	m1, _ := machineOf(r1)
	v1, _ := c.ssh(`claude --version; codex --version`)
	if !strings.Contains(v1, "2.1.311") || !strings.Contains(v1, "0.168.0") {
		return fmt.Errorf("pinned versions not installed: %s\n%s", v1, describe(r1))
	}
	r2, err := c.applyOK("--config", "config-agents.pkl")
	if err != nil {
		return err
	}
	m2, _ := machineOf(r2)
	for _, a := range []string{"claude", "codex"} {
		t := findTask(m2, "package", a)
		if t == nil || t.Status != "changed" || !strings.Contains(t.Detail, "updated") {
			return fmt.Errorf("%s was not updated:\n%s", a, describe(r2))
		}
	}
	out, err := c.mustSSH(`cat ~/.claude/settings.json; echo ----; cat ~/.codex/config.toml; echo ----
test -r ~/.claude/skills/tool-skill/SKILL.md && echo claude-skill-tool
test -r ~/.claude/skills/fleet-skill/SKILL.md && echo claude-skill-fleet
test -r ~/.agents/skills/tool-skill/SKILL.md && echo codex-skill-tool
grep -c 'Claude-only acceptance line' ~/.claude/CLAUDE.md ~/.codex/AGENTS.md
grep -c '# Machine' ~/.claude/CLAUDE.md ~/.codex/AGENTS.md
readlink ~/.claude/CLAUDE.md ~/.codex/AGENTS.md`)
	if err != nil {
		return err
	}
	for _, want := range []string{`"theme": "dark"`, `"autoMemoryEnabled": false`, `"FAM_ACCEPTANCE": "1"`, `model = 'gpt-test'`, `check_for_update_on_startup = false`, `notifications = true`,
		"claude-skill-tool", "claude-skill-fleet", "codex-skill-tool", "CLAUDE.md:1", "codex/AGENTS.md:0", "/.agents/CLAUDE.md", "/.agents/AGENTS.md"} {
		if !strings.Contains(out, want) {
			return fmt.Errorf("agent state is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "CLAUDE.md:1\n") || !strings.Contains(out, "AGENTS.md:1") {
		return fmt.Errorf("instructions are not at the agent paths:\n%s", out)
	}
	r3, err := c.applyOK("--config", "config-agents.pkl")
	if err != nil {
		return err
	}
	m3, _ := machineOf(r3)
	if m3.Counts.Changed != 0 {
		return fmt.Errorf("third apply changed tasks:\n%s", describe(r3))
	}
	vs, _ := c.ssh(`claude --version; codex --version`)
	c.notes = append(c.notes, fmt.Sprintf("pinned %d changed; update: %s; now %s", m1.Counts.Changed, strings.Join(updDetails(m2), "; "), strings.ReplaceAll(strings.TrimSpace(vs), "\n", ", ")))
	return nil
}

func updDetails(m *output.MachineResult) []string {
	var out []string
	for _, t := range m.Tasks {
		if t.Kind == "package" && strings.HasPrefix(t.Group, "agent ") {
			out = append(out, t.Name+" "+t.Detail)
		}
	}
	return out
}

func s14Std(c *Ctx) error {
	r, err := c.applyOK("--config", "config-std.pkl")
	if err != nil {
		return err
	}
	m, _ := machineOf(r)
	out, err := c.mustSSH(`bash -lc 'agent-browser --version'; grep -q '^name: agent-browser' ~/.agents/skills/agent-browser/SKILL.md && echo skill; test -L ~/.claude/skills/agent-browser && echo link`)
	if err != nil || !strings.Contains(out, "skill") || !strings.Contains(out, "link") {
		return fmt.Errorf("agentBrowser: %v %s", err, out)
	}
	// The bundle installs only the CLI and its skill (with the Claude link): no browser, no
	// system packages, no variables.
	for _, t := range m.Tasks {
		if t.Group == "bundle agent-browser" && t.Kind != "package" && t.Kind != "skill" && !(t.Kind == "symlink" && strings.Contains(t.Name, ".claude/skills/agent-browser")) {
			return fmt.Errorf("agentBrowser has a task other than the CLI and its skill: %s %s", t.Kind, t.Name)
		}
	}
	if extra, _ := c.ssh(`test -d ~/.agent-browser/browsers && echo browser; bash -lc 'env' | grep '^AGENT_BROWSER_'`); strings.TrimSpace(extra) != "" {
		return fmt.Errorf("agentBrowser changed more than the CLI: %s", extra)
	}
	c.notes = append(c.notes, fmt.Sprintf("%d changed; %s", m.Counts.Changed, firstLineS(out)))
	return nil
}

// stdSkills are the skills of all standard bundles, apart from the skills of agent-desktop and
// lat.md, which depend on the target.
var stdSkills = []string{
	"agent-browser",
	"vercel-composition-patterns", "deploy-to-vercel", "vercel-react-best-practices", "vercel-react-native-skills",
	"vercel-react-view-transitions", "vercel-cli-with-tokens", "vercel-optimize", "web-design-guidelines", "writing-guidelines",
	"ai-cli", "d3k", "emulate",
	"hyperframes", "hyperframes-cli", "hyperframes-registry", "media-use", "reverse-any-video", "style-sampling",
	"issue-graph", "opensrc", "portless", "find-skills", "webreel", "zero",
}

func s16StdAll(c *Ctx) error {
	start := time.Now()
	r, err := c.applyOK("--config", "config-std-all.pkl")
	if err != nil {
		return err
	}
	applied := time.Since(start)
	m, err := machineOf(r)
	if err != nil {
		return err
	}
	want := stdSkills
	switch {
	case c.t.BootScript != "":
		want = append(want, "agent-desktop", "agent-desktop-macos", "lat-md")
	case !strings.Contains(c.t.Name, "alpine"):
		want = append(want, "agent-desktop", "agent-desktop-linux", "lat-md")
	}
	var missing []string
	for _, s := range want {
		if out, _ := c.ssh("test -f ~/.agents/skills/" + s + "/SKILL.md && test -L ~/.claude/skills/" + s + " && echo ok"); !strings.Contains(out, "ok") {
			missing = append(missing, s)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("skills missing: %s", strings.Join(missing, ", "))
	}
	// The second plan reads the archives from the cache of the cockpit.
	start = time.Now()
	p, err := c.famJSON("plan", "--config", "config-std-all.pkl")
	if err != nil {
		return err
	}
	planned := time.Since(start)
	pm, err := machineOf(p.Res)
	if err != nil || p.Code != 0 || !p.Res.OK || pm.Counts.Change != 0 || pm.Counts.Error != 0 || pm.Counts.Conflict != 0 {
		return fmt.Errorf("plan after the apply:\n%s", describe(p.Res))
	}
	c.notes = append(c.notes, fmt.Sprintf("%d changed, %d skills; apply %s, plan %s", m.Counts.Changed, len(want), applied.Round(time.Second), planned.Round(time.Second)))
	return nil
}

func s17Packages(c *Ctx) error {
	r, err := c.applyOK("--config", "config-pkg.pkl")
	if err != nil {
		return err
	}
	m, _ := machineOf(r)
	t := findTask(m, "package", "tree (")
	if t == nil || t.Status == "failed" || strings.Contains(t.Name, "(npm)") {
		return fmt.Errorf("the tree package did not use the OS package manager:\n%s", describe(r))
	}
	if out, _ := c.ssh("command -v tree"); !strings.Contains(out, "tree") {
		return fmt.Errorf("tree is not installed: %q", out)
	}
	p, err := c.famJSON("plan", "--config", "config-pkg.pkl")
	if err != nil {
		return err
	}
	pm, _ := machineOf(p.Res)
	if pm.Counts.Change != 0 || pm.Counts.Error != 0 {
		return fmt.Errorf("plan after the install:\n%s", describe(p.Res))
	}
	if _, err := c.applyOK("--config", "config-pkg-absent.pkl"); err != nil {
		return err
	}
	// Ask the package manager through FAM: another program can also be called tree
	// (BusyBox on Alpine has a tree applet).
	p2, err := c.famJSON("plan", "--config", "config-pkg.pkl")
	if err != nil {
		return err
	}
	pm2, _ := machineOf(p2.Res)
	if t2 := findTask(pm2, "package", "tree ("); t2 == nil || t2.Status != "change" {
		return fmt.Errorf("the package manager still has tree after state absent:\n%s", describe(p2.Res))
	}
	c.notes = append(c.notes, t.Name+": "+t.Status+", then removed")
	return nil
}

func s15NoSystemd(c *Ctx) error {
	r, err := c.famJSON("apply", "--config", "config-systemd.pkl")
	if err != nil {
		return err
	}
	m, _ := machineOf(r.Res)
	t := findTask(m, "service", "fam-demo")
	if r.Code == 0 || t == nil || t.Status != "failed" || !strings.Contains(t.Error, "needs systemd") {
		return fmt.Errorf("no clear systemd error:\n%s", describe(r.Res))
	}
	c.notes = append(c.notes, "error: "+firstLineS(t.Error))
	return nil
}

// --- runner ---

// ScenarioResult is the result of one scenario on one target.
type ScenarioResult struct {
	Target   string `json:"target"`
	N        int    `json:"n"`
	Name     string `json:"name"`
	Result   string `json:"result"` // pass, fail, skip
	Detail   string `json:"detail"`
	Duration string `json:"duration"`
}

func buildFam(ctx context.Context) (string, error) {
	out := filepath.Join(os.TempDir(), "fam-acceptance-bin")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/fam")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %v: %s", err, b)
	}
	return out, nil
}

func parseScenarios(s string) map[int]bool {
	if s == "" {
		return nil
	}
	out := map[int]bool{}
	for _, f := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			out[n] = true
		}
	}
	return out
}

func runMatrix(ctx context.Context, w *Work, names []string, scen, fam string) error {
	if _, err := os.Stat(filepath.Join("cmd", "fam", "main.go")); err != nil {
		return errors.New("run the harness from the root of the FAM repository")
	}
	if fam == "" {
		var err error
		if fam, err = buildFam(ctx); err != nil {
			return err
		}
	}
	sel := parseScenarios(scen)
	resultsDir := filepath.Join("acceptance", "results")
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		return err
	}
	secretsFile := filepath.Join(w.Dir, "secrets", "secrets.env")
	if err := writeSecrets(secretsFile); err != nil {
		return err
	}
	if len(names) == 2 && sel[13] {
		return runPair(ctx, w, fam, names, secretsFile, resultsDir)
	}
	var all []ScenarioResult
	for _, name := range names {
		t, err := findTarget(name)
		if err != nil {
			return err
		}
		for _, s := range scenarios {
			if sel != nil && !sel[s.n] {
				continue
			}
			sr := ScenarioResult{Target: t.Name, N: s.n, Name: s.name}
			if s.skip != nil {
				if reason := s.skip(t); reason != "" {
					sr.Result, sr.Detail = "skip", reason
					all = append(all, sr)
					fmt.Printf("%-14s %2d SKIP %s (%s)\n", t.Name, s.n, s.name, reason)
					continue
				}
			}
			start := time.Now()
			notes, err := runOne(ctx, w, fam, t, s, secretsFile)
			sr.Duration = time.Since(start).Round(time.Second).String()
			if err != nil {
				sr.Result, sr.Detail = "fail", err.Error()
			} else {
				sr.Result, sr.Detail = "pass", strings.Join(notes, "; ")
			}
			all = append(all, sr)
			fmt.Printf("%-14s %2d %s %s (%s): %s\n", t.Name, s.n, strings.ToUpper(sr.Result), s.name, sr.Duration, firstLineS(sr.Detail))
			if sr.Result == "fail" {
				fmt.Println(indent(sr.Detail))
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if err := saveResults(resultsDir, t.Name, all); err != nil {
			return err
		}
	}
	failed := 0
	for _, r := range all {
		if r.Result == "fail" {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d scenarios failed", failed)
	}
	return nil
}

func indent(s string) string { return "    " + strings.ReplaceAll(s, "\n", "\n    ") }

func saveResults(dir, target string, all []ScenarioResult) error {
	var mine []ScenarioResult
	for _, r := range all {
		if r.Target == target {
			mine = append(mine, r)
		}
	}
	p := filepath.Join(dir, target+".json")
	// Merge with earlier results of other scenarios.
	var old []ScenarioResult
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &old)
	}
	byN := map[int]ScenarioResult{}
	for _, r := range old {
		byN[r.N] = r
	}
	for _, r := range mine {
		byN[r.N] = r
	}
	var merged []ScenarioResult
	for _, r := range byN {
		merged = append(merged, r)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].N < merged[j].N })
	b, _ := json.MarshalIndent(merged, "", "  ")
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

func newScenarioDir(w *Work, fam, label string, machines []machineEntry, secretsFile string) (string, string, error) {
	dir := filepath.Join(w.Dir, "run", "projects", label)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if out, err := exec.Command(fam, "new", "--base", dir).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("fam new: %v: %s", err, out)
	}
	// Replace the example files of fam new with the test project.
	for _, f := range []string{"bundles/hello.pkl", "bundles/files/fam-hello.sh"} {
		_ = os.Remove(filepath.Join(dir, f))
	}
	if err := writeProject(dir, secretsFile, machines); err != nil {
		return "", "", err
	}
	logDir := filepath.Join(w.Dir, "run", "state", label)
	_ = os.RemoveAll(logDir)
	return dir, logDir, os.MkdirAll(logDir, 0o755)
}

func runOne(ctx context.Context, w *Work, fam string, t Target, s scenario, secretsFile string) (notes []string, err error) {
	vm, err := w.Start(ctx, t)
	if err != nil {
		return nil, err
	}
	defer vm.Kill()
	dir, logDir, err := newScenarioDir(w, fam, fmt.Sprintf("%s-s%d", t.Name, s.n), []machineEntry{{Name: "vm", Target: t}}, secretsFile)
	if err != nil {
		return nil, err
	}
	c := &Ctx{ctx: ctx, w: w, fam: fam, t: t, vm: vm, dir: dir, logDir: logDir}
	err = s.run(c)
	return c.notes, err
}

// runPair runs scenario 13: two VMs in parallel, then --serial.
func runPair(ctx context.Context, w *Work, fam string, names []string, secretsFile, resultsDir string) error {
	var ts []Target
	var vms []*VM
	for _, n := range names {
		t, err := findTarget(n)
		if err != nil {
			return err
		}
		ts = append(ts, t)
	}
	for _, t := range ts {
		vm, err := w.Start(ctx, t)
		if err != nil {
			return err
		}
		defer vm.Kill()
		vms = append(vms, vm)
	}
	machines := []machineEntry{{Name: "a-" + ts[0].Name, Target: ts[0]}, {Name: "b-" + ts[1].Name, Target: ts[1]}}
	dir, logDir, err := newScenarioDir(w, fam, "pair", machines, secretsFile)
	if err != nil {
		return err
	}
	c := &Ctx{ctx: ctx, w: w, fam: fam, t: ts[0], vm: vms[0], vms: vms, dir: dir, logDir: logDir}
	start := time.Now()
	sr := ScenarioResult{Target: "pair", N: 13, Name: "two VMs in parallel, then --serial (" + ts[0].Name + " + " + ts[1].Name + ")"}
	err = func() error {
		t0 := time.Now()
		p, err := c.applyOK()
		if err != nil {
			return err
		}
		par := time.Since(t0)
		if len(p.Machines) != 2 {
			return fmt.Errorf("parallel apply has %d machines", len(p.Machines))
		}
		t1 := time.Now()
		s, err := c.famJSON("plan", "--serial")
		if err != nil {
			return err
		}
		ser := time.Since(t1)
		for _, m := range s.Res.Machines {
			if m.Counts.Change != 0 || m.Status != "ok" {
				return fmt.Errorf("serial plan after parallel apply:\n%s", describe(s.Res))
			}
		}
		t2 := time.Now()
		a2, err := c.famJSON("apply", "--serial")
		if err != nil {
			return err
		}
		ser2 := time.Since(t2)
		if !a2.Res.OK {
			return fmt.Errorf("serial apply:\n%s", describe(a2.Res))
		}
		text := c.famCmd("plan")
		ia := strings.Index(text.Stdout, "== a-")
		ib := strings.Index(text.Stdout, "== b-")
		if ia < 0 || ib < 0 || ia > ib {
			return fmt.Errorf("text output is not grouped by machine:\n%s", text.Stdout)
		}
		c.notes = append(c.notes, fmt.Sprintf("parallel apply %s; serial plan %s; serial apply %s; output grouped by machine", par.Round(time.Second), ser.Round(time.Second), ser2.Round(time.Second)))
		return nil
	}()
	sr.Duration = time.Since(start).Round(time.Second).String()
	if err != nil {
		sr.Result, sr.Detail = "fail", err.Error()
	} else {
		sr.Result, sr.Detail = "pass", strings.Join(c.notes, "; ")
	}
	fmt.Printf("%s 13 %s (%s): %s\n", sr.Target, strings.ToUpper(sr.Result), sr.Duration, sr.Detail)
	if err := saveResults(resultsDir, "pair", []ScenarioResult{sr}); err != nil {
		return err
	}
	if sr.Result == "fail" {
		return errors.New("scenario 13 failed")
	}
	return nil
}
