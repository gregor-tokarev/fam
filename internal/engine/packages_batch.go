package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gregor-tokarev/fam/internal/transport"
)

type batchPkgResult struct {
	detail string
}

func supportsBatchInstall(mgr string) bool {
	switch mgr {
	case "apt", "dnf", "pacman", "yay", "apk", "brew", "bun", "npm":
		return true
	}
	return false
}

func batchInstallScript(mgr string, pkgs []string, skipAptUpdate bool) (string, bool) {
	quoted := make([]string, len(pkgs))
	for i, p := range pkgs {
		quoted[i] = transport.Quote(p)
	}
	qList := strings.Join(quoted, " ")
	switch mgr {
	case "apt":
		if skipAptUpdate {
			return "DEBIAN_FRONTEND=noninteractive apt-get install -y " + qList, true
		}
		return "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y " + qList, true
	case "dnf":
		return "dnf install -y " + qList, true
	case "pacman":
		return "pacman -S --needed --noconfirm " + qList, true
	case "yay":
		return "yay -S --needed --noconfirm " + qList, true
	case "apk":
		return "apk add --no-cache " + qList, true
	case "brew":
		return "brew install " + qList, true
	case "bun":
		return "bun add -g " + qList, true
	case "npm":
		return "npm install -g " + qList, true
	default:
		return "", false
	}
}

func isBatchablePackage(t Task) (*PackageTask, bool) {
	pt, ok := t.(*PackageTask)
	if !ok {
		return nil, false
	}
	if pt.Absent || pt.CustomInstall || pt.Pin != "" || pt.Manager == "" || !supportsBatchInstall(pt.Manager) {
		return nil, false
	}
	return pt, true
}

// collectPackageBatch looks ahead from order[startIdx] to gather unsatisfied PackageTasks sharing
// the same package manager and sudo mode that are separated only by already-satisfied tasks (and no
// pending unit restart boundaries). This preserves exact stop-on-first-failure semantics because no
// intervening unsatisfied task is ever skipped over.
func collectPackageBatch(order []Task, startIdx int, pre map[Task]checked, satisfied map[Task]bool, secEnd map[Task]int, restartUnits map[Task]bool) []*PackageTask {
	first, ok := isBatchablePackage(order[startIdx])
	if !ok {
		return nil
	}
	if satisfied[first] {
		return nil
	}
	if res, has := pre[first]; has && (res.err != nil || res.c.Status != Change) {
		return nil
	}
	batch := []*PackageTask{first}
	seenPkg := map[string]bool{first.PackageName: true}
	curEnd := secEnd[first]

	for i := startIdx + 1; i < len(order); i++ {
		if i == curEnd {
			// Crossing into a new section: stop if the finished section had unit restarts pending
			// for any already-collected task.
			if restartUnits[order[i-1]] {
				break
			}
			curEnd = secEnd[order[i]]
		}
		x := order[i]
		if satisfied[x] {
			continue
		}
		pt, ok := isBatchablePackage(x)
		if !ok {
			break
		}
		if pt.Manager != first.Manager || pt.applySudo() != first.applySudo() {
			break
		}
		if res, has := pre[pt]; has && (res.err != nil || res.c.Status != Change) {
			break
		}
		if !seenPkg[pt.PackageName] {
			seenPkg[pt.PackageName] = true
			batch = append(batch, pt)
		}
		if len(pt.Restarts()) > 0 {
			// Stop after a task that triggers a unit restart so the restart runs on time.
			break
		}
	}
	if len(batch) < 2 {
		return nil
	}
	return batch
}

// runPackageBatch installs batch in a single package-manager invocation and verifies all packages.
// On non-connection failure it returns (nil, nil) so the caller falls back cleanly to single-package
// apply with exact per-task error attribution.
func runPackageBatch(ctx context.Context, env *Env, batch []*PackageTask) (map[*PackageTask]batchPkgResult, error) {
	if len(batch) < 2 {
		return nil, nil
	}
	mgr := batch[0].Manager
	sudo := batch[0].applySudo()
	pkgs := make([]string, len(batch))
	for i, pt := range batch {
		pkgs[i] = pt.PackageName
	}
	// Confirm manager binary exists before running the batch.
	tool := managers[mgr].tool
	chk, err := env.UserScript(ctx, "command -v "+transport.Quote(tool)+" >/dev/null 2>&1", batch[0].sudo, "check "+tool, time.Minute)
	if err != nil {
		if transport.IsConnError(err) {
			return nil, err
		}
		return nil, nil
	}
	if chk.Code != 0 {
		return nil, nil
	}

	script, ok := batchInstallScript(mgr, pkgs, mgr == "apt" && env.aptUpdated)
	if !ok {
		return nil, nil
	}
	label := fmt.Sprintf("install %d %s packages", len(pkgs), mgr)
	r, err := env.UserScript(ctx, script, sudo, label, scriptTimeout)
	if err != nil {
		if transport.IsConnError(err) {
			return nil, err
		}
		return nil, nil
	}
	if r.Code != 0 {
		return nil, nil
	}
	if mgr == "apt" {
		env.aptUpdated = true
	}
	if sr, ok := env.T.(transport.SessionRefresher); ok {
		if rerr := sr.RefreshSessionIfNeeded(ctx); rerr != nil {
			if transport.IsConnError(rerr) {
				return nil, rerr
			}
			return nil, nil
		}
	}

	// Verify all batched packages are installed in bulk (plus parallel version lookups).
	verified := map[string]bool{}
	if supportsBulkManager(mgr) {
		var bulkPkgs []string
		for _, pt := range batch {
			if mgr == "apt" && strings.Contains(pt.PackageName, ":") {
				continue
			}
			bulkPkgs = append(bulkPkgs, pt.PackageName)
		}
		if len(bulkPkgs) > 0 {
			inst, _, berr := queryManagerBulk(ctx, env, mgr, bulkPkgs, batch[0].sudo)
			if berr != nil {
				if transport.IsConnError(berr) {
					return nil, berr
				}
				return nil, nil
			}
			for p, ok := range inst {
				if ok {
					verified[p] = true
				}
			}
		}
	}

	out := make(map[*PackageTask]batchPkgResult, len(batch))
	var mu sync.Mutex
	var connErr error
	allOK := true
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxCheckConcurrency(env.T))
	for _, pt := range batch {
		pt := pt
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if !verified[pt.PackageName] {
				ok, ierr := pt.installed(ctx, env)
				if transport.IsConnError(ierr) {
					mu.Lock()
					if connErr == nil {
						connErr = ierr
					}
					mu.Unlock()
					return
				}
				if ierr != nil || !ok {
					mu.Lock()
					allOK = false
					mu.Unlock()
					return
				}
			}
			detail := "install: installed"
			if v := pt.version(ctx, env); v != "" {
				detail = "install: installed " + v
			}
			mu.Lock()
			out[pt] = batchPkgResult{detail: detail}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if connErr != nil {
		return nil, connErr
	}
	if !allOK {
		return nil, nil
	}
	return out, nil
}
