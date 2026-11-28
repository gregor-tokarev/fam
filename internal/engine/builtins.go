package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/gregor-tokarev/fam/internal/config"
	"github.com/gregor-tokarev/fam/internal/secrets"
	"github.com/gregor-tokarev/fam/internal/transport"
)

const (
	blockBegin = "# >>> fam >>>"
	blockEnd   = "# <<< fam <<<"
)

// --- environment and secrets ---

// envPathLine is one PATH entry in ~/.config/fam/env.sh.
func envPathLine(d string) string {
	if strings.HasPrefix(d, "~/") {
		d = "$HOME/" + d[2:]
	}
	d = strings.ReplaceAll(d, `"`, `\"`)
	return fmt.Sprintf("case \":$PATH:\" in *\":%[1]s:\"*) ;; *) PATH=\"%[1]s:$PATH\" ;; esac\n", d)
}

// envScript returns ~/.config/fam/env.sh: bundle variables, ~/.local/bin in PATH, and the secrets.
// A skipped bundle adds no variables or PATH entries.
func envScript(spec *config.Spec) []byte {
	var b strings.Builder
	b.WriteString("# Written by FAM. Do not edit. FAM replaces this file on each apply.\n")
	b.WriteString("# Shells load it from a marked block in ~/.zshenv, ~/.bashrc, and ~/.profile.\n")
	dirs := []string{"~/.local/bin"}
	seenDirs := map[string]bool{"~/.local/bin": true}
	for _, u := range spec.Bundles {
		if u.State != "present" || u.Skipped() {
			continue
		}
		for _, d := range u.Bundle.Path {
			if !seenDirs[d] {
				dirs = append(dirs, d)
				seenDirs[d] = true
			}
		}
	}
	// FAM prepends in reverse order, so the first directory is first in PATH.
	for i := len(dirs) - 1; i >= 0; i-- {
		b.WriteString(envPathLine(dirs[i]))
	}
	b.WriteString("export PATH\n")
	for _, u := range spec.Bundles {
		if u.State != "present" || u.Skipped() {
			continue
		}
		keys := make([]string, 0, len(u.Bundle.Env))
		for k := range u.Bundle.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 0 {
			fmt.Fprintf(&b, "# bundle %s\n", u.Bundle.Name)
		}
		for _, k := range keys {
			fmt.Fprintf(&b, "export %s=%s\n", k, transport.Quote(u.Bundle.Env[k]))
		}
	}
	b.WriteString("[ -r \"$HOME/" + secretsFile + "\" ] && . \"$HOME/" + secretsFile + "\"\n")
	return []byte(b.String())
}

func loaderBlock() string {
	return blockBegin + "\n" +
		"# Loads the environment of FAM (bundle variables and secrets). FAM manages this block.\n" +
		"[ -r \"$HOME/" + envFile + "\" ] && . \"$HOME/" + envFile + "\"\n" +
		blockEnd + "\n"
}

// BlockTask keeps a marked block in a shell startup file.
type BlockTask struct {
	base
	Path  string
	Block string
	// Top puts the block at the start of the file, before any line that stops noninteractive shells.
	Top bool
	// OnlyIfExists leaves a missing file missing.
	OnlyIfExists bool
	// Mode is the mode of a new file. The default is 0644.
	Mode os.FileMode
}

// Kind implements Task.
func (t *BlockTask) Kind() string { return "block" }

// Name implements Task.
func (t *BlockTask) Name() string { return t.Path }

func (t *BlockTask) want(cur []byte) []byte {
	s := string(cur)
	// Remove every old block.
	for {
		i := strings.Index(s, blockBegin)
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], blockEnd)
		if j < 0 {
			break
		}
		end := i + j + len(blockEnd)
		if end < len(s) && s[end] == '\n' {
			end++
		}
		s = s[:i] + s[end:]
	}
	if t.Top {
		return []byte(t.Block + s)
	}
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return []byte(s + t.Block)
}

func (t *BlockTask) specReqs(env *Env) []transport.SpecReq {
	return []transport.SpecReq{specRead(false, env.Path(t.Path))}
}

