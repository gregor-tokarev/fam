package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gregor-tokarev/fam/internal/transport"
)

const idleTimeout = 60 * time.Second

type pooledSSH struct {
	key       string
	target    string
	sshConfig string
	ssh       *transport.SSH
}

type server struct {
	sockPath string
	watchDir string
	ln       net.Listener
	sockInfo os.FileInfo
	runLock  *os.File

	mu           sync.Mutex
	sessions     map[string]*pooledSSH
	staleClosers []*transport.SSH // evicted transports whose ControlMaster exit failed; retried by watchdog/shutdown
	conns        map[net.Conn]struct{}
	targetLocks  map[string]*sync.Mutex
	lastUsed     atomic.Int64
	inFlight     atomic.Int64
	stopOnce     sync.Once
	stopCh       chan struct{}
	serveCancel  context.CancelFunc
	watchdogDone chan struct{}
	connWG       sync.WaitGroup
}

// RunServer starts the local warm daemon listening on sockPath and blocks until idle timeout,
// explicit stop, watchDir disappearance, or context cancellation.
func RunServer(ctx context.Context, sockPath, watchDir string) error {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return err
	}
	// Coordinate sole ownership of sockPath across processes so two concurrent daemon startups
	// can never overwrite or unlink each other's live socket.
	runLock, err := os.OpenFile(sockPath+".runlock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	lockDeadline := time.Now().Add(400 * time.Millisecond)
	for {
		err = syscall.Flock(int(runLock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = runLock.Close()
			return err
		}
		if ping(sockPath) || time.Now().After(lockDeadline) {
			_ = runLock.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}

	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = syscall.Flock(int(runLock.Fd()), syscall.LOCK_UN)
		_ = runLock.Close()
		return err
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	_ = os.Chmod(sockPath, 0o600)
	sockInfo, _ := os.Lstat(sockPath)

	serveCtx, serveCancel := context.WithCancel(ctx)
	s := &server{
		sockPath:     sockPath,
		watchDir:     watchDir,
		ln:           ln,
		sockInfo:     sockInfo,
		runLock:      runLock,
		sessions:     map[string]*pooledSSH{},
		conns:        map[net.Conn]struct{}{},
		targetLocks:  map[string]*sync.Mutex{},
		stopCh:       make(chan struct{}),
		serveCancel:  serveCancel,
		watchdogDone: make(chan struct{}),
	}
	s.touch()
	defer s.shutdown()

	go s.watchdog(serveCtx)

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return nil
			default:
				return err
			}
		}
		s.connWG.Add(1)
		go func() {
			defer s.connWG.Done()
			s.handleConn(serveCtx, conn)
		}()
	}
}

func (s *server) touch() {
	s.lastUsed.Store(time.Now().UnixNano())
}

func (s *server) stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		if s.serveCancel != nil {
			s.serveCancel()
		}
		if s.ln != nil {
			_ = s.ln.Close()
		}
		s.mu.Lock()
		active := make([]net.Conn, 0, len(s.conns))
		for c := range s.conns {
			active = append(active, c)
		}
		s.conns = nil
		s.mu.Unlock()
		for _, c := range active {
			_ = c.Close()
		}
	})
}

func (s *server) addStaleCloseLocked(ssh *transport.SSH) {
	if ssh == nil {
		return
	}
	for _, existing := range s.staleClosers {
		if existing == ssh {
			return
		}
	}
	s.staleClosers = append(s.staleClosers, ssh)
}

func (s *server) retryStaleCloses() {
	s.mu.Lock()
	if len(s.staleClosers) == 0 {
		s.mu.Unlock()
		return
	}
	pending := s.staleClosers
	s.staleClosers = nil
	s.mu.Unlock()

	var remaining []*transport.SSH
	for _, ssh := range pending {
		_ = ssh.Close()
		if ssh.ControlSock() != "" {
			remaining = append(remaining, ssh)
		}
	}
	if len(remaining) > 0 {
		s.mu.Lock()
		for _, ssh := range remaining {
			s.addStaleCloseLocked(ssh)
		}
		s.mu.Unlock()
	}
}

