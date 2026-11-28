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

func batchInstallScript(mgr string, pkgs []string, skipUpdate bool) (string, bool) {
	m, ok := managers[mgr]
	if !ok || !m.batchInstall {
		return "", false
	}
	quoted := make([]string, len(pkgs))
	for i, p := range pkgs {
		quoted[i] = transport.Quote(p)
	}
	tpl := m.install
	if skipUpdate && m.installNoUpdate != "" {
		tpl = m.installNoUpdate
	}
	return fmt.Sprintf(tpl, strings.Join(quoted, " ")), true
}

func isBatchablePackage(t Task) (*PackageTask, bool) {
	pt, ok := t.(*PackageTask)
	if !ok {
		return nil, false
	}
	if pt.Absent || pt.CustomInstall || pt.Pin != "" || pt.Manager == "" || !managers[pt.Manager].batchInstall {
		return nil, false
	}
	return pt, true
}

func hasPendingUpdate(t Task) bool {
	u, ok := t.(Updater)
	return ok && u.Updates()
}

// collectPackageBatch gathers strictly contiguous unsatisfied PackageTasks starting at
// order[startIdx] that share the same package manager and sudo mode, without crossing any
// intervening task, pending update boundary, or pending unit restart boundary.
func collectPackageBatch(order []Task, startIdx int, pre map[Task]checked, secEnd map[Task]int, restartUnits map[Task]bool) []*PackageTask {
	first, ok := isBatchablePackage(order[startIdx])
	if !ok {
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
		// Any later task with a pending update is a hard boundary (whether satisfied or missing).
		if hasPendingUpdate(x) {
			break
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
		if seenPkg[pt.PackageName] {
			break
		}
		seenPkg[pt.PackageName] = true
		batch = append(batch, pt)
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

// confirmStillMissing filters batch to packages that still need installation when pre does not
// have fresh Check results for every candidate (for example, after an earlier mutation cleared pre).
func confirmStillMissing(ctx context.Context, env *Env, batch []*PackageTask, pre map[Task]checked) ([]*PackageTask, error) {
	allPreConfirmed := true
	for _, pt := range batch {
		res, has := pre[pt]
		if !has || res.err != nil || res.c.Status != Change {
			allPreConfirmed = false
			break
		}
	}
	if allPreConfirmed {
		return batch, nil
	}

	mgr := batch[0].Manager
	checkedInst := map[string]bool{}
	checkedOK := map[string]bool{}
	if supportsBulkManager(mgr) {
		var bulkPkgs []string
		for _, pt := range batch {
			if mgr == "apt" && strings.Contains(pt.PackageName, ":") {
				continue
			}
			bulkPkgs = append(bulkPkgs, pt.PackageName)
		}
		if len(bulkPkgs) > 0 {
			inst, ok, berr := queryManagerBulk(ctx, env, mgr, bulkPkgs, batch[0].sudo)
			if berr != nil {
				if transport.IsConnError(berr) {
					return nil, berr
				}
				return nil, nil
			}
			if ok {
				for _, p := range bulkPkgs {
					checkedInst[p] = inst[p]
					checkedOK[p] = true
				}
			}
		}
	}

	var toInstall []*PackageTask
	for i, pt := range batch {
		inst := false
		if checkedOK[pt.PackageName] {
			inst = checkedInst[pt.PackageName]
		} else {
			var err error
			inst, err = pt.installed(ctx, env)
			if err != nil {
				if transport.IsConnError(err) {
					return nil, err
				}
				if i == 0 {
					return nil, nil
				}
				break
			}
		}
		if inst {
			if i == 0 {
				return nil, nil
			}
			// Stop at the first already-installed package so the batch stays a strictly contiguous
			// missing prefix and never jumps over an intervening package task.
			break
		}
		toInstall = append(toInstall, pt)
	}
	if len(toInstall) < 2 {
		return nil, nil
	}
	return toInstall, nil
}

// runPackageBatch installs batch in a single package-manager invocation and verifies all packages.
// On non-connection failure it returns (nil, nil) so the caller falls back cleanly to single-package
// apply with exact per-task error attribution; the underlying failed command is already recorded
// in the run log via env.UserScript.
func runPackageBatch(ctx context.Context, env *Env, batch []*PackageTask, pre map[Task]checked) (map[*PackageTask]batchPkgResult, error) {
	if len(batch) < 2 {
		return nil, nil
	}
	var err error
	batch, err = confirmStillMissing(ctx, env, batch, pre)
	if err != nil || len(batch) < 2 {
		return nil, err
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
	label := fmt.Sprintf("install %d %s packages (batch)", len(pkgs), mgr)
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
