package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errSpecMissed = errors.New("speculative result missed")

type specEntry struct {
	done chan struct{}
	resp WireResp
	err  error
}

// specResultCache holds speculative read-only check results for a single CLI run.
// Each SSH instance owns its own private cache (never shared across concurrent CLI processes).
type specResultCache struct {
	mu      sync.Mutex
	entries map[string]*specEntry
	closed  bool
}

func newSpecResultCache(reqs []SpecReq) *specResultCache {
	entries := make(map[string]*specEntry, len(reqs))
	for _, r := range reqs {
		entries[r.Key] = &specEntry{done: make(chan struct{})}
	}
	return &specResultCache{entries: entries}
}

func newReadySpecCache(results map[string]WireResp) *specResultCache {
	if len(results) == 0 {
		return nil
	}
	entries := make(map[string]*specEntry, len(results))
	for k, v := range results {
		ch := make(chan struct{})
		close(ch)
		entries[k] = &specEntry{done: ch, resp: v}
	}
	return &specResultCache{entries: entries, closed: true}
}

func (c *specResultCache) resolve(key string, resp WireResp) {
	c.mu.Lock()
	e := c.entries[key]
	if e == nil || c.closed {
		c.mu.Unlock()
		return
	}
	select {
	case <-e.done:
	default:
		e.resp = resp
		close(e.done)
	}
	c.mu.Unlock()
}