func (s *server) shutdown() {
	s.stop()
	// Join watchdog and all active connection handlers before the final drain so neither a
	// watchdog retry nor an in-flight eviction can requeue a master after shutdown drains.
	if s.watchdogDone != nil {
		<-s.watchdogDone
	}
	s.connWG.Wait()

	s.mu.Lock()
	all := make([]*transport.SSH, 0, len(s.sessions)+len(s.staleClosers))
	for k, p := range s.sessions {
		all = append(all, p.ssh)
		delete(s.sessions, k)
	}
	all = append(all, s.staleClosers...)
	s.staleClosers = nil
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, ssh := range all {
		ssh := ssh
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ssh.Close()
		}()
	}
	wg.Wait()
	// Unlink only if sockPath still points to the exact socket inode this daemon created.
	if s.sockInfo != nil {
		if curInfo, err := os.Lstat(s.sockPath); err == nil && os.SameFile(s.sockInfo, curInfo) {
			_ = os.Remove(s.sockPath)
		}
	}
	if s.runLock != nil {
		_ = syscall.Flock(int(s.runLock.Fd()), syscall.LOCK_UN)
		_ = s.runLock.Close()
	}
}

func (s *server) watchdog(ctx context.Context) {
	if s.watchdogDone != nil {
		defer close(s.watchdogDone)
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stop()
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.retryStaleCloses()
			select {
			case <-s.stopCh:
				return
			default:
			}
			if s.watchDir != "" {
				if _, err := os.Stat(s.watchDir); err != nil {
					s.stop()
					return
				}
			}
			if curInfo, err := os.Lstat(s.sockPath); err != nil || (s.sockInfo != nil && !os.SameFile(s.sockInfo, curInfo)) {
				s.stop()
				return
			}
			if s.inFlight.Load() == 0 && time.Since(time.Unix(0, s.lastUsed.Load())) > idleTimeout {
				s.stop()
				return
			}
		}
	}
}

func (s *server) targetLock(sockKey string) *sync.Mutex {
	s.mu.Lock()
	m := s.targetLocks[sockKey]
	if m == nil {
		m = &sync.Mutex{}
		s.targetLocks[sockKey] = m
	}
	s.mu.Unlock()
	return m
}

// getOrCreateByKey returns the single pooled SSH session for a pre-resolved socket key k,
// pinning k and its effective SendEnv/SetEnv onto the transport so it never re-resolves or drifts.
func (s *server) getOrCreateByKey(machine, target, sshConfig, k string, sendEnv, setEnv []string) *pooledSSH {
	s.mu.Lock()
	p := s.sessions[k]
	s.mu.Unlock()
	if p != nil {
		return p
	}
	tl := s.targetLock(k)
	tl.Lock()
	defer tl.Unlock()
	s.mu.Lock()
	p = s.sessions[k]
	if p == nil {
		ssh := transport.NewSSH(machine, target, sshConfig, nil)
		ssh.PinSocketIdentity(k, sendEnv, setEnv)
		p = &pooledSSH{
			key:       k,
			target:    target,
			sshConfig: sshConfig,
			ssh:       ssh,
		}
		s.sessions[k] = p
	}
	s.mu.Unlock()
	return p
}

func (s *server) getOrCreate(machine, target, sshConfig string) (*pooledSSH, bool) {
	k, sendEnv, setEnv, ok := transport.ControlSocketProbe(target, sshConfig)
	if !ok {
		return nil, false
	}
	return s.getOrCreateByKey(machine, target, sshConfig, k, sendEnv, setEnv), true
}

func (s *server) lookupByKey(k string) *pooledSSH {
	s.mu.Lock()
	p := s.sessions[k]
	s.mu.Unlock()
	return p
}

// reconnectAndEvict atomically verifies expectedSock and closes p under p.ssh's bootMu (serialized
// under targetLock) so a concurrent bootstrap cannot rotate the master before teardown, and an
// empty or mismatched expectedSock never tears down a live session.
func (s *server) reconnectAndEvict(ctx context.Context, p *pooledSSH, expectedSock string) error {
	if p == nil || expectedSock == "" {
		return nil
	}
	tl := s.targetLock(p.key)
	tl.Lock()
	defer tl.Unlock()
	s.mu.Lock()
	owned := s.sessions[p.key] == p
	s.mu.Unlock()
	if !owned {
		return nil
	}
	matched, err := p.ssh.ReconnectIfSock(ctx, expectedSock)
	if !matched {
		return nil
	}
	needsRetry := p.ssh.ControlSock() != ""
	s.mu.Lock()
	if s.sessions[p.key] == p {
		delete(s.sessions, p.key)
	}
	if needsRetry {
		s.addStaleCloseLocked(p.ssh)
	}
	s.mu.Unlock()
	return err
}