// Check implements Task.
func (t *BlockTask) Check(ctx context.Context, env *Env) (Check, error) {
	p := env.Path(t.Path)
	cur, fi, err := env.T.ReadFileInfo(ctx, p, false)
	if err != nil {
		return Check{}, err
	}
	mode := os.FileMode(0o644)
	if t.Mode != 0 {
		mode = t.Mode
	}
	if fi.Exists && fi.Type == "file" {
		mode = fi.Mode.Perm()
	}
	if !fi.Exists {
		if t.OnlyIfExists {
			return Check{Status: Satisfied, Detail: "no file"}, nil
		}
		want := t.want(nil)
		return Check{
			Status: Change,
			Detail: "create with the FAM block",
			Plan:   writePrimPlan(p, want, mode, false, false, true, "", ""),
		}, nil
	}
	want := t.want(cur)
	if bytes.Equal(cur, want) {
		return Check{Status: Satisfied}, nil
	}
	detail := "add the FAM block"
	if bytes.Contains(cur, []byte(blockBegin)) {
		detail = "update the FAM block"
	}
	var plan *PrimPlan
	if fi.Type == "file" {
		plan = writePrimPlan(p, want, mode, false, false, false, fi.SHA256, "")
	}
	return Check{Status: Change, Detail: detail, Plan: plan}, nil
}

// Apply implements Task.
func (t *BlockTask) Apply(ctx context.Context, env *Env) (string, error) {
	p := env.Path(t.Path)
	cur, fi, err := env.T.ReadFileInfo(ctx, p, false)
	if err != nil {
		return "", err
	}
	mode := os.FileMode(0o644)
	if t.Mode != 0 {
		mode = t.Mode
	}
	if fi.Exists && fi.Type == "file" {
		mode = fi.Mode.Perm()
	}
	return "written", env.T.WriteFile(ctx, p, t.want(cur), mode, false)
}

const sshdDropIn = "/etc/ssh/sshd_config.d/50-fam-bash-env.conf"

const sshdDropInContent = `# Written by FAM. Lets ~/.ssh/environment set BASH_ENV, so that noninteractive
# SSH commands in Bash load the FAM environment. Bash on this machine does not
# read ~/.bashrc for SSH commands.
PermitUserEnvironment BASH_ENV
`

// allowsBashEnv reads the `permituserenvironment` line of `sshd -T`: "yes", "no", or an
// OpenSSH pattern list. A negated pattern (!pattern) that matches denies the variable.
func allowsBashEnv(sshdT string) bool {
	for _, line := range strings.Split(sshdT, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || !strings.EqualFold(f[0], "permituserenvironment") {
			continue
		}
		switch strings.ToLower(f[1]) {
		case "yes":
			return true
		case "no":
			return false
		}
		allowed := false
		for _, pat := range strings.Split(f[1], ",") {
			neg := strings.HasPrefix(pat, "!")
			pat = strings.TrimPrefix(pat, "!")
			if ok, _ := path.Match(pat, "BASH_ENV"); ok {
				if neg {
					return false
				}
				allowed = true
			}
		}
		return allowed
	}
	return false
}

// SshdEnvTask allows BASH_ENV in ~/.ssh/environment. FAM uses it only on machines where
// the login shell is Bash and Bash does not read ~/.bashrc for noninteractive SSH commands.
type SshdEnvTask struct {
	base
}

// Kind implements Task.
func (t *SshdEnvTask) Kind() string { return "sshd" }

// Name implements Task.
func (t *SshdEnvTask) Name() string { return sshdDropIn + " (PermitUserEnvironment BASH_ENV)" }

const sshdCheckScript = `out=$(sshd -T) || exit 5; printf '%s\n' "$out" | grep -i '^permituserenvironment ' || true`

func (t *SshdEnvTask) specReqs(env *Env) []transport.SpecReq {
	return []transport.SpecReq{specExec(true, sshdCheckScript, 10*time.Minute)}
}