func (c *specResultCache) finish(err error) {
	if err == nil {
		err = errSpecMissed
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for _, e := range c.entries {
		select {
		case <-e.done:
		default:
			e.err = err
			close(e.done)
		}
	}
	c.mu.Unlock()
}

func (c *specResultCache) lookup(ctx context.Context, key string) (WireResp, bool, error) {
	c.mu.Lock()
	e := c.entries[key]
	c.mu.Unlock()
	if e == nil {
		return WireResp{}, false, nil
	}
	select {
	case <-e.done:
		if errors.Is(e.err, errSpecMissed) {
			return WireResp{}, false, nil
		}
		if e.err != nil {
			return WireResp{}, true, e.err
		}
		return e.resp, true, nil
	case <-ctx.Done():
		return WireResp{}, true, ctx.Err()
	}
}

// RawFactsSHA returns the canonical SHA-256 hex digest of trimmed facts stdout.
// Must match wire_worker.py (FACTS_SHA / live_sha) and facts.RawHash.
func RawFactsSHA(b []byte) string {
	sum := sha256.Sum256(bytes.TrimRight(b, "\r\n"))
	return hex.EncodeToString(sum[:])
}

func (s *SSH) lookupSpec(ctx context.Context, key string) (WireResp, bool, error) {
	s.wireMu.Lock()
	sc := s.specCache
	s.wireMu.Unlock()
	if sc == nil {
		return WireResp{}, false, nil
	}
	return sc.lookup(ctx, key)
}

// ClearSpecCache discards any speculative pre-check cache after the initial check pass finishes.
func (s *SSH) ClearSpecCache() {
	s.wireMu.Lock()
	sc := s.specCache
	s.wireMu.Unlock()
	if sc != nil {
		s.wireMu.Lock()
		if s.specCache == sc {
			s.specCache = nil
		}
		s.wireMu.Unlock()
		sc.finish(errSpecMissed)
	}
}

func (s *SSH) bootstrapScript(factsScript, secretsRelPath, specB64 string) string {
	specExport := ""
	if specB64 != "" {
		specExport = "export FAM_SPEC_B64=" + Quote(specB64) + "\n"
	}
	return "FAM_FACTS_RAW=$(\n" + factsScript + "\n); FAM_FACTS_RC=$?\n" +
		"printf '%s\\n' \"$FAM_FACTS_RAW\"\n" +
		"printf 'FAM-SECRETS-BEGIN %d\\n' \"$FAM_FACTS_RC\"\n" +
		fmt.Sprintf("p=\"$HOME/\"%s\n", Quote(secretsRelPath)) +
		"if [ -f \"$p\" ] && [ -r \"$p\" ]; then\n" +
		"  wc -c < \"$p\"\n" +
		"  cat -- \"$p\"\n" +
		"else\n" +
		"  echo -1\n" +
		"fi\n" +
		"echo ''\n" +
		"if [ \"$FAM_FACTS_RC\" -eq 0 ]; then export FAM_FACTS_RAW; else unset FAM_FACTS_RAW; fi\n" +
		specExport +
		wireLaunchSnippet()
}

// coldSpecReqID is reserved for the inline speculative pre-check batch embedded via FAM_SPEC_B64
// during cold bootstrap; normal RPC IDs start strictly above it.
const coldSpecReqID uint64 = 1

// BootstrapSpec establishes the persistent SSH worker (reusing SessionPool when configured),
// returns facts + secrets, and optionally runs a speculative read-only check batch.
func (s *SSH) BootstrapSpec(ctx context.Context, factsScript, secretsRelPath, expectedFactsSHA string, reqs []SpecReq) (BootstrapResult, error) {
	if err := s.lockBoot(ctx); err != nil {
		return BootstrapResult{FactsRes: Result{Code: 255}}, err
	}
	defer s.unlockBoot()
	if s.closed {
		return BootstrapResult{FactsRes: Result{Code: 255}}, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "ssh transport closed"}
	}
	key, sendEnv, setEnv, reusable := ControlSocketProbeCtx(ctx, s.Target, s.ConfigFile)
	if !reusable {
		if ctx.Err() != nil {
			return BootstrapResult{FactsRes: Result{Code: 255}}, ctx.Err()
		}
		// Identity could not be statically resolved (e.g. slow Match exec, Match command,
		// ProxyCommand, or jump ControlPath): bypass daemon pool and establish a fresh
		// unique-socket ControlMaster while still preserving effective SendEnv/SetEnv.
		s.wireMu.Lock()
		s.pinnedKey = ""
		s.unreusable = true
		s.masterDead = false
		s.sendEnv = sendEnv
		s.setEnv = setEnv
		s.wireMu.Unlock()
		return s.bootstrapCold(ctx, factsScript, secretsRelPath, expectedFactsSHA, reqs)
	}
	if s.Pool != nil {
		res, w, lease, err := s.Pool.WarmBootstrap(ctx, s.Machine, s.Target, s.ConfigFile, key, factsScript, secretsRelPath, expectedFactsSHA, reqs)
		if err == nil && res.SessionKey == key && res.ControlSock != "" {
			s.observe(Cmd{Label: "collect facts"}, res.FactsRes, nil)
			if res.SecretsExists {
				s.observe(Cmd{Label: "read ~/" + secretsRelPath, Secret: true}, Result{}, nil)
			}
			var sc *specResultCache
			if res.SpecHit && len(res.SpecResults) > 0 {
				sc = newReadySpecCache(res.SpecResults)
			}
			s.wireMu.Lock()
			oldWire := s.wire
			oldPooled := s.pooled
			oldLease := s.poolLease
			oldSC := s.specCache
			s.wire = w
			s.wireGen++
			s.hasPy3 = res.HasWire
			s.poolLease = lease
			s.pooled = true
			s.pinnedKey = key
			s.unreusable = false
			s.controlSock = res.ControlSock
			s.masterPinned = true
			s.masterDead = false
			s.sendEnv = sendEnv
			s.setEnv = setEnv
			s.specCache = sc
			s.wireMu.Unlock()
			if oldSC != nil {
				oldSC.finish(errSpecMissed)
			}
			if oldLease != nil && oldLease != lease {
				_ = oldLease.Close()
			}
			if !oldPooled && oldWire != nil && oldWire != w {
				_ = oldWire.Close()
			}
			return res, nil
		}
		if err == nil {
			// Daemon returned a mismatched session key or missing socket: refuse reuse and fall
			// back through a fresh unique-socket direct connection.
			if lease != nil {
				_ = lease.Close()
			}
			s.wireMu.Lock()
			s.pinnedKey = ""
			s.unreusable = true
			s.masterDead = false
			s.sendEnv = sendEnv
			s.setEnv = setEnv
			s.wireMu.Unlock()
			return s.bootstrapCold(ctx, factsScript, secretsRelPath, expectedFactsSHA, reqs)
		}
		if IsConnError(err) || ctx.Err() != nil {
			s.observe(Cmd{Label: "collect facts"}, Result{Code: 255}, err)
			return BootstrapResult{FactsRes: Result{Code: 255}}, err
		}
		var idErr *IdentityError
		if errors.As(err, &idErr) {
			// Daemon could not verify identity: fall back via fresh unique-socket direct SSH
			// without clearing s.Pool permanently or reusing any existing socket.
			s.wireMu.Lock()
			s.pinnedKey = ""
			s.unreusable = true
			s.masterDead = false
			s.sendEnv = sendEnv
			s.setEnv = setEnv
			s.wireMu.Unlock()
			return s.bootstrapCold(ctx, factsScript, secretsRelPath, expectedFactsSHA, reqs)
		}
		// Daemon unavailable: fall back transparently to direct SSH for this run.
		s.Pool = nil
	}
	s.wireMu.Lock()
	s.pinnedKey = key
	s.unreusable = false
	s.masterDead = false
	s.sendEnv = sendEnv
	s.setEnv = setEnv
	s.wireMu.Unlock()
	return s.bootstrapCold(ctx, factsScript, secretsRelPath, expectedFactsSHA, reqs)
}

