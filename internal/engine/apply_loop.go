package engine

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/gregor-tokarev/fam/internal/output"
	"github.com/gregor-tokarev/fam/internal/transport"
)

// applyLoop executes the apply phase for one machine after the initial conflict-check pass.
// All loop state (cached check results, refresh/env-dirty/reconnect flags, unit restart queue,
// and batched package outcomes) lives on this struct and updates through one afterChange hook.
type applyLoop struct {
	mr  *machineRun
	r   *output.MachineResult
	env *Env

	order              []Task
	pos                map[Task]int
	secEnd             map[Task]int
	sectionHasRestarts map[Task]bool
	satisfied          map[Task]bool

	// Cached check results and invalidation flags:
	//   pre     holds current check results valid until the next mutation clears it.
	//   refresh is set after a mutation so the next run of previously-satisfied tasks can be
	//           re-checked in parallel before entering applyTask.
	//   fresh   is true when pre came from a post-mutation refresh (not the initial plan pass),
	//           allowing errors in pre to be reported immediately without re-running Check.
	pre         map[Task]checked
	refresh     bool
	fresh       bool
	stopped     bool
	mutated     bool
	envDirty    bool
	reconnect   bool
	index       map[Task]int
	batchedDone map[*PackageTask]batchPkgResult
	step        int

	// Per-section state, reset at the start of each section.
	units           []Unit
	failedInSection bool
}

func newApplyLoop(mr *machineRun, pre map[Task]checked) *applyLoop {
	l := &applyLoop{
		mr:                 mr,
		r:                  mr.res,
		env:                mr.env,
		pos:                map[Task]int{},
		secEnd:             map[Task]int{},
		sectionHasRestarts: map[Task]bool{},
		satisfied:          make(map[Task]bool, len(pre)),
		pre:                pre,
		index:              map[Task]int{},
		batchedDone:        map[*PackageTask]batchPkgResult{},
	}
	for t, res := range pre {
		l.satisfied[t] = res.err == nil && res.c.Status == Satisfied
	}
	for _, s := range mr.sects {
		if s.SkipReason != "" {
			continue
		}
		hasR := false
		for _, t := range s.Tasks {
			l.pos[t] = len(l.order)
			l.order = append(l.order, t)
			if rs, ok := t.(Restarter); ok && len(rs.Restarts()) > 0 {
				hasR = true
			}
		}
		for _, t := range s.Tasks {
			l.secEnd[t] = len(l.order)
			l.sectionHasRestarts[t] = s.RestartUnits && hasR
		}
	}
	return l
}

func (l *applyLoop) run(ctx context.Context, preVerify map[int][2]shellRun) {
	l.r.Status = "ok"
	l.mr.p.Count("applying", countTasks(l.mr.sects, true))
	for _, s := range l.mr.sects {
		if recordSkippedSection(l.r, s) {
			continue
		}
		l.runSection(ctx, s)
	}
	if l.r.Status == "error" {
		return
	}
	if !l.stopped && l.mutated {
		l.mr.p.Stage("checking the shell blocks")
		l.stopped = restoreBlocks(ctx, l.env, l.mr, l.index)
		if l.r.Status == "error" {
			return
		}
	}
	if l.r.Counts.Failed > 0 || l.stopped {
		l.r.Status = "failed"
		l.r.ErrorKind = "task"
		if l.r.Error == "" {
			l.r.Error = "a task failed. FAM stopped the tasks after it"
		}
		return
	}
	if l.r.Counts.Changed > 0 {
		l.r.Status = "changed"
	}
	if l.reconnect {
		l.mr.p.Stage("reconnecting")
		if err := l.env.T.Reconnect(ctx); err != nil {
			setError(l.r, err)
			return
		}
	}
	if l.mutated || l.reconnect {
		preVerify = nil
	}
	verifyMachineWithCache(ctx, l.mr, preVerify)
}

