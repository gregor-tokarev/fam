package engine

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

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
			if h := ee.Hint(); h != "" {
				p.Message = strings.TrimSuffix(p.Message, ".") + ". " + h
			}
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
	machines, sec, err := ev.Init(ctx, f.Properties())
	if err != nil {
		// Fall back to separate reads so a broken secrets block still reports machines.
		var merr error
		machines, merr = ev.MachineList(ctx, f.Properties())
		if merr != nil {
			add("error", project.ConfigPath, merr)
			return
		}
		sec, err = ev.Secrets(ctx, f.Properties())
	}
	result.Machines = machines
	add("error", "", checkProject(project))
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
		machines = []config.Machine{{Name: "check", Cockpit: true}}
	}
	type evalJob struct {
		m      config.Machine
		osName string
	}
	var jobs []evalJob
	for _, m := range machines {
		for _, osName := range []string{"linux", "darwin"} {
			jobs = append(jobs, evalJob{m: m, osName: osName})
		}
	}
	type evalOut struct {
		errs []error
	}
	outs := make([]evalOut, len(jobs))
	var wg sync.WaitGroup
	for idx, job := range jobs {
		idx, job := idx, job
		wg.Add(1)
		go func() {
			defer wg.Done()
			var errs []error
			jf := facts.Reference()
			jf.Machine = job.m.Name
			jf.Cockpit = job.m.Cockpit
			jf.Tags = job.m.Tags
			if job.osName == "darwin" {
				jf.OS = "darwin"
				jf.Distro = "macos"
				jf.DistroVersion = "15"
				jf.Arch = "arm64"
				jf.Libc = ""
				jf.Init = "launchd"
				jf.Home = "/Users/fam"
				jf.Shell = "/bin/zsh"
			}
			cfg, err := ev.Eval(ctx, jf.Properties())
			if err != nil {
				outs[idx].errs = []error{err}
				return
			}
			if problems := config.ValidationErrors(cfg); len(problems) > 0 {
				outs[idx].errs = problems
				return
			}
			spec, err := config.Resolve(cfg, job.m.Name)
			if err != nil {
				outs[idx].errs = []error{err}
				return
			}
			if secretsOK {
				for _, message := range missingSecrets(spec, list) {
					errs = append(errs, errors.New(message))
				}
			}
			env := &Env{Facts: jf, Machine: job.m, Project: project, Secrets: list}
			if _, _, err := buildSections(ctx, env, spec, true); err != nil {
				errs = append(errs, err)
			}
			outs[idx].errs = errs
		}()
	}
	wg.Wait()
	for _, o := range outs {
		for _, err := range o.errs {
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