// WarmBootstrap is used by the local session daemon to reuse an active persistent worker for
// facts, secrets, and speculative checks in a single RTT, falling back to cold bootstrap if the
// worker is missing, idle-dead, or its watched shell startup files changed.
// Returns the wire generation active on completion/error so daemon eviction is generation-checked.
func (s *SSH) WarmBootstrap(ctx context.Context, factsScript, secretsRelPath, expectedFactsSHA string, reqs []SpecReq) (BootstrapResult, uint64, error) {
	if err := s.lockBoot(ctx); err != nil {
		return BootstrapResult{FactsRes: Result{Code: 255}}, s.currentWireGen(), err
	}
	defer s.unlockBoot()
	if s.closed {
		return BootstrapResult{FactsRes: Result{Code: 255}}, s.currentWireGen(), &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "ssh transport closed"}
	}

	if w := s.getWire(); w != nil {
		wctx := ctx
		var cancel context.CancelFunc
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			wctx, cancel = context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
		}
		resp, werr := w.Call(wctx, WireReq{
			Op:          "warm_bootstrap",
			FactsScript: factsScript,
			SecretsRel:  secretsRelPath,
			FactsSHA:    expectedFactsSHA,
			Reqs:        reqs,
		})
		if werr == nil && resp.Err == "" && !resp.TimedOut {
			if resp.EnvChanged {
				// Startup files changed since this worker started: restart the login-shell worker
				// over the existing ControlMaster so facts and checks see the fresh environment.
				s.closeWire()
			} else {
				outB, _ := base64.StdEncoding.DecodeString(resp.StdoutB64)
				errB, _ := base64.StdEncoding.DecodeString(resp.StderrB64)
				secB, _ := base64.StdEncoding.DecodeString(resp.DataB64)
				return BootstrapResult{
					FactsRes: Result{
						Stdout:   outB,
						Stderr:   errB,
						Code:     resp.Code,
						Duration: time.Duration(resp.DurationMS) * time.Millisecond,
					},
					SecretsData:   secB,
					SecretsExists: resp.Exists,
					SpecHit:       resp.SpecHit,
					SpecResults:   resp.SpecResults,
					HasWire:       true,
					ControlSock:   s.ControlSock(),
				}, s.currentWireGen(), nil
			}
		} else {
			_ = s.reconnectLocked(ctx)
		}
	}

	res, err := s.bootstrapCold(ctx, factsScript, secretsRelPath, expectedFactsSHA, reqs)
	gen := s.currentWireGen()
	if err != nil {
		return res, gen, err
	}
	if res.SpecHit {
		s.wireMu.Lock()
		sc := s.specCache
		s.wireMu.Unlock()
		if sc != nil {
			res.SpecResults = make(map[string]WireResp, len(reqs))
			for _, r := range reqs {
				if wr, hit, lerr := sc.lookup(ctx, r.Key); hit && lerr == nil {
					res.SpecResults[r.Key] = wr
				}
			}
			s.ClearSpecCache()
		}
	}
	res.HasWire = s.getWire() != nil
	return res, gen, nil
}