// Check implements Task.
func (t *SshdEnvTask) Check(ctx context.Context, env *Env) (Check, error) {
	r, err := env.Sh(ctx, sshdCheckScript, true, "sshd -T")
	if err != nil {
		return Check{}, err
	}
	if r.Code != 0 {
		return Check{}, sudoHint(&ExitError{Label: "sshd -T (the sshd configuration)", Code: r.Code, Stderr: r.Err()}, true, "sshd")
	}
	if allowsBashEnv(r.Out()) {
		return Check{Status: Satisfied}, nil
	}
	return Check{Status: Change, Detail: "allow BASH_ENV in ~/.ssh/environment (Bash here does not read ~/.bashrc for SSH commands)"}, nil
}

// Apply implements Task. It tests the configuration before the reload.
func (t *SshdEnvTask) Apply(ctx context.Context, env *Env) (string, error) {
	if err := env.T.WriteFile(ctx, sshdDropIn, []byte(sshdDropInContent), 0o644, true); err != nil {
		return "", sudoHint(err, true, "sshd")
	}
	_, err := env.ShOK(ctx, `if ! sshd -t; then rm -f `+sshdDropIn+`; echo "sshd rejected the configuration" >&2; exit 1; fi
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl reload sshd 2>/dev/null || systemctl reload ssh
elif command -v rc-service >/dev/null 2>&1; then
  rc-service sshd reload
elif [ -f /var/run/sshd.pid ]; then
  kill -HUP "$(cat /var/run/sshd.pid)"
else
  echo "FAM cannot reload sshd on this machine" >&2; exit 1
fi`, true, "reload sshd")
	return "sshd reloaded", sudoHint(err, true, "sshd")
}

func environmentSection(env *Env, spec *config.Spec, secretList []secrets.Secret) (Section, error) {
	g := base{group: "environment"}
	sec := Section{Name: "environment"}
	if spec.Secrets != nil && env.Machine.IsCockpit() {
		target := env.Home(secretsFile)
		if f := spec.Secrets.File; f != nil && samePath(*f, target) {
			return sec, fmt.Errorf("the secrets file of the config is %s, the file that FAM writes on each machine. Keep the source file in another path, for example ~/.config/fam-secrets/secrets.env", target)
		}
		if inf := spec.Secrets.Infisical; inf != nil && inf.Credentials != nil && samePath(*inf.Credentials, target) {
			return sec, fmt.Errorf("the Infisical credentials file of the config is %s, the file that FAM writes on each machine. Keep it in another path, for example ~/.config/fam-secrets/infisical.env", target)
		}
	}
	if spec.Secrets != nil {
		names := secrets.Names(secretList)
		sec.Tasks = append(sec.Tasks, &FileTask{base: g, Path: "~/" + secretsFile, Content: secrets.Render(secretList), Mode: 0o600, Secret: true,
			Label: fmt.Sprintf("~/%s (%d secrets: %s)", secretsFile, len(names), strings.Join(names, ", "))})
	} else {
		sec.Tasks = append(sec.Tasks, &OwnedRemoveTask{base: g, Path: "~/" + secretsFile, Header: secrets.Header, Label: "~/" + secretsFile + " (no secrets in the config)"})
	}
	sec.Tasks = append(sec.Tasks,
		&FileTask{base: g, Path: "~/" + envFile, Content: envScript(spec), Mode: 0o644},
		&BlockTask{base: g, Path: "~/.zshenv", Block: loaderBlock()},
		&BlockTask{base: g, Path: "~/.bashrc", Block: loaderBlock(), Top: true},
		&BlockTask{base: g, Path: "~/.profile", Block: loaderBlock()},
		// Bash login shells read ~/.bash_profile or ~/.bash_login instead of ~/.profile when one exists.
		&BlockTask{base: g, Path: "~/.bash_profile", Block: loaderBlock(), OnlyIfExists: true},
		&BlockTask{base: g, Path: "~/.bash_login", Block: loaderBlock(), OnlyIfExists: true},
	)
	// Only for workers: the cockpit runs its commands locally, not over SSH.
	if !env.Machine.IsCockpit() && strings.HasSuffix(env.Facts.Shell, "/bash") && !env.Facts.BashSSHRC && env.Facts.OS == "linux" {
		sec.Tasks = append(sec.Tasks,
			&BlockTask{base: g, Path: "~/.ssh/environment", Mode: 0o600, Block: blockBegin + "\n" +
				"BASH_ENV=" + env.Home(envFile) + "\n" + blockEnd + "\n"},
			&SshdEnvTask{base: base{group: "environment", sudo: true}},
		)
	}
	return sec, nil
}