func (l *applyLoop) runSection(ctx context.Context, s Section) {
	l.units = nil
	l.failedInSection = false
	l.env.restarted = nil

	for ti := 0; ti < len(s.Tasks); ti++ {
		t := s.Tasks[ti]
		if l.stopped {
			l.r.Tasks = append(l.r.Tasks, taskResult(t, Skipped, "an earlier task failed", nil))
			l.r.Counts.Add(string(Skipped))
			continue
		}
		if l.envDirty && s.Name != "environment" {
			if !l.flushEnv(ctx) {
				l.failedInSection = true
				l.r.Tasks = append(l.r.Tasks, taskResult(t, Skipped, "an earlier task failed", nil))
				l.r.Counts.Add(string(Skipped))
				continue
			}
		}
		// After an apply, update, or restart, refresh the next segment in parallel: either a run
		// of previously-satisfied tasks, or a contiguous run of filesystem tasks on a PrimBatcher.
		if _, ok := l.pre[t]; !ok {
			if l.refresh && l.satisfied[t] {
				l.refreshFrom(ctx, t, l.step+1, s.RestartUnits && len(l.units) > 0)
			} else {
				l.checkFSBatch(ctx, s, ti)
			}
		}
		if pt, ok := t.(*PackageTask); ok {
			if br, done := l.batchedDone[pt]; done {
				delete(l.batchedDone, pt)
				l.step++
				l.mr.p.Task(l.step, taskLabel(t))
				l.afterChange(s.Name, t, Changed, br.detail, nil, touchApply)
				continue
			}
		}

		p := l.preChecked(t)
		if p == nil && l.tryPackageBatch(ctx, s.Name, t) {
			continue
		}
		if p == nil {
			if consumed := l.tryPrimBatch(ctx, s, ti); consumed > 0 {
				ti += consumed - 1
				continue
			}
		}

		l.step++
		l.mr.p.Task(l.step, taskLabel(t))
		st, detail, err, tc := applyTask(ctx, l.env, t, p)
		l.afterChange(s.Name, t, st, detail, err, tc)
	}

	if s.RestartUnits && !l.failedInSection && !l.stopped {
		l.restartSectionUnits(ctx)
	}
}

// preChecked returns a pre-checked result when it can skip re-checking t: either a current
// Satisfied result, or an error from a fresh post-mutation refresh.
func (l *applyLoop) preChecked(t Task) *checked {
	res, ok := l.pre[t]
	if !ok {
		return nil
	}
	if (res.err == nil && res.c.Status == Satisfied) || (res.err != nil && l.fresh) {
		return &res
	}
	return nil
}

func (l *applyLoop) refreshFrom(ctx context.Context, t Task, step int, inSection bool) {
	l.refresh = false
	end := len(l.order)
	if inSection {
		end = l.secEnd[t]
	}
	var seg []Task
	for _, x := range l.order[l.pos[t]:end] {
		if !l.satisfied[x] {
			break
		}
		seg = append(seg, x)
		if hasPendingUpdate(x) {
			break
		}
	}
	rp := newRunning(l.mr.p)
	rp.index = step
	results, _ := checkAll(ctx, l.env, seg, rp, true)
	l.pre = make(map[Task]checked, len(seg))
	for i, x := range seg {
		if results[i].ran {
			l.pre[x] = results[i]
		}
	}
	l.fresh = true
}

func isFSBatchCandidate(t Task) bool {
	switch x := t.(type) {
	case *FileTask:
		return !x.Absent
	case *SymlinkTask:
		return !x.Absent
	case *SkillLinkTask, *BlockTask, *ManagedFileTask, *SkillTask:
		return true
	case *MergeTask:
		return !x.Absent
	}
	return false
}

// checkFSBatch checks a contiguous run of filesystem tasks in section s in one parallel round
// trip when the transport supports PrimBatcher, populating l.pre so tryPrimBatch can pipeline
// all their mutations in one round trip while keeping 100% of Check/conflict logic in Go.
func (l *applyLoop) checkFSBatch(ctx context.Context, s Section, ti int) {
	pb, ok := l.env.T.(transport.PrimBatcher)
	if !ok || !pb.SupportsPrimBatch() {
		return
	}
	var seg []Task
	for j := ti; j < len(s.Tasks); j++ {
		tj := s.Tasks[j]
		if !isFSBatchCandidate(tj) {
			break
		}
		seg = append(seg, tj)
		if s.Name != "environment" && mayChangeShellEnv(tj) {
			break
		}
	}
	if len(seg) < 2 {
		return
	}
	rp := newRunning(l.mr.p)
	rp.index = l.step + 1
	results, _ := checkAll(ctx, l.env, seg, rp, false)
	l.pre = make(map[Task]checked, len(seg))
	for i, x := range seg {
		if results[i].ran {
			l.pre[x] = results[i]
		}
	}
	l.fresh = true
}