// evictIfGen removes and closes p only if p still owns its entry AND its current wire
// generation still equals gen, so a late error from an older wire never tears down a session
// that a concurrent refresh or bootstrap has already restored.
func (s *server) evictIfGen(p *pooledSSH, gen uint64) {
	if p == nil {
		return
	}
	tl := s.targetLock(p.key)
	tl.Lock()
	defer tl.Unlock()
	s.mu.Lock()
	owned := s.sessions[p.key] == p
	s.mu.Unlock()
	if !owned {
		return
	}
	if p.ssh.CloseIfGen(gen) {
		needsRetry := p.ssh.ControlSock() != ""
		s.mu.Lock()
		if s.sessions[p.key] == p {
			delete(s.sessions, p.key)
		}
		if needsRetry {
			s.addStaleCloseLocked(p.ssh)
		}
		s.mu.Unlock()
	}
}

func fillErr(resp *rpcResp, err error) {
	if err == nil {
		resp.OK = true
		return
	}
	resp.Err = err.Error()
	var ce *transport.ConnError
	if errors.As(err, &ce) {
		resp.ConnErr = true
		resp.ConnStderr = ce.Stderr
	}
}

func (s *server) handleConn(parentCtx context.Context, conn net.Conn) {
	s.mu.Lock()
	select {
	case <-s.stopCh:
		s.mu.Unlock()
		_ = conn.Close()
		return
	default:
		if s.conns == nil {
			s.conns = map[net.Conn]struct{}{}
		}
		s.conns[conn] = struct{}{}
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()
	s.inFlight.Add(1)
	defer func() {
		s.touch()
		s.inFlight.Add(-1)
	}()

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	br := bufio.NewReaderSize(conn, 64*1024)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	s.touch()

	var req rpcReq
	if err := json.Unmarshal(line, &req); err != nil {
		writeResp(conn, rpcResp{Err: fmt.Sprintf("decode rpc: %v", err)})
		return
	}

	// Watch for client disconnect (cancellation or lease release).
	doneReading := make(chan struct{})
	go func() {
		defer close(doneReading)
		var b [1]byte
		_, _ = br.Read(b[:])
		cancel()
	}()

	switch req.Op {
	case "ping":
		writeResp(conn, rpcResp{OK: true})
		return
	case "lease":
		// Hold open (keeping inFlight > 0) for the lifetime of the client Transport so the 60s
		// idle timer only starts after the last active client closes or exits.
		writeResp(conn, rpcResp{OK: true})
		select {
		case <-doneReading:
		case <-s.stopCh:
		}
		return
	case "stop":
		writeResp(conn, rpcResp{OK: true})
		go s.stop()
		return
	}

	var ps *pooledSSH
	switch req.Op {
	case "wire", "wire_stream":
		if req.SessionKey != "" {
			ps = s.lookupByKey(req.SessionKey)
		} else {
			if p, ok := s.getOrCreate(req.Machine, req.Target, req.SSHConfig); ok {
				ps = p
			}
		}
		if ps == nil {
			ce := &transport.ConnError{Machine: req.Machine, Target: req.Target, Stderr: "remote worker is not active"}
			resp := rpcResp{Done: req.Op == "wire_stream"}
			fillErr(&resp, ce)
			writeResp(conn, resp)
			return
		}
	case "refresh":
		curKey, sendEnv, setEnv, ok := transport.ControlSocketProbeCtx(ctx, req.Target, req.SSHConfig)
		if !ok || req.SessionKey == "" || curKey != req.SessionKey {
			ce := &transport.ConnError{Machine: req.Machine, Target: req.Target, Stderr: "SSH destination configuration changed during run; re-bootstrap required"}
			resp := rpcResp{IdentityErr: true}
			fillErr(&resp, ce)
			writeResp(conn, resp)
			return
		}
		if req.ControlSock != "" {
			// Caller is refreshing an already-pinned master: never silently create or switch
			// to a replacement master if the pinned one is gone.
			ps = s.lookupByKey(curKey)
			if ps == nil {
				ce := &transport.ConnError{Machine: req.Machine, Target: req.Target, Stderr: "SSH control master is no longer active"}
				var resp rpcResp
				fillErr(&resp, ce)
				writeResp(conn, resp)
				return
			}
		} else {
			ps = s.getOrCreateByKey(req.Machine, req.Target, req.SSHConfig, curKey, sendEnv, setEnv)
		}
	case "reconnect":
		// Reject empty teardown requests so a stale/unpinned client cannot tear down another master.
		if req.ControlSock == "" {
			writeResp(conn, rpcResp{OK: true})
			return
		}
		if req.SessionKey != "" {
			ps = s.lookupByKey(req.SessionKey)
			if ps == nil {
				writeResp(conn, rpcResp{OK: true})
				return
			}
		} else {
			writeResp(conn, rpcResp{OK: true})
			return
		}
	case "bootstrap":
		daemonKey, sendEnv, setEnv, ok := transport.ControlSocketProbeCtx(ctx, req.Target, req.SSHConfig)
		if !ok {
			writeResp(conn, rpcResp{IdentityErr: true, Err: "ssh identity unavailable; bypassing pool"})
			return
		}
		if req.SessionKey == "" || daemonKey != req.SessionKey {
			writeResp(conn, rpcResp{IdentityErr: true, Err: "ssh identity mismatch between client and daemon"})
			return
		}
		ps = s.getOrCreateByKey(req.Machine, req.Target, req.SSHConfig, daemonKey, sendEnv, setEnv)
	default:
		p, ok := s.getOrCreate(req.Machine, req.Target, req.SSHConfig)
		if !ok {
			writeResp(conn, rpcResp{IdentityErr: true, Err: "ssh identity unavailable; bypassing pool"})
			return
		}
		ps = p
	}
	ssh := ps.ssh

	switch req.Op {
	case "bootstrap":
		res, gen, err := ssh.WarmBootstrap(ctx, req.FactsScript, req.SecretsRel, req.ExpectedFactsSHA, req.SpecReqs)
		if transport.IsConnError(err) || ssh.IsMasterDead() {
			s.evictIfGen(ps, gen)
		}
		resp := rpcResp{
			FactsRes:      res.FactsRes,
			SecretsData:   res.SecretsData,
			SecretsExists: res.SecretsExists,
			SpecHit:       res.SpecHit,
			SpecResults:   res.SpecResults,
			HasWire:       res.HasWire,
			SessionKey:    ps.key,
			ControlSock:   res.ControlSock,
		}
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "wire":
		if req.Wire == nil {
			writeResp(conn, rpcResp{Err: "missing wire request"})
			return
		}
		wr, gen, evict, err := ssh.WireCall(ctx, *req.Wire, req.ControlSock)
		if evict && transport.IsConnError(err) {
			s.evictIfGen(ps, gen)
		}
		resp := rpcResp{Wire: &wr}
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "wire_stream":
		if req.Wire == nil {
			writeResp(conn, rpcResp{Done: true, Err: "missing wire request"})
			return
		}
		var wmu sync.Mutex
		gen, evict, err := ssh.WireCallStream(ctx, *req.Wire, req.ControlSock, func(wr transport.WireResp) {
			wmu.Lock()
			writeResp(conn, rpcResp{Wire: &wr})
			wmu.Unlock()
		})
		if evict && transport.IsConnError(err) {
			s.evictIfGen(ps, gen)
		}
		resp := rpcResp{Done: true}
		fillErr(&resp, err)
		wmu.Lock()
		writeResp(conn, resp)
		wmu.Unlock()

	case "refresh":
		hasWire, gen, sock, evict, err := ssh.EnsureWorker(ctx, req.ControlSock)
		if evict && (transport.IsConnError(err) || ssh.IsMasterDead()) {
			s.evictIfGen(ps, gen)
		}
		resp := rpcResp{HasWire: hasWire, SessionKey: ps.key, ControlSock: sock}
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "reconnect":
		err := s.reconnectAndEvict(ctx, ps, req.ControlSock)
		var resp rpcResp
		fillErr(&resp, err)
		writeResp(conn, resp)

	default:
		writeResp(conn, rpcResp{Err: "unknown daemon op: " + req.Op})
	}
}

func writeResp(conn net.Conn, resp rpcResp) {
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	b = append(b, '\n')
	_, _ = conn.Write(b)
}