// OwnedRemoveTask removes a file only when its first line is the header that FAM writes.
// A file that FAM did not write stays.
type OwnedRemoveTask struct {
	base
	Path   string
	Header string
	Label  string
}

// Kind implements Task.
func (t *OwnedRemoveTask) Kind() string { return "file" }

// Name implements Task.
func (t *OwnedRemoveTask) Name() string { return t.Label }

func (t *OwnedRemoveTask) specReqs(env *Env) []transport.SpecReq {
	return []transport.SpecReq{specRead(false, env.Path(t.Path))}
}

// Check implements Task.
func (t *OwnedRemoveTask) Check(ctx context.Context, env *Env) (Check, error) {
	data, exists, err := env.T.ReadFile(ctx, env.Path(t.Path), false)
	if err != nil {
		return Check{}, err
	}
	if !exists {
		return Check{Status: Satisfied}, nil
	}
	if line, _, _ := strings.Cut(string(data), "\n"); line != strings.TrimSuffix(t.Header, "\n") {
		return Check{Status: Satisfied, Detail: "FAM did not write this file and keeps it"}, nil
	}
	return Check{Status: Change, Detail: "remove"}, nil
}

// Apply implements Task.
func (t *OwnedRemoveTask) Apply(ctx context.Context, env *Env) (string, error) {
	_, err := env.ShOK(ctx, "rm -f -- "+transport.Quote(env.Path(t.Path)), false, "remove "+t.Path)
	return "removed", err
}

// --- instructions ---

const managedPrefix = "<!-- Written by FAM from the FAM project. Do not edit: FAM replaces this file. fam-sha256:"

// managedContent adds the FAM marker with the digest of the body.
func managedContent(body []byte) []byte {
	h := sha256.Sum256(body)
	return append([]byte(managedPrefix+hex.EncodeToString(h[:])+" -->\n"), body...)
}

// writtenByFAM reports whether data is a FAM file that nobody changed.
func writtenByFAM(data []byte) bool {
	line, body, ok := bytes.Cut(data, []byte("\n"))
	if !ok || !bytes.HasPrefix(line, []byte(managedPrefix)) {
		return false
	}
	want := strings.TrimSuffix(strings.TrimPrefix(string(line), managedPrefix), " -->")
	h := sha256.Sum256(body)
	return want == hex.EncodeToString(h[:])
}

// ManagedFileTask is a file that FAM owns. Before FAM replaces a file that it did not write,
// it saves a copy next to it.
type ManagedFileTask struct {
	base
	Path    string
	Content []byte
}

// Kind implements Task.
func (t *ManagedFileTask) Kind() string { return "file" }

// Name implements Task.
func (t *ManagedFileTask) Name() string { return t.Path }

func (t *ManagedFileTask) specReqs(env *Env) []transport.SpecReq {
	return []transport.SpecReq{specStat(false, env.Path(t.Path))}
}

// Check implements Task.
func (t *ManagedFileTask) Check(ctx context.Context, env *Env) (Check, error) {
	p := env.Path(t.Path)
	fi, err := env.T.Stat(ctx, p, false)
	if err != nil {
		return Check{}, err
	}
	if !fi.Exists {
		return Check{
			Status: Change,
			Detail: "create",
			Plan:   writePrimPlan(p, t.Content, 0o644, false, false, true, "", ""),
		}, nil
	}
	if fi.Type != "file" {
		return Check{}, conflict(fmt.Sprintf("%s is a %s. FAM needs a regular file here", p, fi.Type))
	}
	if fi.SHA256 == sha(t.Content) {
		return Check{Status: Satisfied}, nil
	}
	cur, _, err := env.T.ReadFile(ctx, p, false)
	if err != nil {
		return Check{}, err
	}
	if writtenByFAM(cur) {
		return Check{
			Status: Change,
			Detail: "update",
			Plan:   writePrimPlan(p, t.Content, 0o644, false, false, false, fi.SHA256, ""),
		}, nil
	}
	return Check{Status: Change, Detail: "replace a file that FAM did not write (FAM saves a copy)"}, nil
}