func (s *SSH) bootstrapCold(ctx context.Context, factsScript, secretsRelPath, expectedFactsSHA string, reqs []SpecReq) (BootstrapResult, error) {
	// Always establish a fresh unique-socket ControlMaster on cold bootstrap. Detach and close
	// any prior wire, spec cache, and pool lease up front so a failed teardown or mid-bootstrap
	// error can never leave an old wire callable or orphan a newly allocated local master.
	s.wireMu.Lock()
	wasPooled := s.pooled
	hadSock := s.controlSock != ""
	oldWire := s.wire
	oldLease := s.poolLease
	oldSC := s.specCache
	s.wire = nil
	s.hasPy3 = false
	s.specCache = nil
	s.masterPinned = false
	s.masterDead = false
	if wasPooled {
		s.pooled = false
		s.poolLease = nil
		s.controlSock = ""
	}
	s.wireMu.Unlock()
	if oldSC != nil {
		oldSC.finish(errSpecMissed)
	}
	if oldLease != nil {
		_ = oldLease.Close()
	}
	if !wasPooled && oldWire != nil {
		_ = oldWire.Close()
	}
	if !wasPooled && hadSock {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		exitErr := s.control(cleanCtx, "exit")
		cleanCancel()
		if exitErr != nil {
			// Retain s.controlSock for later teardown and refuse to reuse it as a fresh master.
			s.wireMu.Lock()
			s.masterPinned = true
			s.masterDead = true
			s.wireMu.Unlock()
			ce := &ConnError{Machine: s.Machine, Target: s.Target, Stderr: exitErr.Error()}
			s.observe(Cmd{Label: "collect facts"}, Result{Code: 255}, ce)
			return BootstrapResult{FactsRes: Result{Code: 255}}, ce
		}
	}

	var specB64 string
	var specSent bool
	if expectedFactsSHA != "" && len(reqs) > 0 {
		if b, jerr := json.Marshal(WireReq{ID: coldSpecReqID, Op: "spec_batch", FactsSHA: expectedFactsSHA, Reqs: reqs}); jerr == nil {
			enc := base64.StdEncoding.EncodeToString(b)
			if len(enc) <= 64*1024 {
				specB64 = enc
				specSent = true
			}
		}
	}
	start := time.Now()
	script := s.bootstrapScript(factsScript, secretsRelPath, specB64)
	s.wireMu.Lock()
	unreusable := s.unreusable
	s.wireMu.Unlock()
	if unreusable {
		// When SSH config is command-dependent or timed out the quick reuse probe, resolve
		// effective SendEnv/SetEnv for the exact master launch invocation under the full
		// bootstrap context and surface resolution failures instead of silently dropping env.
		sendEnv, setEnv, ok := resolveEnvForCommand(ctx, s.Target, s.ConfigFile, "sh -c "+Quote(script))
		if !ok {
			if ctx.Err() != nil {
				return BootstrapResult{FactsRes: Result{Code: 255}}, ctx.Err()
			}
			ce := &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "failed to resolve SSH environment forwarding configuration"}
			s.observe(Cmd{Label: "collect facts"}, Result{Code: 255, Duration: time.Since(start)}, ce)
			return BootstrapResult{FactsRes: Result{Code: 255}}, ce
		}
		s.wireMu.Lock()
		s.sendEnv = sendEnv
		s.setEnv = setEnv
		s.wireMu.Unlock()
	}
	proc, err := s.startSSHProcess(ctx, script)
	if err != nil {
		s.cleanupPartialMaster()
		return BootstrapResult{}, err
	}

	var factsBuf bytes.Buffer
	foundSecretsMarker := false
	factsCode := 0
	for {
		line, rerr := proc.br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(string(line), "\r\n")
			if rest, ok := strings.CutPrefix(trimmed, "FAM-SECRETS-BEGIN"); ok {
				if rc, perr := strconv.Atoi(strings.TrimSpace(rest)); perr == nil {
					factsCode = rc
				}
				foundSecretsMarker = true
				break
			}
			factsBuf.Write(line)
		}
		if rerr != nil {
			break
		}
	}

	if !foundSecretsMarker {
		_ = proc.stdin.Close()
		waitErr := proc.cmd.Wait()
		close(proc.done)
		dur := time.Since(start)
		res := Result{Stdout: factsBuf.Bytes(), Stderr: []byte(proc.errBuf.String()), Duration: dur}
		if waitErr != nil {
			var ee *exec.ExitError
			if errors.As(waitErr, &ee) {
				res.Code = ee.ExitCode()
			} else {
				s.cleanupPartialMaster()
				err = fmt.Errorf("run ssh: %w", waitErr)
				s.observe(Cmd{Label: "collect facts"}, res, err)
				return BootstrapResult{FactsRes: res}, err
			}
		}
		if ctx.Err() != nil {
			s.cleanupPartialMaster()
			s.observe(Cmd{Label: "collect facts"}, res, ctx.Err())
			return BootstrapResult{FactsRes: res}, ctx.Err()
		}
		if res.Code == 255 {
			s.cleanupPartialMaster()
			ce := &ConnError{Machine: s.Machine, Target: s.Target, Stderr: string(res.Stderr)}
			s.observe(Cmd{Label: "collect facts"}, res, ce)
			return BootstrapResult{FactsRes: res}, ce
		}
		s.cleanupPartialMaster()
		s.observe(Cmd{Label: "collect facts"}, res, nil)
		return BootstrapResult{FactsRes: res}, nil
	}

	lenLine, err := proc.br.ReadString('\n')
	if err != nil {
		_ = proc.stdin.Close()
		_ = proc.cmd.Wait()
		close(proc.done)
		s.cleanupPartialMaster()
		if ctx.Err() != nil {
			return BootstrapResult{FactsRes: Result{Code: 255}}, ctx.Err()
		}
		return s.bootstrapConnFail(proc.errBuf.String(), time.Since(start), err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(lenLine))
	if err != nil {
		_ = proc.stdin.Close()
		_ = proc.cmd.Wait()
		close(proc.done)
		s.cleanupPartialMaster()
		return BootstrapResult{}, fmt.Errorf("bootstrap secrets length: %w", err)
	}
	var secretsData []byte
	secretsExists := false
	if n >= 0 {
		secretsExists = true
		secretsData = make([]byte, n)
		if n > 0 {
			if _, err := io.ReadFull(proc.br, secretsData); err != nil {
				_ = proc.stdin.Close()
				_ = proc.cmd.Wait()
				close(proc.done)
				s.cleanupPartialMaster()
				if ctx.Err() != nil {
					return BootstrapResult{FactsRes: Result{Code: 255}}, ctx.Err()
				}
				return s.bootstrapConnFail(proc.errBuf.String(), time.Since(start), err)
			}
		}
	}

	wireMode, waited, cerr := s.readWireHandshake(ctx, proc, start)
	if cerr != nil {
		s.cleanupPartialMaster()
		return BootstrapResult{FactsRes: Result{Code: 255}}, cerr
	}

	dur := time.Since(start)
	factsRes := Result{Stdout: factsBuf.Bytes(), Stderr: []byte(proc.errBuf.String()), Code: factsCode, Duration: dur}
	s.observe(Cmd{Label: "collect facts"}, factsRes, nil)
	if secretsExists {
		s.observe(Cmd{Label: "read ~/" + secretsRelPath, Secret: true}, Result{}, nil)
	}

	specHit := false
	if wireMode == "FAM-WIRE-PY3" {
		w := newSSHWire(s.Machine, s.Target, proc)
		var sc *specResultCache
		if specSent {
			w.nextID.Store(coldSpecReqID)
			specPC := &pendingCall{ch: make(chan WireResp, len(reqs)+4), cancel: make(chan struct{}), stream: true}
			w.pending[coldSpecReqID] = specPC
			if factsCode == 0 && RawFactsSHA(factsBuf.Bytes()) == expectedFactsSHA {
				specHit = true
				sc = newSpecResultCache(reqs)
				go func() {
					for {
						resp, ok := <-specPC.ch
						if !ok {
							sc.finish(w.closedErr())
							return
						}
						if resp.Done {
							if resp.Err != "" {
								sc.finish(errors.New(resp.Err))
							} else {
								sc.finish(errSpecMissed)
							}
							return
						}
						if resp.SpecKey != "" && resp.SpecResp != nil {
							sc.resolve(resp.SpecKey, *resp.SpecResp)
						}
					}
				}()
			} else {
				go func() {
					for resp := range specPC.ch {
						if resp.Done {
							return
						}
					}
				}()
			}
		}
		w.start(proc.br, proc.errBuf)
		s.wireMu.Lock()
		s.masterPinned = true
		s.wireMu.Unlock()
		s.installWire(w, sc, true)
	} else {
		if !waited {
			_ = proc.stdin.Close()
			_ = proc.cmd.Wait()
		}
		s.wireMu.Lock()
		s.masterPinned = true
		s.wireMu.Unlock()
		// Replace any prior pooled wire/lease/specCache cleanly when falling back to no-wire mode.
		s.installWire(nil, nil, false)
	}
	return BootstrapResult{
		FactsRes:      factsRes,
		SecretsData:   secretsData,
		SecretsExists: secretsExists,
		SpecHit:       specHit,
		HasWire:       wireMode == "FAM-WIRE-PY3",
		ControlSock:   s.ControlSock(),
	}, nil
}

