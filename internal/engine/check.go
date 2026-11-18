package engine

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/gregor-tokarev/fam/internal/config"
	"github.com/gregor-tokarev/fam/internal/facts"
	"github.com/gregor-tokarev/fam/internal/infisical"
	"github.com/gregor-tokarev/fam/internal/output"
	"github.com/gregor-tokarev/fam/internal/schema"
	"github.com/gregor-tokarev/fam/internal/secrets"
)

// Problem is a static project diagnostic. Pkl holds the original evaluator diagnostic.
type Problem struct {
	Severity string `json:"severity"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
	Pkl      string `json:"pkl,omitempty"`
}
type CheckResult struct {
	OK       bool             `json:"ok"`
	Problems []Problem        `json:"problems"`
	Errors   int              `json:"errors"`
	Warnings int              `json:"warnings"`
	Machines []config.Machine `json:"machines"`
}

var pklPosition = regexp.MustCompile(`(?m)^[ \t]*(\d+) \|[^\n]*\n[^\n]*\nat [^\n]*\((file:[^\n)]+)\)`)

// CheckProject validates local inputs. It runs no task, shell command, SSH, download, or write.
// Representative facts cover Linux and macOS branches. Remote facts remain a plan concern.
func CheckProject(ctx context.Context, project *Project) (result CheckResult) {
	result.Problems = []Problem{}
	result.Machines = []config.Machine{}
	seen := map[string]bool{}
	redactor := output.NewRedactor(nil)
	add := func(severity, file string, err error) {
		if err == nil {
			return
		}
		p := Problem{Severity: severity, File: file, Message: redactor.String(err.Error())}
		var ee *config.EvalError
		if errors.As(err, &ee) {
			p.File = ee.Path
			p.Pkl = redactor.String(ee.Err.Error())
			if m := pklPosition.FindStringSubmatch(p.Pkl); len(m) > 0 {
				p.File = m[2]
				if u, e := url.Parse(p.File); e == nil && u.Scheme == "file" {
					p.File = u.Path
				}
				p.Line, _ = strconv.Atoi(m[1])
			}
			// Keep the complete Pkl diagnostic separately and preserve its message in text output.
			msg := strings.TrimSpace(p.Pkl)
			msg = strings.TrimPrefix(msg, "–– Pkl Error ––")
			if i := strings.Index(msg, "\n\n"); i >= 0 {
				msg = msg[:i]
			}
			p.Message = strings.Join(strings.Fields(msg), " ")
		}
		key := fmt.Sprintf("%s:%s:%d:%s", p.Severity, p.File, p.Line, p.Message)
		if seen[key] {
			return
		}
		seen[key] = true
		result.Problems = append(result.Problems, p)
		if severity == "error" {
			result.Errors++
		} else {
			result.Warnings++
		}
	}
	defer func() { result.OK = result.Errors == 0 }()
	pkg, err := schema.Current()
	if err != nil {
		add("error", "", err)
		return
	}
	pin, err := schema.ProjectPin(project.Base)
	if err != nil {
		add("warning", project.Base+"/PklProject", err)
	} else if pin != pkg.Version {
		add("warning", project.Base+"/PklProject", fmt.Errorf("project pins FAM %s, this binary is %s. Run `fam migrate`", pin, pkg.Version))
	}
	if err := schema.Check(project.Base, pkg); err != nil {
		add("warning", project.Base+"/PklProject.deps.json", err)
	}
	ev, err := config.NewReadOnlyEvaluator(ctx, project.Base, project.ConfigPath)
	if err != nil {
		add("error", project.ConfigPath, err)
		return
	}
	defer ev.Close()
	f := facts.Reference()
	machines, err := ev.MachineList(ctx, f.Properties())
	if err != nil {
		add("error", project.ConfigPath, err)
		return
	}
	result.Machines = machines
	add("error", "", checkProject(project))
	sec, err := ev.Secrets(ctx, f.Properties())
	var list []secrets.Secret
	secretsOK := err == nil
	if err != nil {
		add("error", project.ConfigPath, err)
	} else if sec != nil {
		// Check never calls Infisical: it uses the cache of the last plan or apply.
		src, err := loadSecrets(ctx, sec, project.Base, false, true)
		redactor = output.NewRedactor(src.values())
		list = src.List
		secretsOK = err == nil
		switch {
		case errors.Is(err, infisical.ErrNoCache):
			add("warning", project.ConfigPath, fmt.Errorf("%v. Check does not call Infisical, so it does not compare the required secret names. Run `fam plan` once", err))
		case err != nil:
			add("error", sourceFile(sec, project.ConfigPath), err)
		}
	}
	if len(machines) == 0 {
		add("error", project.ConfigPath, fmt.Errorf("the config has no machines"))
		machines = []config.Machine{{Name: "check", Role: "cockpit"}}
	}
	for _, m := range machines {
		for _, osName := range []string{"linux", "darwin"} {
			f := facts.Reference()
			f.Machine = m.Name
			f.Role = m.Role
			if osName == "darwin" {
				f.OS = "darwin"
				f.Distro = "macos"
				f.DistroVersion = "15"
				f.Arch = "arm64"
				f.Libc = ""
				f.Init = "launchd"
				f.Home = "/Users/fam"
				f.Shell = "/bin/zsh"
			}
			cfg, err := ev.Eval(ctx, f.Properties())
			if err != nil {
				add("error", project.ConfigPath, err)
				continue
			}
			if problems := config.ValidationErrors(cfg); len(problems) > 0 {
				for _, err := range problems {
					add("error", project.ConfigPath, err)
				}
				continue
			}
			spec, err := config.Resolve(cfg, m.Name)
			if err != nil {
				add("error", project.ConfigPath, err)
				continue
			}
			if secretsOK {
				for _, message := range missingSecrets(spec, list) {
					add("error", project.ConfigPath, errors.New(message))
				}
			}
			env := &Env{Facts: f, Machine: m, Project: project, Secrets: list}
			_, _, err = buildSections(ctx, env, spec, true)
			add("error", project.ConfigPath, err)
		}
	}
	return
}

// missingSecrets supplies the same names-only diagnostics to check, plan, and apply.
func missingSecrets(spec *config.Spec, list []secrets.Secret) []string {
	supplied := map[string]bool{}
	for _, secret := range list {
		supplied[secret.Name] = true
	}
	var messages []string
	for _, u := range spec.Bundles {
		if u.State != "present" || u.Skipped() {
			continue
		}
		seen := map[string]bool{}
		for _, name := range u.Bundle.RequiredSecrets {
			if !supplied[name] && !seen[name] {
				messages = append(messages, fmt.Sprintf("bundle %s needs secret %s, but the config secrets do not supply it", u.Bundle.Name, name))
				seen[name] = true
			}
		}
	}
	return messages
}
