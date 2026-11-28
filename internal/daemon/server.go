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
	"time"

	"github.com/gregor-tokarev/fam/internal/transport"
)

const idleTimeout = 60 * time.Second

type pooledSSH struct {
	key string
	ssh *transport.SSH
}

type server struct {
	sockPath string
	watchDir string
	ln       net.Listener

	mu       sync.Mutex
	sessions map[string]*pooledSSH
	lastUsed atomic.Int64
	inFlight atomic.Int64
	stopOnce sync.Once
	stopCh   chan struct{}
}

// RunServer starts the local warm daemon listening on sockPath and blocks until idle timeout,
// explicit stop, watchDir disappearance, or context cancellation.
func RunServer(ctx context.Context, sockPath, watchDir string) error {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return err
	}
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(sockPath, 0o600)

	s := &server{
		sockPath: sockPath,
		watchDir: watchDir,
		ln:       ln,
		sessions: map[string]*pooledSSH{},
		stopCh:   make(chan struct{}),
	}
	s.touch()
	defer s.shutdown()

	go s.watchdog(ctx)

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
		go s.handleConn(ctx, conn)
	}
}

func (s *server) touch() {
	s.lastUsed.Store(time.Now().UnixNano())
}

func (s *server) stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		_ = s.ln.Close()
	})
}

func (s *server) shutdown() {
	s.stop()
	s.mu.Lock()
	all := make([]*pooledSSH, 0, len(s.sessions))
	for k, p := range s.sessions {
		all = append(all, p)
		delete(s.sessions, k)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range all {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.ssh.Close()
		}()
	}
	wg.Wait()
	_ = os.Remove(s.sockPath)
}

