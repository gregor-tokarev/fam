package engine

import (
	"context"
	"math"
	"sync"
	"sync/atomic"

	"github.com/gregor-tokarev/fam/internal/progress"
	"github.com/gregor-tokarev/fam/internal/transport"
)

// runVerifyPairs executes the (noninteractive, interactive) verify pair for each spec in specs
// concurrently (capped below sshd MaxSessions on SSH), sharing the warm ControlMaster connection.
// runSecond controls whether the interactive shell runs after a non-connection noninteractive
// result (for example, gateUpdates skips interactive when noninteractive already failed).
func runVerifyPairs(ctx context.Context, env *Env, p *progress.Machine, specs []VerifySpec, runSecond func(shellRun) bool) [][2]shellRun {
	runs := make([][2]shellRun, len(specs))
	if len(specs) == 0 {
		return runs
	}
	var mu sync.Mutex
	var stopAt atomic.Int64
	stopAt.Store(math.MaxInt64)
	var wg sync.WaitGroup
	// Keep SSH direct verify sessions below sshd MaxSessions (10).
	limit := parallelCommands
	if env.T.Kind() != "ssh" {
		limit = maxCheckConcurrency(env.T)
	}
	sem := make(chan struct{}, limit)
	rp := newRunning(p)
	for i, v := range specs {
		sem <- struct{}{}
		if int64(i) > stopAt.Load() {
			<-sem
			break
		}
		wg.Add(1)
		rp.begin(i, "verify "+v.Name)
		go func(i int, v VerifySpec) {
			defer wg.Done()
			defer func() { <-sem }()
			defer rp.end(i)
			for k, interactive := range []bool{false, true} {
				if k > 0 && int64(i) > stopAt.Load() {
					return
				}
				res, err := runVerify(ctx, env, v, interactive)
				conn := transport.IsConnError(err)
				var sr shellRun
				if conn {
					sr = shellRun{err: err, ran: true}
				} else {
					sr = shellRun{row: verifyRow(v, verifyShells[k], res, err), ran: true}
				}
				mu.Lock()
				runs[i][k] = sr
				if conn && int64(i) < stopAt.Load() {
					stopAt.Store(int64(i))
				}
				mu.Unlock()
				if conn || (k == 0 && !runSecond(sr)) {
					return
				}
			}
		}(i, v)
	}
	wg.Wait()
	return runs
}