func (l *applyLoop) flushEnv(ctx context.Context) bool {
	if !l.envDirty {
		return true
	}
	l.envDirty = false
	if l.reconnect {
		l.mr.p.Stage("reconnecting")
		if err := l.env.T.Reconnect(ctx); err != nil {
			setError(l.r, err)
			l.stopped = true
			return false
		}
		l.reconnect = false
	}
	if sr, ok := l.env.T.(transport.SessionRefresher); ok {
		if err := sr.RefreshSession(ctx); err != nil {
			setError(l.r, err)
			l.stopped = true
			return false
		}
	}
	return true
}

// afterChange is the single state-transition hook for every task outcome (single, package batch,
// or primitive batch). It records results, invalidates stale check caches, updates apt index
// tracking, flags shell-env refreshes, and queues service restarts.
func (l *applyLoop) afterChange(sectionName string, t Task, st Status, detail string, err error, tc touch) {
	switch tc {
	case touchApply:
		l.mutated = true
		l.pre, l.refresh, l.fresh = nil, true, false
		if _, isPkg := t.(*PackageTask); !isPkg {
			l.env.aptUpdated = false
		}
	case touchUpdate:
		l.mutated = true
		l.pre, l.refresh, l.fresh = nil, true, false
	}
	l.index[t] = len(l.r.Tasks)
	l.r.Tasks = append(l.r.Tasks, taskResult(t, st, detail, err))
	l.r.Counts.Add(string(st))
	if transport.IsConnError(err) {
		setError(l.r, err)
		l.stopped = true
		l.failedInSection = true
		return
	}
	if st == Failed || st == Conflict {
		l.stopped = true
		l.failedInSection = true
		return
	}
	if st == Changed {
		// ScriptTask and PackageTask already refresh-if-needed inside applyTask/tryPackageBatch
		// before their post-apply checks; filesystem tasks defer one batched refresh to flushEnv.
		if sectionName == "environment" {
			l.envDirty = true
		} else if mayChangeShellEnv(t) {
			switch t.(type) {
			case *ScriptTask, *PackageTask:
			default:
				l.envDirty = true
			}
		}
		if _, ok := t.(*SshdEnvTask); ok {
			l.reconnect = true
		}
		// A Service task that restarted its unit satisfies earlier restart requests; later ones
		// still restart the unit at the end of the bundle.
		if len(l.env.restarted) > 0 {
			kept := l.units[:0]
			for _, u := range l.units {
				if !l.env.restarted[u] {
					kept = append(kept, u)
				}
			}
			l.units = kept
			l.env.restarted = nil
		}
		if rs, ok := t.(Restarter); ok {
			l.units = append(l.units, rs.Restarts()...)
		}
	}
}

func (l *applyLoop) tryPackageBatch(ctx context.Context, sectionName string, t Task) bool {
	batch := collectPackageBatch(l.order, l.pos[t], l.pre, l.secEnd, l.sectionHasRestarts)
	if len(batch) < 2 {
		return false
	}
	l.mr.p.Task(l.step+1, fmt.Sprintf("install %d %s packages", len(batch), batch[0].Manager))
	resMap, berr := runPackageBatch(ctx, l.env, batch, l.pre)
	if berr != nil {
		l.step++
		l.afterChange(sectionName, t, Failed, "install", berr, touchApply)
		return true
	}
	if len(resMap) < 2 {
		return false
	}
	pt := batch[0]
	firstRes, ok := resMap[pt]
	if !ok {
		return false
	}
	// Refresh persistent worker session if the package install touched watched startup files.
	if sr, ok := l.env.T.(transport.SessionRefresher); ok {
		if rerr := sr.RefreshSessionIfNeeded(ctx); rerr != nil {
			l.step++
			l.afterChange(sectionName, t, Failed, "install", rerr, touchApply)
			return true
		}
	}
	for bpt, br := range resMap {
		if bpt != pt {
			l.batchedDone[bpt] = br
		}
	}
	l.step++
	l.afterChange(sectionName, t, Changed, firstRes.detail, nil, touchApply)
	return true
}