// Apply implements Task.
func (t *ManagedFileTask) Apply(ctx context.Context, env *Env) (string, error) {
	p := env.Path(t.Path)
	cur, exists, err := env.T.ReadFile(ctx, p, false)
	if err != nil {
		return "", err
	}
	detail := "written"
	if exists && !writtenByFAM(cur) {
		r, err := env.ShOK(ctx, fmt.Sprintf(`b=%[1]s.fam-backup-$(date +%%Y%%m%%d%%H%%M%%S); cp -p -- %[1]s "$b" && echo "$b"`, transport.Quote(p)), false, "save a copy of "+p)
		if err != nil {
			return "", err
		}
		detail = "written, copy saved as " + r.Out()
	}
	return detail, env.T.WriteFile(ctx, p, t.Content, 0o644, false)
}

// TemplateData is the data of the instruction templates.
func templateData(env *Env) map[string]any {
	f := env.Facts
	return map[string]any{
		"Machine": map[string]any{"Name": env.Machine.Name, "Cockpit": env.Machine.Cockpit, "Tags": env.Machine.Tags, "Address": env.Machine.Address, "User": env.Machine.User},
		"Facts": map[string]any{"Machine": f.Machine, "Cockpit": f.Cockpit, "Tags": f.Tags, "OS": f.OS, "Distro": f.Distro, "DistroVersion": f.DistroVersion,
			"Arch": f.Arch, "CPUs": f.CPUs, "Home": f.Home, "User": f.User, "Hostname": f.Hostname, "Init": f.Init, "Shell": f.Shell},
	}
}

func render(file string, data map[string]any) (string, error) {
	src, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	tpl, err := template.New(path.Base(file)).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return "", fmt.Errorf("instruction template %s: %w", file, err)
	}
	var b bytes.Buffer
	if err := tpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("instruction template %s: %w", file, err)
	}
	return b.String(), nil
}

func instructionsSection(env *Env, spec *config.Spec) (Section, error) {
	g := base{group: "instructions"}
	data := templateData(env)
	agents, err := render(path.Join(env.Project.InstructionsDir, "AGENTS.md"), data)
	if err != nil {
		return Section{}, err
	}
	joinMD := func(parts ...string) string {
		var out []string
		for _, p := range parts {
			if s := strings.Trim(p, "\n"); s != "" {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return ""
		}
		return strings.Join(out, "\n\n") + "\n"
	}
	agentsBody := joinMD(agents)
	claude := ""
	cp := path.Join(env.Project.InstructionsDir, "CLAUDE.md")
	if _, err := os.Stat(cp); err == nil {
		claude, err = render(cp, data)
		if err != nil {
			return Section{}, err
		}
	}
	claudeBody := joinMD(agents, claude)
	return Section{Name: "instructions", Tasks: []Task{
		&ManagedFileTask{base: g, Path: "~/.agents/AGENTS.md", Content: managedContent([]byte(agentsBody))},
		&ManagedFileTask{base: g, Path: "~/.agents/CLAUDE.md", Content: managedContent([]byte(claudeBody))},
		&SymlinkTask{base: g, Path: "~/.codex/AGENTS.md", Target: "~/.agents/AGENTS.md", Backup: true},
		&SymlinkTask{base: g, Path: "~/.claude/CLAUDE.md", Target: "~/.agents/CLAUDE.md", Backup: true},
	}}, nil
}

// samePath reports whether the cockpit path p (with `~/`) is target, also through links.
func samePath(p, target string) bool {
	src, err := secrets.ExpandHome(p)
	if err != nil {
		return false
	}
	if path.Clean(src) == path.Clean(target) {
		return true
	}
	a, err1 := filepath.EvalSymlinks(src)
	b, err2 := filepath.EvalSymlinks(target)
	return err1 == nil && err2 == nil && a == b
}