func (s *SSH) readWireHandshake(ctx context.Context, proc *startedSSH, start time.Time) (wireMode string, waited bool, err error) {
	for {
		line, rerr := proc.br.ReadString('\n')
		if rerr != nil {
			_ = proc.stdin.Close()
			waitErr := proc.cmd.Wait()
			close(proc.done)
			var ee *exec.ExitError
			if errors.As(waitErr, &ee) && ee.ExitCode() == 255 {
				_, ce := s.bootstrapConnFail(proc.errBuf.String(), time.Since(start), rerr)
				return "", true, ce
			}
			if ctx.Err() != nil {
				return "", true, ctx.Err()
			}
			// Non-255 exit (e.g. a broken python3 shim exiting 127): continue without wire.
			return "FAM-WIRE-NONE", true, nil
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "FAM-WIRE-PY3" {
			close(proc.done)
			return trimmed, false, nil
		}
		if trimmed == "FAM-WIRE-NONE" {
			// Keep context cancellation active all the way through Wait() on no-worker paths.
			_ = proc.stdin.Close()
			waitErr := proc.cmd.Wait()
			close(proc.done)
			var ee *exec.ExitError
			if errors.As(waitErr, &ee) && ee.ExitCode() == 255 {
				_, ce := s.bootstrapConnFail(proc.errBuf.String(), time.Since(start), waitErr)
				return "", true, ce
			}
			if ctx.Err() != nil {
				return "", true, ctx.Err()
			}
			return trimmed, true, nil
		}
	}
}

func (s *SSH) installWire(w WireConn, sc *specResultCache, hasPy3 bool) {
	s.wireMu.Lock()
	old := s.wire
	oldPooled := s.pooled
	oldLease := s.poolLease
	oldSC := s.specCache
	s.wire = w
	s.wireGen++
	s.pooled = false
	s.poolLease = nil
	s.specCache = sc
	s.hasPy3 = hasPy3
	s.wireMu.Unlock()
	if oldSC != nil {
		oldSC.finish(errSpecMissed)
	}
	if oldLease != nil {
		_ = oldLease.Close()
	}
	if !oldPooled && old != nil && old != w {
		_ = old.Close()
	}
}

func (s *SSH) bootstrapConnFail(stderr string, dur time.Duration, err error) (BootstrapResult, error) {
	ce := &ConnError{Machine: s.Machine, Target: s.Target, Stderr: stderr}
	if strings.TrimSpace(ce.Stderr) == "" && err != nil {
		ce.Stderr = err.Error()
	}
	s.observe(Cmd{Label: "collect facts"}, Result{Stderr: []byte(stderr), Code: 255, Duration: dur}, ce)
	return BootstrapResult{FactsRes: Result{Code: 255}}, ce
}

// WireCall forwards one raw WireReq to the active local sshWire (used by the session daemon),
// verifying expectedSock (when non-empty) atomically so a stale client pinned to an older
// ControlMaster fails closed without using or evicting a newer replacement master.
func (s *SSH) WireCall(ctx context.Context, req WireReq, expectedSock string) (WireResp, uint64, bool, error) {
	s.wireMu.Lock()
	w, gen, sock := s.wire, s.wireGen, s.controlSock
	s.wireMu.Unlock()
	if expectedSock != "" && sock != expectedSock {
		return WireResp{}, gen, false, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "remote worker is not active"}
	}
	if w == nil {
		return WireResp{}, gen, true, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "remote worker is not active"}
	}
	resp, err := w.Call(ctx, req)
	return resp, gen, true, err
}