// tryPrimBatch pipelines consecutive Go-planned dumb primitives (write, symlink, skill_write)
// from pre in one remote round trip. Each op carries a CAS precondition computed by Task.Check;
// any CAS mismatch ("stale") stops the batch cleanly and falls back to normal applyTask in Go.
func (l *applyLoop) tryPrimBatch(ctx context.Context, s Section, ti int) int {
	pb, ok := l.env.T.(transport.PrimBatcher)
	if !ok || !pb.SupportsPrimBatch() || l.pre == nil {
		return 0
	}
	type item struct {
		idx    int
		task   Task
		detail string
		plan   *PrimPlan
	}
	var items []item
	var ops []transport.PrimOp
	seenPaths := map[string]bool{}
	for j := ti; j < len(s.Tasks); j++ {
		tj := s.Tasks[j]
		res, has := l.pre[tj]
		if !has || res.err != nil || res.c.Status != Change || res.c.Plan == nil {
			break
		}
		plan := res.c.Plan
		// Never batch two tasks that touch the same or nested filesystem path.
		if plan.TargetPath != "" && overlapsPath(seenPaths, plan.TargetPath) {
			break
		}
		stopAfter := s.Name != "environment" && mayChangeShellEnv(tj)
		op, err := plan.finalizeOp(j, stopAfter)
		if err != nil {
			break
		}
		if plan.TargetPath != "" {
			seenPaths[plan.TargetPath] = true
		}
		items = append(items, item{idx: j, task: tj, detail: res.c.Detail, plan: plan})
		ops = append(ops, op)
		if stopAfter {
			break
		}
	}
	// Batching only saves round trips when at least two tasks are pipelined; a single task runs
	// directly through applyTask -> Task.Apply so Task.Apply always remains the canonical path.
	if len(ops) < 2 {
		return 0
	}

	doneOps := make(map[int]transport.PrimResult, len(ops))
	batchErr := pb.BatchPrims(ctx, ops, func(pr transport.PrimResult) {
		doneOps[pr.Index] = pr
		if !pr.Stale {
			l.mr.p.Task(l.step+(pr.Index-ti)+1, taskLabel(s.Tasks[pr.Index]))
		}
	})

	consumed := 0
	for k, it := range items {
		pr, ran := doneOps[it.idx]
		if !ran {
			if batchErr != nil {
				l.step++
				consumed++
				l.afterChange(s.Name, it.task, Failed, it.detail, batchErr, touchApply)
			}
			break
		}
		if pr.Stale {
			// Remote path changed since Check: stop batching here so the caller runs full applyTask.
			break
		}
		l.step++
		consumed++
		if pr.Err != "" {
			err := errors.New(pr.Err)
			if it.plan.SudoKind != "" {
				err = sudoHint(err, it.plan.Op.Sudo, it.plan.SudoKind)
			}
			l.afterChange(s.Name, it.task, Failed, it.detail, err, touchApply)
			break
		}
		if verr := verifyPrimResult(it.plan, pr); verr != nil {
			l.afterChange(s.Name, it.task, Failed, it.detail, verr, touchApply)
			break
		}
		l.afterChange(s.Name, it.task, Changed, it.detail, nil, touchApply)
		if l.stopped || k == len(items)-1 {
			break
		}
	}
	return consumed
}

func overlapsPath(seen map[string]bool, p string) bool {
	cp := path.Clean(p)
	for s := range seen {
		cs := path.Clean(s)
		if cp == cs || strings.HasPrefix(cp, cs+"/") || strings.HasPrefix(cs, cp+"/") {
			return true
		}
	}
	return false
}

func verifyPrimResult(plan *PrimPlan, pr transport.PrimResult) error {
	switch plan.Op.Op {
	case "write":
		if !pr.Exists || pr.Type != "file" || pr.SHA256 != plan.WantSHA256 || pr.Mode != uint32(plan.WantMode.Perm()) {
			return errors.New("the check still finds a difference after apply")
		}
	case "symlink":
		if !pr.Exists || pr.Type != "link" || pr.Target != plan.WantTarget {
			return errors.New("the check still finds a difference after apply")
		}
	case "skill_write":
		if !pr.Exists || !pr.IsDir || !pr.Managed || pr.Digest != plan.WantDigest {
			return errors.New("the check still finds a difference after apply")
		}
	}
	return nil
}

func (l *applyLoop) restartSectionUnits(ctx context.Context) {
	seen := map[Unit]bool{}
	for _, u := range l.units {
		if l.stopped {
			break
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		l.mr.p.Stage("restarting " + u.Name)
		l.mutated = true
		l.pre, l.refresh, l.fresh = nil, true, false
		rr := output.RestartResult{Unit: u.Name, Scope: u.Scope, Status: "restarted"}
		if err := restartUnit(ctx, l.env, u); err != nil {
			rr.Status = "failed"
			rr.Error = err.Error()
			l.stopped = true
			if transport.IsConnError(err) {
				setError(l.r, err)
			}
		}
		l.r.Restarts = append(l.r.Restarts, rr)
	}
}