func (s *server) watchdog(ctx context.Context) {
	sockInfo, _ := os.Lstat(s.sockPath)
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
			if s.watchDir != "" {
				if _, err := os.Stat(s.watchDir); err != nil {
					s.stop()
					return
				}
			}
			if curInfo, err := os.Lstat(s.sockPath); err != nil || (sockInfo != nil && !os.SameFile(sockInfo, curInfo)) {
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

func sessionKey(target, sshConfig string) string {
	cfgPath := sshConfig
	if cfgPath == "" {
		if h, err := os.UserHomeDir(); err == nil {
			cfgPath = filepath.Join(h, ".ssh", "config")
		}
	}
	stamp := ""
	if cfgPath != "" {
		if info, err := os.Stat(cfgPath); err == nil {
			stamp = fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
		}
	}
	return target + "\x00" + sshConfig + "\x00" + stamp
}

func (s *server) getOrCreate(machine, target, sshConfig, osName string) *transport.SSH {
	k := sessionKey(target, sshConfig)
	s.mu.Lock()
	p := s.sessions[k]
	if p == nil {
		p = &pooledSSH{
			key: k,
			ssh: transport.NewSSH(machine, target, sshConfig, nil),
		}
		s.sessions[k] = p
	}
	if osName != "" {
		p.ssh.SetOS(osName)
	}
	s.mu.Unlock()
	return p.ssh
}

func (s *server) evict(target, sshConfig string) {
	k := sessionKey(target, sshConfig)
	s.mu.Lock()
	p := s.sessions[k]
	delete(s.sessions, k)
	s.mu.Unlock()
	if p != nil {
		go func() { _ = p.ssh.Close() }()
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
	if transport.IsSkillFallback(err) {
		resp.SkillFallbk = true
	}
}

func (s *server) handleConn(parentCtx context.Context, conn net.Conn) {
	defer conn.Close()
	s.inFlight.Add(1)
	defer func() {
		s.touch()
		s.inFlight.Add(-1)
	}()

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	br := bufio.NewReaderSize(conn, 64*1024)
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	s.touch()

	var req rpcReq
	if err := json.Unmarshal(line, &req); err != nil {
		writeResp(conn, rpcResp{Err: fmt.Sprintf("decode rpc: %v", err)})
		return
	}

	// Watch for client disconnect (cancellation) while request runs.
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
	case "stop":
		writeResp(conn, rpcResp{OK: true})
		go s.stop()
		return
	}

	ssh := s.getOrCreate(req.Machine, req.Target, req.SSHConfig, req.OS)

	switch req.Op {
	case "bootstrap":
		factsRes, secData, secExists, specHit, specResults, err := ssh.WarmBootstrap(
			ctx, req.FactsScript, req.SecretsRel, req.ExpectedFactsSHA, req.SpecReqs,
		)
		if transport.IsConnError(err) {
			s.evict(req.Target, req.SSHConfig)
		}
		resp := rpcResp{
			FactsRes:      factsRes,
			SecretsData:   secData,
			SecretsExists: secExists,
			SpecHit:       specHit,
			SpecResults:   specResults,
			SupportsSeq:   ssh.SupportsApplySeq(),
			MaxConcur:     ssh.MaxConcurrency(),
			SHA256Cmd:     ssh.SHA256Cmd(),
		}
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "run":
		res, err := ssh.Run(ctx, transport.Cmd{
			Script:      req.Script,
			Stdin:       req.Stdin,
			Sudo:        req.Sudo,
			LoginShell:  req.LoginShell,
			Interactive: req.Interactive,
			Timeout:     time.Duration(req.TimeoutMS) * time.Millisecond,
			Label:       req.Label,
			Secret:      req.Secret,
		})
		if transport.IsConnError(err) {
			s.evict(req.Target, req.SSHConfig)
		}
		resp := rpcResp{Result: res}
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "stat":
		fi, err := ssh.Stat(ctx, req.Path, req.Sudo)
		if transport.IsConnError(err) {
			s.evict(req.Target, req.SSHConfig)
		}
		resp := rpcResp{FileInfo: fi}
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "read":
		data, fi, err := ssh.ReadFileInfo(ctx, req.Path, req.Sudo)
		if transport.IsConnError(err) {
			s.evict(req.Target, req.SSHConfig)
		}
		resp := rpcResp{Data: data, FileInfo: fi, Exists: fi.Exists}
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "write":
		err := ssh.WriteFile(ctx, req.Path, req.Data, req.Mode, req.Sudo)
		if transport.IsConnError(err) {
			s.evict(req.Target, req.SSHConfig)
		}
		var resp rpcResp
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "write_verify":
		fi, err := ssh.WriteFileVerify(ctx, req.Path, req.Data, req.Mode, req.Sudo)
		if transport.IsConnError(err) {
			s.evict(req.Target, req.SSHConfig)
		}
		resp := rpcResp{FileInfo: fi}
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "skill_check":
		st, err := ssh.SkillCheck(ctx, req.Dir, req.Marker)
		if transport.IsConnError(err) {
			s.evict(req.Target, req.SSHConfig)
		}
		resp := rpcResp{SkillState: st}
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "skill_write":
		err := ssh.SkillWrite(ctx, req.Dir, req.Marker, req.Data)
		if transport.IsConnError(err) {
			s.evict(req.Target, req.SSHConfig)
		}
		var resp rpcResp
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "apply_seq":
		var wmu sync.Mutex
		err := ssh.ApplySeq(ctx, req.Ops, func(it transport.ApplyOpResult) {
			wmu.Lock()
			writeResp(conn, rpcResp{Item: &it})
			wmu.Unlock()
		})
		if transport.IsConnError(err) {
			s.evict(req.Target, req.SSHConfig)
		}
		resp := rpcResp{Done: true, SupportsSeq: ssh.SupportsApplySeq()}
		fillErr(&resp, err)
		wmu.Lock()
		writeResp(conn, resp)
		wmu.Unlock()

	case "refresh":
		var err error
		if req.IfNeeded {
			err = ssh.RefreshSessionIfNeeded(ctx)
		} else {
			err = ssh.RefreshSession(ctx)
		}
		if transport.IsConnError(err) {
			s.evict(req.Target, req.SSHConfig)
		}
		resp := rpcResp{SupportsSeq: ssh.SupportsApplySeq(), MaxConcur: ssh.MaxConcurrency()}
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "reconnect":
		err := ssh.Reconnect(ctx)
		s.evict(req.Target, req.SSHConfig)
		var resp rpcResp
		fillErr(&resp, err)
		writeResp(conn, resp)

	case "set_os":
		ssh.SetOS(req.OS)
		writeResp(conn, rpcResp{OK: true, SHA256Cmd: ssh.SHA256Cmd()})

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