// WireCallStream forwards one streaming WireReq to the active local sshWire (used by the daemon),
// verifying expectedSock (when non-empty) atomically so a stale client pinned to an older
// ControlMaster fails closed without using or evicting a newer replacement master.
func (s *SSH) WireCallStream(ctx context.Context, req WireReq, expectedSock string, onResp func(WireResp)) (uint64, bool, error) {
	s.wireMu.Lock()
	w, gen, sock := s.wire, s.wireGen, s.controlSock
	s.wireMu.Unlock()
	if expectedSock != "" && sock != expectedSock {
		return gen, false, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "remote worker is not active"}
	}
	if w == nil {
		return gen, true, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "remote worker is not active"}
	}
	return gen, true, w.CallStream(ctx, req, onResp)
}

// RefreshSessionIfNeeded implements SessionRefresher. It only restarts the persistent remote
// worker when a command observed a change to any watched shell startup or environment file.
func (s *SSH) RefreshSessionIfNeeded(ctx context.Context) error {
	s.wireMu.Lock()
	dirty := s.envChanged
	hasPy3 := s.hasPy3
	s.wireMu.Unlock()
	if !hasPy3 || !dirty {
		return nil
	}
	return s.RefreshSession(ctx)
}

// cleanupPartialMaster tears down any ControlMaster that may have started before an aborted or
// timed-out launch, using a bounded background context and retaining s.controlSock if exit fails.
func (s *SSH) cleanupPartialMaster() error {
	cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanCancel()
	err := s.control(cleanCtx, "exit")
	if err != nil {
		// Socket still appears live; poison the transport so s.controlSock is retained for Close().
		s.wireMu.Lock()
		s.masterPinned = true
		s.masterDead = true
		s.wireMu.Unlock()
	}
	return err
}

// EnsureWorker closes any existing local worker and starts a fresh persistent worker
// (establishing a ControlMaster if needed). When expectedSock is non-empty, it requires the
// currently pinned master socket to match expectedSock (returning evict=false on mismatch so
// a stale client never replaces or evicts a newer master).
func (s *SSH) EnsureWorker(ctx context.Context, expectedSock string) (bool, uint64, string, bool, error) {
	if err := s.lockBoot(ctx); err != nil {
		return false, s.currentWireGen(), "", false, err
	}
	defer s.unlockBoot()
	if s.closed {
		return false, s.currentWireGen(), "", false, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "ssh transport closed"}
	}
	if expectedSock != "" {
		s.wireMu.Lock()
		curSock := s.controlSock
		gen := s.wireGen
		s.wireMu.Unlock()
		if curSock != expectedSock {
			return false, gen, "", false, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH control master is no longer active"}
		}
	}
	s.wireMu.Lock()
	s.envChanged = false
	s.wireMu.Unlock()
	err := s.startWorkerLocked(ctx)
	s.wireMu.Lock()
	hasWire := s.wire != nil
	gen := s.wireGen
	sock := s.controlSock
	s.wireMu.Unlock()
	return hasWire, gen, sock, true, err
}

// ensureMasterAndAttachArgs serializes first/post-Reconnect master establishment and pinned
// attachment argument selection under bootSem so concurrent direct calls, Reconnect, and Close
// can never race to open an extra master or select unpinned arguments.
func (s *SSH) ensureMasterAndAttachArgs(ctx context.Context) ([]string, error) {
	if err := s.lockBoot(ctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("command on %s stopped after the timeout: %w", s.Machine, err)
		}
		return nil, err
	}
	defer s.unlockBoot()
	if s.closed {
		return nil, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "ssh transport closed"}
	}
	s.wireMu.Lock()
	dead := s.masterDead
	pinned := s.masterPinned
	pooled := s.pooled
	sock := s.controlSock
	sendEnv := s.sendEnv
	setEnv := s.setEnv
	s.wireMu.Unlock()
	if dead {
		return nil, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH control master is not active"}
	}
	if !pinned {
		var err error
		if pooled {
			err = s.ensurePooledMasterLocked(ctx)
		} else {
			err = s.ensureDirectMasterLocked(ctx)
		}
		if err != nil {
			return nil, err
		}
		s.wireMu.Lock()
		dead = s.masterDead
		pinned = s.masterPinned
		sock = s.controlSock
		sendEnv = s.sendEnv
		setEnv = s.setEnv
		s.wireMu.Unlock()
		if dead || !pinned || sock == "" {
			return nil, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH control master is not active"}
		}
	}
	if sock == "" {
		return nil, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH control master is not active"}
	}
	return attachArgs(sock, sendEnv, setEnv), nil
}

// ensureDirectMasterLocked opens and pins a unique-socket ControlMaster while holding bootSem.
func (s *SSH) ensureDirectMasterLocked(ctx context.Context) error {
	s.wireMu.Lock()
	pinnedKey := s.pinnedKey
	unreusable := s.unreusable
	s.wireMu.Unlock()

	if unreusable {
		s.wireMu.Lock()
		s.masterPinned = true
		s.masterDead = true
		s.wireMu.Unlock()
		return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH destination identity could not be verified; re-bootstrap required"}
	}
	if pinnedKey != "" {
		curKey, ok := ControlSocketKeyCtx(ctx, s.Target, s.ConfigFile)
		if !ok {
			if ctx.Err() != nil {
				return fmt.Errorf("command on %s stopped after the timeout: %w", s.Machine, ctx.Err())
			}
			s.wireMu.Lock()
			s.masterPinned = true
			s.masterDead = true
			s.wireMu.Unlock()
			return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH destination configuration changed during run; re-bootstrap required"}
		}
		if curKey != pinnedKey {
			s.wireMu.Lock()
			s.masterPinned = true
			s.masterDead = true
			s.wireMu.Unlock()
			return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH destination configuration changed during run; re-bootstrap required"}
		}
	} else {
		// First direct command without a prior BootstrapSpec: resolve identity and SendEnv/SetEnv.
		key, sendEnv, setEnv, ok := ControlSocketProbeCtx(ctx, s.Target, s.ConfigFile)
		if !ok {
			if ctx.Err() != nil {
				return fmt.Errorf("command on %s stopped after the timeout: %w", s.Machine, ctx.Err())
			}
			var envOK bool
			sendEnv, setEnv, envOK = resolveEnvForCommand(ctx, s.Target, s.ConfigFile, "sh -c true")
			if !envOK {
				if ctx.Err() != nil {
					return fmt.Errorf("command on %s stopped after the timeout: %w", s.Machine, ctx.Err())
				}
				return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "failed to resolve SSH environment forwarding configuration"}
			}
		}
		s.wireMu.Lock()
		s.sendEnv = sendEnv
		s.setEnv = setEnv
		if ok {
			s.pinnedKey = key
			s.unreusable = false
		} else {
			s.pinnedKey = ""
			s.unreusable = true
		}
		s.wireMu.Unlock()
	}

	dir, err := controlDir()
	if err != nil {
		return err
	}
	sock := uniqueControlSock(dir)
	s.wireMu.Lock()
	s.controlSock = sock
	s.wireMu.Unlock()

	args := append(s.masterArgsWithSock(sock), "-T", s.Target, "--", "sh -c true")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 200 * time.Millisecond
	if err := cmd.Run(); err != nil {
		_ = s.cleanupPartialMaster()
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 255 {
			return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: stderr.String()}
		}
		if ctx.Err() != nil {
			return fmt.Errorf("command on %s stopped after the timeout: %w", s.Machine, ctx.Err())
		}
		return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: stderr.String()}
	}
	s.wireMu.Lock()
	s.masterPinned = true
	s.masterDead = false
	s.wireMu.Unlock()
	return nil
}

// ensurePooledMasterLocked re-establishes a daemon-owned ControlMaster + worker after an explicit
// pooled Reconnect while holding bootSem, verifying pinnedKey before contacting the pool.
func (s *SSH) ensurePooledMasterLocked(ctx context.Context) error {
	s.wireMu.Lock()
	pinnedKey := s.pinnedKey
	unreusable := s.unreusable
	s.wireMu.Unlock()
	if s.Pool == nil {
		return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH control master is not active"}
	}
	if unreusable || pinnedKey == "" {
		s.wireMu.Lock()
		s.wire = nil
		s.masterPinned = true
		s.masterDead = true
		s.wireMu.Unlock()
		return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH destination identity could not be verified; re-bootstrap required"}
	}
	curKey, ok := ControlSocketKeyCtx(ctx, s.Target, s.ConfigFile)
	if !ok {
		if ctx.Err() != nil {
			return fmt.Errorf("command on %s stopped after the timeout: %w", s.Machine, ctx.Err())
		}
		s.wireMu.Lock()
		s.wire = nil
		s.masterPinned = true
		s.masterDead = true
		s.wireMu.Unlock()
		return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH destination configuration changed during run; re-bootstrap required"}
	}
	if curKey != pinnedKey {
		s.wireMu.Lock()
		s.wire = nil
		s.masterPinned = true
		s.masterDead = true
		s.wireMu.Unlock()
		return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH destination configuration changed during run; re-bootstrap required"}
	}
	w, sock, ok, err := s.Pool.Refresh(ctx, s.Machine, s.Target, s.ConfigFile, pinnedKey, "")
	if err != nil {
		s.wireMu.Lock()
		s.wire = nil
		s.masterPinned = true
		s.masterDead = true
		s.wireMu.Unlock()
		if !IsConnError(err) && ctx.Err() != nil {
			return fmt.Errorf("command on %s stopped after the timeout: %w", s.Machine, ctx.Err())
		}
		return err
	}
	if sock == "" {
		s.wireMu.Lock()
		s.wire = nil
		s.masterPinned = true
		s.masterDead = true
		s.wireMu.Unlock()
		return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "daemon refresh returned no control socket"}
	}
	s.wireMu.Lock()
	s.wire = w
	s.wireGen++
	s.hasPy3 = ok
	s.controlSock = sock
	s.masterPinned = true
	s.masterDead = false
	s.wireMu.Unlock()
	return nil
}

// RefreshSession implements SessionRefresher. It closes the current persistent worker channel
// and opens a fresh one on the shared SSH connection so subsequent commands inherit updated
// login environment files (~/.bashrc, ~/.config/fam/env.sh, secrets.env) without stale vars.
func (s *SSH) RefreshSession(ctx context.Context) error {
	if err := s.lockBoot(ctx); err != nil {
		return err
	}
	defer s.unlockBoot()
	if s.closed {
		return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "ssh transport closed"}
	}

	s.wireMu.Lock()
	dead := s.masterDead
	hasPy3 := s.hasPy3
	pooled := s.pooled
	pinnedKey := s.pinnedKey
	unreusable := s.unreusable
	pinned := s.masterPinned
	curSock := s.controlSock
	s.envChanged = false
	s.wireMu.Unlock()
	if dead {
		return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH control master is not active"}
	}
	if !hasPy3 {
		return nil
	}
	// Never split one active transport across two hosts if SSH config changed mid-run.
	if !unreusable && pinnedKey != "" {
		curKey, ok := ControlSocketKeyCtx(ctx, s.Target, s.ConfigFile)
		if !ok {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !pooled {
				s.closeWire()
			}
			s.wireMu.Lock()
			s.wire = nil
			s.masterPinned = true
			s.masterDead = true
			s.wireMu.Unlock()
			return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH destination configuration changed during run; re-bootstrap required"}
		}
		if curKey != pinnedKey {
			if !pooled {
				s.closeWire()
			}
			// Preserve s.controlSock so a subsequent Reconnect targets the exact last pinned
			// socket instead of sending an empty teardown request; mark masterDead to fail closed.
			s.wireMu.Lock()
			s.wire = nil
			s.masterPinned = true
			s.masterDead = true
			s.wireMu.Unlock()
			return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH destination configuration changed during run; re-bootstrap required"}
		}
	}
	if pooled && s.Pool != nil {
		if pinned && curSock == "" {
			s.wireMu.Lock()
			s.wire = nil
			s.masterDead = true
			s.wireMu.Unlock()
			return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH control master is not active"}
		}
		expectedSock := ""
		if pinned {
			expectedSock = curSock
		}
		w, sock, ok, err := s.Pool.Refresh(ctx, s.Machine, s.Target, s.ConfigFile, pinnedKey, expectedSock)
		if err != nil {
			// Always invalidate the client's old wire and fail closed on any refresh failure
			// while preserving s.controlSock so a subsequent Reconnect targets the exact master.
			s.wireMu.Lock()
			s.wire = nil
			s.masterPinned = true
			s.masterDead = true
			s.wireMu.Unlock()
			return err
		}
		if sock == "" {
			s.wireMu.Lock()
			s.wire = nil
			s.masterPinned = true
			s.masterDead = true
			s.wireMu.Unlock()
			return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "daemon refresh returned no control socket"}
		}
		s.wireMu.Lock()
		s.wire = w
		s.wireGen++
		s.hasPy3 = ok
		s.controlSock = sock
		s.masterPinned = true
		s.masterDead = false
		s.wireMu.Unlock()
		return nil
	}
	if err := s.startWorkerLocked(ctx); err != nil {
		// Poison the local transport while preserving s.controlSock for teardown on Reconnect/Close.
		s.wireMu.Lock()
		s.wire = nil
		s.masterPinned = true
		s.masterDead = true
		s.wireMu.Unlock()
		return err
	}
	return nil
}

func (s *SSH) startWorkerLocked(ctx context.Context) error {
	s.closeWire()
	s.wireMu.Lock()
	wasPinned := s.masterPinned
	s.wireMu.Unlock()
	proc, err := s.startSSHProcess(ctx, wireLaunchSnippet())
	if err != nil {
		if !wasPinned {
			_ = s.cleanupPartialMaster()
		}
		return err
	}
	wireMode, waited, err := s.readWireHandshake(ctx, proc, time.Now())
	if err != nil {
		if !wasPinned {
			_ = s.cleanupPartialMaster()
		}
		return err
	}
	s.wireMu.Lock()
	s.masterPinned = true
	s.wireMu.Unlock()
	if wireMode == "FAM-WIRE-PY3" {
		w := newSSHWire(s.Machine, s.Target, proc)
		w.start(proc.br, proc.errBuf)
		s.installWire(w, nil, true)
		return nil
	}
	if !waited {
		_ = proc.stdin.Close()
		_ = proc.cmd.Wait()
	}
	s.installWire(nil, nil, false)
	return nil
}
