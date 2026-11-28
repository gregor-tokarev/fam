package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SSH runs commands on a worker with the system `ssh` command and ControlMaster connection reuse.
// When python3 >= 3.6 is available on the worker, SSH keeps a single persistent multiplexed
// `ssh -T` worker process alive for the run (or relays to one held by SessionPool) to avoid
// per-command SSH channel handshakes and forks.
type SSH struct {
	Machine string
	// Target is `user@address` or `address`.
	Target string
	// ConfigFile is given to `ssh -F` when it is not empty.
	ConfigFile     string
	ConnectTimeout time.Duration
	Observer       Observer
	// Pool optionally delegates persistent wire + ControlMaster reuse to the local daemon.
	Pool SessionPool
	files

	wireMu       sync.Mutex
	bootOnce     sync.Once
	bootSem      chan struct{} // cancellable mutex serializing bootstrap/refresh/reconnect/close
	closed       bool          // guarded by bootSem
	wire         WireConn
	wireGen      uint64   // monotonic generation bumped on every wire installation; guarded by wireMu
	pinnedKey    string   // resolved ControlSocketKey pinned for this run; guarded by wireMu
	unreusable   bool     // true when SSH identity could not be resolved; bypasses reuse; guarded by wireMu
	controlSock  string   // unique ControlPath socket opened or attached by this SSH; guarded by wireMu
	masterPinned bool     // true once controlSock has an active ControlMaster; attached calls fail closed
	masterDead   bool     // true after refresh/drift failure: fails closed while preserving controlSock for teardown
	sendEnv      []string // effective SendEnv patterns from ssh -G carried onto pinned attachments
	setEnv       []string // effective SetEnv entries from ssh -G carried onto pinned attachments
	poolLease    io.Closer
	pooled     bool
	hasPy3     bool
	specCache  *specResultCache
	envChanged bool
}

// NewSSH returns an SSH transport for one worker.
func NewSSH(machine, target, configFile string, obs Observer) *SSH {
	s := &SSH{Machine: machine, Target: target, ConfigFile: configFile, Observer: obs}
	s.files.run = s.Run
	return s
}

func (s *SSH) initBootLock() {
	s.bootOnce.Do(func() { s.bootSem = make(chan struct{}, 1) })
}

func (s *SSH) lockBoot(ctx context.Context) error {
	s.initBootLock()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.bootSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *SSH) lockBootBlocking() {
	s.initBootLock()
	s.bootSem <- struct{}{}
}

func (s *SSH) unlockBoot() {
	<-s.bootSem
}

func controlDir() (string, error) {
	// Keep the socket path short, including on macOS with a long TMPDIR.
	dir := filepath.Join("/tmp", fmt.Sprintf("fam-%d", os.Getuid()))
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("SSH control directory %s must be a directory, not a symlink", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0o700 {
		return "", fmt.Errorf("SSH control directory %s must belong to the current user and have mode 0700", dir)
	}
	return dir, nil
}

func sshG(ctx context.Context, target, configFile string, extraArgs []string, remoteCmd string) (stdout, stderr string, ok bool) {
	var args []string
	if configFile != "" {
		args = append(args, "-F", configFile)
	}
	args = append(args, extraArgs...)
	args = append(args, "-G", "--", target)
	if remoteCmd != "" {
		args = append(args, remoteCmd)
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 100 * time.Millisecond
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", "", false
	}
	return outBuf.String(), errBuf.String(), true
}

// extractMatchDebugMsg anchors `debug3: <file> line <N>: <msg>` against the exact config
// filenames reported by `debug1: Reading configuration data <file>` so filenames containing
// ` line <N>: ` or skipped `exec` payloads cannot confuse Match criterion parsing.
// Returns (msg, matched, ambiguous).
func extractMatchDebugMsg(line string, knownFiles []string) (msg string, matched bool, ambiguous bool) {
	rest, ok := strings.CutPrefix(line, "debug3: ")
	if !ok {
		return "", false, false
	}
	bestLen := -1
	bestMsg := ""
	for _, f := range knownFiles {
		prefix := f + " line "
		if len(f) <= bestLen || !strings.HasPrefix(rest, prefix) {
			continue
		}
		pos := len(prefix)
		end := pos
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end > pos && strings.HasPrefix(rest[end:], ": ") {
			bestLen = len(f)
			bestMsg = rest[end+2:]
		}
	}
	if bestLen >= 0 {
		return bestMsg, true, false
	}
	if strings.Contains(rest, " line ") {
		return "", false, true
	}
	return "", false, false
}

// hasCommandDependentMatch inspects `ssh -vvv -G` stderr and returns true if any evaluated
// Match block contained a `command` / `!command` criterion whose non-command criteria all
// matched (or had an ambiguous debug prefix), meaning effective SSH config can change depending
// on the remote command string.
func hasCommandDependentMatch(stderr string) bool {
	knownFiles := []string{"command-line"}
	inMatch := false
	hasCmd := false
	otherFailed := false
	ambiguous := false
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimRight(line, "\r")
		if path, ok := strings.CutPrefix(line, "debug1: Reading configuration data "); ok && path != "" {
			knownFiles = append(knownFiles, path)
			continue
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "debug2: checking match for ") {
			inMatch = true
			hasCmd = false
			otherFailed = false
			ambiguous = false
			continue
		}
		if !inMatch {
			continue
		}
		if trimmed == "debug2: match found" || trimmed == "debug2: match not found" {
			if (hasCmd && !otherFailed) || ambiguous {
				return true
			}
			inMatch = false
			continue
		}
		msg, ok, amb := extractMatchDebugMsg(line, knownFiles)
		if amb {
			ambiguous = true
			continue
		}
		if !ok {
			continue
		}
		if rest, ok := strings.CutPrefix(msg, "not matched '"); ok {
			lower := strings.ToLower(rest)
			if strings.HasPrefix(lower, "command ") || strings.HasPrefix(lower, "!command ") {
				hasCmd = true
			} else {
				otherFailed = true
			}
		} else if rest, ok := strings.CutPrefix(msg, "matched '"); ok {
			lower := strings.ToLower(rest)
			if strings.HasPrefix(lower, "command ") || strings.HasPrefix(lower, "!command ") {
				hasCmd = true
			}
		}
	}
	return false
}

type sshGInfo struct {
	filtered    string
	hostName    string
	port        string
	proxyCmd    string
	proxyJump   string
	controlPath string
	sendEnv     []string
	setEnv      []string
}

func filterSSHG(out string, dropProbeOnly bool) sshGInfo {
	var b strings.Builder
	var info sshGInfo
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "controlpath "); ok {
			if v = strings.TrimSpace(v); v != "" && !strings.EqualFold(v, "none") {
				info.controlPath = v
			}
			continue
		}
		if strings.HasPrefix(line, "controlmaster ") || strings.HasPrefix(line, "controlpersist ") {
			continue
		}
		if dropProbeOnly && (strings.HasPrefix(line, "requesttty ") || strings.HasPrefix(line, "loglevel ")) {
			continue
		}
		if v, ok := strings.CutPrefix(line, "hostname "); ok {
			info.hostName = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "port "); ok {
			info.port = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "sendenv "); ok {
			if v != "" {
				info.sendEnv = append(info.sendEnv, v)
			}
		}
		if v, ok := strings.CutPrefix(line, "setenv "); ok {
			if v != "" {
				info.setEnv = append(info.setEnv, v)
			}
		}
		if v, ok := strings.CutPrefix(line, "proxycommand "); ok {
			if v = strings.TrimSpace(v); v != "" && !strings.EqualFold(v, "none") {
				info.proxyCmd = v
			}
		}
		if v, ok := strings.CutPrefix(line, "proxyjump "); ok {
			if v = strings.TrimSpace(v); v != "" && !strings.EqualFold(v, "none") {
				info.proxyJump = v
			}
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	info.filtered = b.String()
	return info
}

func resolveEnvForCommand(ctx context.Context, target, configFile, remoteCmd string) ([]string, []string, bool) {
	out, _, ok := sshG(ctx, target, configFile, []string{"-T"}, remoteCmd)
	if !ok {
		return nil, nil, false
	}
	info := filterSSHG(out, false)
	return info.sendEnv, info.setEnv, true
}

func splitJumpHop(hop string) (target, port string) {
	hop = strings.TrimSuffix(strings.TrimPrefix(hop, "ssh://"), "/")
	userPrefix := ""
	hostPort := hop
	if at := strings.LastIndexByte(hop, '@'); at >= 0 {
		userPrefix = hop[:at+1]
		hostPort = hop[at+1:]
	}
	if strings.HasPrefix(hostPort, "[") {
		if end := strings.IndexByte(hostPort, ']'); end >= 0 {
			host := hostPort[1:end]
			rest := hostPort[end+1:]
			if strings.HasPrefix(rest, ":") {
				return userPrefix + host, rest[1:]
			}
			return userPrefix + host, ""
		}
	}
	if strings.Count(hostPort, ":") == 1 {
		i := strings.LastIndexByte(hostPort, ':')
		return userPrefix + hostPort[:i], hostPort[i+1:]
	}
	return userPrefix + hostPort, ""
}

// resolveSSHIdentity queries `ssh -G` to capture the effective resolved OpenSSH configuration
// (including Include files, Hostname, Port, User, IdentityFile, and every resolved ProxyJump hop
// using OpenSSH's exact `-p`, preceding `-J`, and `-W [host]:port` jump context), plus the
// top-level SendEnv/SetEnv forwarding patterns. Returns ok=false when resolution fails, when
// Match command/sessiontype makes config depend on the invocation context, or when encountering
// an unresolvable proxy route (custom ProxyCommand, jump ControlPath, or cycle/deep chain).
func resolveSSHIdentity(parentCtx context.Context, target, configFile string) (id string, sendEnv []string, setEnv []string, ok bool) {
	ctx, cancel := context.WithTimeout(parentCtx, 2*time.Second)
	defer cancel()
	h := sha256.New()
	visited := map[string]bool{}
	var topSendEnv, topSetEnv []string
	var walk func(spec, portOverride, prevJumps, forwardDest string, depth int) bool
	walk = func(spec, portOverride, prevJumps, forwardDest string, depth int) bool {
		visitKey := spec + "\x00" + portOverride + "\x00" + prevJumps + "\x00" + forwardDest
		if depth > 8 || visited[visitKey] {
			return false
		}
		visited[visitKey] = true

		var extra []string
		if prevJumps != "" {
			extra = append(extra, "-J", prevJumps)
		}
		if portOverride != "" {
			extra = append(extra, "-p", portOverride)
		}
		if forwardDest != "" {
			extra = append(extra, "-W", forwardDest)
		}

		var info sshGInfo
		if depth == 0 {
			// Probe both the worker launch command (`-E <log> -vvv -T` + `sh -c true`) and the
			// no-command baseline. Capturing parent diagnostics via `-E` isolates them from stderr
			// of any nested commands run by `Match exec`.
			logFile, lerr := os.CreateTemp("", "fam-sshg-*")
			if lerr != nil {
				return false
			}
			logPath := logFile.Name()
			_ = logFile.Close()
			defer os.Remove(logPath)

			cmdExtra := append(append([]string{}, extra...), "-E", logPath, "-vvv", "-T")
			outCmd, _, ok1 := sshG(ctx, spec, configFile, cmdExtra, "sh -c true")
			if ok1 {
				cmdInfo := filterSSHG(outCmd, true)
				topSendEnv = cmdInfo.sendEnv
				topSetEnv = cmdInfo.setEnv
			}
			diagBytes, rerr := os.ReadFile(logPath)
			outBase, _, ok2 := sshG(ctx, spec, configFile, extra, "")
			if !ok1 || !ok2 || rerr != nil || hasCommandDependentMatch(string(diagBytes)) {
				return false
			}
			fCmd := filterSSHG(outCmd, true)
			fBase := filterSSHG(outBase, true)
			if fCmd.filtered != fBase.filtered || fCmd.hostName != fBase.hostName || fCmd.port != fBase.port || fCmd.proxyCmd != fBase.proxyCmd || fCmd.proxyJump != fBase.proxyJump {
				return false
			}
			// Hash the full unfiltered baseline (preserving user LogLevel/RequestTTY) after verifying
			// equivalence with the command-mode probe.
			info = filterSSHG(outBase, false)
			topSendEnv = info.sendEnv
			topSetEnv = info.setEnv
		} else {
			out, _, ok := sshG(ctx, spec, configFile, extra, "")
			if !ok {
				return false
			}
			info = filterSSHG(out, false)
			if info.controlPath != "" {
				// A ProxyJump hop with ControlPath can multiplex through an external ControlMaster
				// whose forwarding target is not statically captured by `ssh -G`.
				return false
			}
		}
		if info.proxyCmd != "" {
			// Arbitrary ProxyCommand targets cannot be statically fingerprinted.
			return false
		}
		h.Write([]byte("hop\x00"))
		h.Write([]byte(visitKey))
		h.Write([]byte{0})
		h.Write([]byte(info.filtered))

		if info.proxyJump != "" {
			var hops []string
			for _, rawHop := range strings.Split(info.proxyJump, ",") {
				if rawHop = strings.TrimSpace(rawHop); rawHop != "" {
					hops = append(hops, rawHop)
				}
			}
			if len(hops) == 0 {
				return true
			}
			// OpenSSH implements ProxyJump by invoking the last hop with `-p <hopPort>`,
			// `-J <preceding hops>`, and `-W '[%h]:%p'`, which recursively unwinds the chain.
			lastHop := hops[len(hops)-1]
			hopTarget, hopPort := splitJumpHop(lastHop)
			hopPrev := ""
			if len(hops) > 1 {
				hopPrev = strings.Join(hops[:len(hops)-1], ",")
			}
			nextForward := ""
			if info.hostName != "" && info.port != "" {
				nextForward = fmt.Sprintf("[%s]:%s", info.hostName, info.port)
			}
			h.Write([]byte("jump\x00"))
			h.Write([]byte(info.proxyJump))
			h.Write([]byte{'\n'})
			if !walk(hopTarget, hopPort, hopPrev, nextForward, depth+1) {
				return false
			}
		}
		return true
	}
	if !walk(target, "", "", "", 0) {
		return "", topSendEnv, topSetEnv, false
	}
	return hex.EncodeToString(h.Sum(nil)), topSendEnv, topSetEnv, true
}

// ControlSocketProbeCtx resolves both the canonical identity key (when statically guaranteed)
// and effective SendEnv/SetEnv forwarding patterns from `ssh -G`, honoring ctx cancellation.
func ControlSocketProbeCtx(ctx context.Context, target, configFile string) (key string, sendEnv []string, setEnv []string, ok bool) {
	id, sEnv, setE, ok := resolveSSHIdentity(ctx, target, configFile)
	if !ok {
		return "", sEnv, setE, false
	}
	cfg := configFile
	if cfg != "" {
		if abs, err := filepath.Abs(cfg); err == nil {
			cfg = abs
		}
	}
	return target + "\x00" + cfg + "\x00" + id, sEnv, setE, true
}

// ControlSocketProbe resolves both the canonical identity key and effective SendEnv/SetEnv.
func ControlSocketProbe(target, configFile string) (key string, sendEnv []string, setEnv []string, ok bool) {
	return ControlSocketProbeCtx(context.Background(), target, configFile)
}

// ControlSocketKeyCtx returns a canonical key honoring ctx cancellation/timeout.
func ControlSocketKeyCtx(ctx context.Context, target, configFile string) (string, bool) {
	key, _, _, ok := ControlSocketProbeCtx(ctx, target, configFile)
	return key, ok
}

// ControlSocketKey returns a canonical key combining the raw alias, config path, and effective
// `ssh -G` connection identity (including resolved ProxyJump hops and Match command check).
// Returns ("", false) when effective identity cannot be statically guaranteed so callers bypass
// daemon pooling cleanly.
func ControlSocketKey(target, configFile string) (string, bool) {
	return ControlSocketKeyCtx(context.Background(), target, configFile)
}

func uniqueControlSock(dir string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return filepath.Join(dir, "m-"+hex.EncodeToString(b[:]))
}

// ControlSock returns the ControlMaster socket path currently owned or attached by s.
func (s *SSH) ControlSock() string {
	s.wireMu.Lock()
	defer s.wireMu.Unlock()
	return s.controlSock
}

// IsMasterDead reports whether s has been poisoned after a refresh, drift, or teardown failure.
func (s *SSH) IsMasterDead() bool {
	s.wireMu.Lock()
	defer s.wireMu.Unlock()
	return s.masterDead
}

// PinSocketIdentity records the verified identity key and effective SendEnv/SetEnv forwarding
// patterns for this SSH instance (used by the daemon).
func (s *SSH) PinSocketIdentity(key string, sendEnv, setEnv []string) {
	s.wireMu.Lock()
	s.pinnedKey = key
	s.unreusable = false
	s.sendEnv = append([]string(nil), sendEnv...)
	s.setEnv = append([]string(nil), setEnv...)
	s.wireMu.Unlock()
}

func sshOptionQuote(v string) string {
	// Quote raw bytes directly (never rune-decoding non-UTF-8 bytes) so apostrophes, whitespace,
	// backslashes, quotes, trailing CRs, and arbitrary OpenSSH token bytes survive unchanged.
	var b bytes.Buffer
	b.Grow(len(v) + 2)
	b.WriteByte('"')
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	b.WriteByte('"')
	return b.String()
}

func attachArgs(sock string, sendEnv, setEnv []string) []string {
	// Fail-closed multiplexing onto an already-open ControlMaster: never parses ~/.ssh/config,
	// never spawns a new master, carries forward effective SendEnv/SetEnv patterns (emitting all
	// SetEnv entries in a single option since OpenSSH keeps only the first SetEnv directive),
	// and fails immediately (exit 255) if the pinned master died.
	args := make([]string, 0, 14)
	args = append(args,
		"-F", "/dev/null",
		"-o", "ControlMaster=no",
		"-o", "ProxyCommand=false",
		"-o", "BatchMode=yes",
	)
	if len(sendEnv) > 0 {
		quoted := make([]string, len(sendEnv))
		for i, pat := range sendEnv {
			quoted[i] = sshOptionQuote(pat)
		}
		args = append(args, "-o", "SendEnv "+strings.Join(quoted, " "))
	}
	if len(setEnv) > 0 {
		quoted := make([]string, len(setEnv))
		for i, kv := range setEnv {
			quoted[i] = sshOptionQuote(kv)
		}
		args = append(args, "-o", "SetEnv "+strings.Join(quoted, " "))
	}
	args = append(args, "-S", sock)
	return args
}

func (s *SSH) masterArgsWithSock(sock string) []string {
	var args []string
	if s.ConfigFile != "" {
		args = append(args, "-F", s.ConfigFile)
	}
	timeout := 20
	if s.ConnectTimeout > 0 {
		timeout = int(s.ConnectTimeout.Seconds())
	}
	args = append(args,
		"-o", "ControlMaster=auto",
		"-o", "ControlPath="+sock,
		"-o", "ControlPersist=120",
		"-o", "BatchMode=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", timeout),
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=3",
	)
	return args
}

// commandArgs returns SSH arguments and destination for either:
//   - fail-closed attachment to an already-pinned ControlMaster (never switches hosts), or
//   - opening a new unique-socket ControlMaster (only called under bootSem, first verifying pinnedKey).
func (s *SSH) commandArgs(ctx context.Context) (args []string, target string, createdMaster bool, err error) {
	s.wireMu.Lock()
	dead := s.masterDead
	pinned := s.masterPinned
	sock := s.controlSock
	pinnedKey := s.pinnedKey
	unreusable := s.unreusable
	sendEnv := s.sendEnv
	setEnv := s.setEnv
	s.wireMu.Unlock()
	if dead {
		return nil, "", false, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH control master is not active"}
	}
	if pinned {
		if sock == "" {
			return nil, "", false, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH control master is not active"}
		}
		return attachArgs(sock, sendEnv, setEnv), "_", false, nil
	}
	// Establishing a new ControlMaster: if this transport already pinned an identity (for example
	// before Reconnect), refuse to open a replacement master if SSH config drifted.
	if !unreusable && pinnedKey != "" {
		curKey, ok := ControlSocketKeyCtx(ctx, s.Target, s.ConfigFile)
		if !ok {
			if ctx.Err() != nil {
				return nil, "", false, ctx.Err()
			}
			return nil, "", false, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH destination configuration changed during run; re-bootstrap required"}
		}
		if curKey != pinnedKey {
			return nil, "", false, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH destination configuration changed during run; re-bootstrap required"}
		}
	}
	dir, err := controlDir()
	if err != nil {
		return nil, "", false, err
	}
	s.wireMu.Lock()
	if s.controlSock == "" {
		s.controlSock = uniqueControlSock(dir)
	}
	sock = s.controlSock
	s.wireMu.Unlock()
	return s.masterArgsWithSock(sock), s.Target, true, nil
}

func (s *SSH) getWire() WireConn {
	s.wireMu.Lock()
	defer s.wireMu.Unlock()
	return s.wire
}

func (s *SSH) getWireWithGen() (WireConn, uint64) {
	s.wireMu.Lock()
	defer s.wireMu.Unlock()
	return s.wire, s.wireGen
}

func (s *SSH) currentWireGen() uint64 {
	s.wireMu.Lock()
	defer s.wireMu.Unlock()
	return s.wireGen
}

func (s *SSH) closeWire() {
	s.wireMu.Lock()
	w := s.wire
	s.wire = nil
	s.wireMu.Unlock()
	if w != nil {
		_ = w.Close()
	}
}

// Name implements Transport.
func (s *SSH) Name() string { return s.Machine }

// Kind implements Transport.
func (s *SSH) Kind() string { return "ssh" }

// MaxConcurrency implements ConcurrencyProvider.
func (s *SSH) MaxConcurrency() int {
	if s.getWire() != nil {
		return 64
	}
	return 8
}

// HasNativeSkillOps implements SkillOps.
func (s *SSH) HasNativeSkillOps() bool {
	return s.getWire() != nil
}

// SupportsPrimBatch implements PrimBatcher.
func (s *SSH) SupportsPrimBatch() bool {
	return s.getWire() != nil
}

// Run implements Transport.
func (s *SSH) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Sudo && len(c.Stdin) > 0 {
		return Result{}, errors.New("internal error: a sudo command cannot read stdin")
	}
	if !c.LoginShell && !c.Interactive {
		if w := s.getWire(); w != nil {
			return s.runWire(ctx, w, c)
		}
	}
	return s.runDirect(ctx, c)
}

func (s *SSH) runWire(ctx context.Context, w WireConn, c Cmd) (Result, error) {
	start := time.Now()
	var resp WireResp
	var err error
	var hit bool
	if len(c.Stdin) == 0 && !c.LoginShell && !c.Interactive {
		resp, hit, err = s.lookupSpec(ctx, SpecKeyExec(c.Sudo, c.Script))
	}
	if !hit {
		req := WireReq{
			Op:        "exec",
			Script:    c.Script,
			Sudo:      c.Sudo,
			TimeoutMS: c.Timeout.Milliseconds(),
		}
		if len(c.Stdin) > 0 {
			req.StdinB64 = base64.StdEncoding.EncodeToString(c.Stdin)
		}
		resp, err = w.Call(ctx, req)
	}
	if err != nil {
		if c.Sudo && ctx.Err() != nil {
			s.closeWire()
		}
		res := Result{Duration: time.Since(start)}
		if IsConnError(err) {
			res.Code = 255
		}
		s.observe(c, res, err)
		return res, err
	}
	if resp.EnvChanged {
		s.wireMu.Lock()
		s.envChanged = true
		s.wireMu.Unlock()
	}
	res, err := decodeExec(s.Machine, s.Target, resp)
	s.observe(c, res, err)
	return res, err
}

// Stat overrides files.Stat to execute in-process on the remote persistent worker when active.
func (s *SSH) Stat(ctx context.Context, path string, sudo bool) (FileInfo, error) {
	w := s.getWire()
	if w == nil {
		return s.files.Stat(ctx, path, sudo)
	}
	start := time.Now()
	resp, hit, err := s.lookupSpec(ctx, SpecKeyStat(sudo, path))
	if !hit {
		resp, err = w.Call(ctx, WireReq{Op: "stat", Path: path, Sudo: sudo})
	}
	if err != nil {
		s.observe(Cmd{Label: "stat " + path, Sudo: sudo}, Result{Duration: time.Since(start)}, err)
		return FileInfo{}, err
	}
	dur := time.Duration(resp.DurationMS) * time.Millisecond
	if resp.Err != "" {
		err = errors.New(resp.Err)
		s.observe(Cmd{Label: "stat " + path, Sudo: sudo}, Result{Code: 5, Duration: dur}, err)
		return FileInfo{}, err
	}
	s.observe(Cmd{Label: "stat " + path, Sudo: sudo}, Result{Duration: dur}, nil)
	return decodeFileInfo(resp), nil
}

// ReadFile overrides files.ReadFile to execute in-process on the remote persistent worker.
func (s *SSH) ReadFile(ctx context.Context, path string, sudo bool) ([]byte, bool, error) {
	if s.getWire() == nil {
		return s.files.ReadFile(ctx, path, sudo)
	}
	data, info, err := s.ReadFileInfo(ctx, path, sudo)
	return data, info.Exists, err
}

// ReadFileInfo overrides files.ReadFileInfo to read content + mode in a single RPC.
func (s *SSH) ReadFileInfo(ctx context.Context, path string, sudo bool) ([]byte, FileInfo, error) {
	w := s.getWire()
	if w == nil {
		return s.files.ReadFileInfo(ctx, path, sudo)
	}
	start := time.Now()
	resp, hit, err := s.lookupSpec(ctx, SpecKeyRead(sudo, path))
	if !hit {
		resp, err = w.Call(ctx, WireReq{Op: "read", Path: path, Sudo: sudo})
	}
	if err != nil {
		s.observe(Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, Result{Duration: time.Since(start)}, err)
		return nil, FileInfo{}, err
	}
	dur := time.Duration(resp.DurationMS) * time.Millisecond
	if resp.Err != "" {
		err = errors.New(resp.Err)
		s.observe(Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, Result{Code: 5, Duration: dur}, err)
		return nil, FileInfo{}, err
	}
	if resp.NotRegular {
		err = fmt.Errorf("%s exists and is not a regular file", path)
		s.observe(Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, Result{Code: 4, Duration: dur}, err)
		return nil, FileInfo{Exists: true}, err
	}
	if !resp.Exists {
		s.observe(Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, Result{Code: 3, Duration: dur}, nil)
		return nil, FileInfo{}, nil
	}
	data, err := base64.StdEncoding.DecodeString(resp.DataB64)
	if err != nil {
		return nil, FileInfo{}, fmt.Errorf("decode read %s: %w", path, err)
	}
	s.observe(Cmd{Label: "read " + path, Sudo: sudo, Secret: true}, Result{Duration: dur}, nil)
	return data, decodeFileInfo(resp), nil
}

// WriteFile overrides files.WriteFile to write atomically in a single RPC.
func (s *SSH) WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode, sudo bool) error {
	w := s.getWire()
	if w == nil {
		return s.files.WriteFile(ctx, path, data, mode, sudo)
	}
	start := time.Now()
	m := uint32(mode.Perm())
	resp, err := w.Call(ctx, WireReq{
		Op:      "write",
		Path:    path,
		DataB64: base64.StdEncoding.EncodeToString(data),
		Mode:    &m,
		Sudo:    sudo,
	})
	if err != nil {
		s.observe(Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, Result{Duration: time.Since(start)}, err)
		return err
	}
	dur := time.Duration(resp.DurationMS) * time.Millisecond
	if resp.Err != "" {
		err = errors.New(resp.Err)
		s.observe(Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, Result{Code: 1, Duration: dur}, err)
		return err
	}
	s.observe(Cmd{Label: "write " + path, Sudo: sudo, Stdin: data}, Result{Duration: dur}, nil)
	return nil
}

// SkillCheck implements SkillOps in a single in-process RPC when the remote worker is active.
func (s *SSH) SkillCheck(ctx context.Context, dir, marker string) (SkillState, error) {
	w := s.getWire()
	if w == nil {
		return SkillState{}, errors.New("native skill ops unavailable")
	}
	start := time.Now()
	resp, hit, err := s.lookupSpec(ctx, SpecKeySkill(dir, marker))
	if !hit {
		resp, err = w.Call(ctx, WireReq{Op: "skill_check", Dir: dir, Marker: marker})
	}
	if err != nil {
		s.observe(Cmd{Label: "digest of " + dir}, Result{Duration: time.Since(start)}, err)
		return SkillState{}, err
	}
	dur := time.Duration(resp.DurationMS) * time.Millisecond
	if resp.Err != "" {
		err = errors.New(resp.Err)
		s.observe(Cmd{Label: "digest of " + dir}, Result{Code: 1, Duration: dur}, err)
		return SkillState{}, err
	}
	s.observe(Cmd{Label: "digest of " + dir}, Result{Duration: dur}, nil)
	return SkillState{Exists: resp.Exists, IsDir: resp.IsDir, Managed: resp.Managed, Digest: resp.Digest}, nil
}

// SkillWrite implements SkillOps in a single in-process RPC when the remote worker is active.
func (s *SSH) SkillWrite(ctx context.Context, dir, marker string, tarball []byte) error {
	w := s.getWire()
	if w == nil {
		return errors.New("native skill ops unavailable")
	}
	start := time.Now()
	resp, err := w.Call(ctx, WireReq{
		Op:     "skill_write",
		Dir:    dir,
		Marker: marker,
		TarB64: base64.StdEncoding.EncodeToString(tarball),
	})
	if err != nil {
		s.observe(Cmd{Label: "copy skill " + filepath.Base(dir), Stdin: tarball}, Result{Duration: time.Since(start)}, err)
		return err
	}
	dur := time.Duration(resp.DurationMS) * time.Millisecond
	if resp.Err != "" {
		err = errors.New(resp.Err)
		s.observe(Cmd{Label: "copy skill " + filepath.Base(dir), Stdin: tarball}, Result{Code: 1, Duration: dur}, err)
		return err
	}
	s.observe(Cmd{Label: "copy skill " + filepath.Base(dir), Stdin: tarball}, Result{Duration: dur}, nil)
	return nil
}

// BatchPrims implements PrimBatcher: executes Go-planned dumb primitives sequentially in one RTT.
func (s *SSH) BatchPrims(ctx context.Context, ops []PrimOp, onResult func(PrimResult)) error {
	w := s.getWire()
	if w == nil {
		return errors.New("persistent worker is not active")
	}
	byIdx := make(map[int]PrimOp, len(ops))
	for _, op := range ops {
		byIdx[op.Index] = op
	}
	return w.CallStream(ctx, WireReq{Op: "batch", Ops: ops}, func(resp WireResp) {
		if resp.Prim == nil {
			return
		}
		pr := *resp.Prim
		op := byIdx[pr.Index]
		var opErr error
		code := 0
		if pr.Err != "" && !pr.Stale {
			opErr = errors.New(pr.Err)
			code = 1
		}
		if !pr.Stale {
			s.observe(Cmd{Label: op.Label, Sudo: op.Sudo, Secret: op.Secret}, Result{Code: code, Duration: time.Duration(pr.DurationMS) * time.Millisecond}, opErr)
		}
		onResult(pr)
	})
}

func (s *SSH) directAttachArgs(ctx context.Context) ([]string, error) {
	s.wireMu.Lock()
	dead := s.masterDead
	pinned := s.masterPinned
	sock := s.controlSock
	sendEnv := s.sendEnv
	setEnv := s.setEnv
	s.wireMu.Unlock()
	if dead {
		return nil, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH control master is not active"}
	}
	if pinned {
		if sock == "" {
			return nil, &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH control master is not active"}
		}
		return attachArgs(sock, sendEnv, setEnv), nil
	}
	return s.ensureMasterAndAttachArgs(ctx)
}

func (s *SSH) runDirect(ctx context.Context, c Cmd) (Result, error) {
	// Apply Cmd.Timeout before master establishment so it covers the first SSH connection too.
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	// Direct commands are always attachment-only: master establishment (if needed) and pinned
	// attachment argument selection are serialized under bootMu.
	args, err := s.directAttachArgs(ctx)
	if err != nil {
		res := Result{}
		if IsConnError(err) {
			res.Code = 255
		}
		s.observe(c, res, err)
		return res, err
	}
	var remote string
	switch {
	case c.LoginShell && c.Interactive:
		args = append(args, "-tt")
		remote = `exec "$SHELL" -i -c ` + Quote(c.Script)
	case c.LoginShell:
		args = append(args, "-T")
		remote = c.Script
	case c.Sudo:
		args = append(args, "-T")
		remote = "sudo -n sh -c " + Quote(c.Script)
	default:
		args = append(args, "-T")
		remote = "sh -c " + Quote(c.Script)
	}
	args = append(args, "_", "--", remote)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = bytes.NewReader(c.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	err = cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Duration: time.Since(start)}
	if c.Interactive {
		res.Stdout = bytes.ReplaceAll(res.Stdout, []byte("\r\n"), []byte("\n"))
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			err = fmt.Errorf("run ssh: %w", err)
			s.observe(c, res, err)
			return res, err
		}
		res.Code = ee.ExitCode()
		if res.Code == -1 && ctx.Err() != nil {
			err = fmt.Errorf("command on %s stopped after the timeout: %w", s.Machine, ctx.Err())
			s.observe(c, res, err)
			return res, err
		}
	}
	// Exit code 255 is a connection error. Classify it before the script result.
	if res.Code == 255 {
		err := &ConnError{Machine: s.Machine, Target: s.Target, Stderr: string(res.Stderr)}
		s.observe(c, res, err)
		return res, err
	}
	s.observe(c, res, nil)
	return res, nil
}

func (s *SSH) observe(c Cmd, r Result, err error) {
	if s.Observer != nil {
		s.Observer(s.Machine, c, r, err)
	}
}

func (s *SSH) control(ctx context.Context, op string) error {
	s.wireMu.Lock()
	sock := s.controlSock
	s.wireMu.Unlock()
	if sock == "" {
		// This SSH instance never opened a ControlMaster socket: nothing to close.
		return nil
	}
	// Use -F /dev/null -S <sock> so control commands never depend on the configured -F file
	// still existing or still matching the master that opened sock.
	cmd := exec.CommandContext(ctx, "ssh", "-F", "/dev/null", "-o", "BatchMode=yes", "-S", sock, "-O", op, "_")
	out, err := cmd.CombinedOutput()
	if op == "exit" {
		if err != nil {
			// Retain s.controlSock if teardown was cancelled or the socket still appears live
			// so a subsequent Close() can finish tearing down the master.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, statErr := os.Stat(sock); statErr == nil {
				if bytes.Contains(out, []byte("Connection refused")) {
					_ = os.Remove(sock)
				} else {
					return fmt.Errorf("ssh -O exit: %s", bytes.TrimSpace(out))
				}
			}
		}
		s.wireMu.Lock()
		if s.controlSock == sock {
			s.controlSock = ""
			s.masterPinned = false
		}
		s.wireMu.Unlock()
		return nil
	}
	if err != nil {
		return fmt.Errorf("ssh -O %s: %s", op, bytes.TrimSpace(out))
	}
	return nil
}

// Reconnect implements Transport. It closes the persistent worker and ControlMaster connection.
func (s *SSH) Reconnect(ctx context.Context) error {
	if err := s.lockBoot(ctx); err != nil {
		return err
	}
	defer s.unlockBoot()
	if s.closed {
		return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "ssh transport closed"}
	}
	return s.reconnectLocked(ctx)
}

func (s *SSH) reconnectLocked(ctx context.Context) error {
	s.wireMu.Lock()
	pooled := s.pooled
	pinnedKey := s.pinnedKey
	unreusable := s.unreusable
	curSock := s.controlSock
	hadMaster := s.masterPinned || curSock != ""
	s.wireMu.Unlock()
	s.ClearSpecCache()
	// When the original destination identity could not be statically verified (unreusable or
	// missing pinnedKey), tear down the current master and require a fresh bootstrap rather than
	// allowing Reconnect to open a new connection to an unverified destination.
	if unreusable || (hadMaster && pinnedKey == "") {
		if pooled && s.Pool != nil {
			s.wireMu.Lock()
			s.wire = nil
			s.hasPy3 = false
			s.masterPinned = true
			s.masterDead = true
			s.wireMu.Unlock()
			if curSock != "" {
				_ = s.Pool.Reconnect(ctx, s.Machine, s.Target, s.ConfigFile, pinnedKey, curSock)
			}
		} else {
			s.closeWire()
			cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.control(cleanCtx, "exit")
			cleanCancel()
			s.wireMu.Lock()
			s.hasPy3 = false
			s.masterPinned = true
			s.masterDead = true
			s.wireMu.Unlock()
		}
		return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: "SSH destination identity could not be verified; re-bootstrap required"}
	}
	if pooled && s.Pool != nil {
		s.wireMu.Lock()
		s.wire = nil
		s.controlSock = ""
		s.masterPinned = false
		s.masterDead = false
		// Keep s.hasPy3 and s.pinnedKey intact so a subsequent RefreshSession or direct command
		// validates the pinned identity and restores the pooled worker on the same host.
		s.wireMu.Unlock()
		if curSock == "" {
			return nil
		}
		return s.Pool.Reconnect(ctx, s.Machine, s.Target, s.ConfigFile, pinnedKey, curSock)
	}
	s.closeWire()
	cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := s.control(cleanCtx, "exit")
	cleanCancel()
	if err == nil {
		s.wireMu.Lock()
		s.masterDead = false
		s.wireMu.Unlock()
	}
	return err
}

// ReconnectIfSock atomically verifies under bootSem that s is still pinned to expectedSock and
// closes its worker and ControlMaster if so. Used by the daemon so a concurrent bootstrap cannot
// rotate the master between socket check and teardown.
func (s *SSH) ReconnectIfSock(ctx context.Context, expectedSock string) (bool, error) {
	if expectedSock == "" {
		return false, nil
	}
	if err := s.lockBoot(ctx); err != nil {
		return false, err
	}
	defer s.unlockBoot()
	if s.closed {
		return false, nil
	}
	s.wireMu.Lock()
	curSock := s.controlSock
	s.wireMu.Unlock()
	if curSock != expectedSock {
		return false, nil
	}
	return true, s.closeLocked()
}

// Close implements Transport. When backed by SessionPool, it releases only this client's lease so
// the ControlMaster and wire remain warm for subsequent commands; otherwise it tears them down.
func (s *SSH) Close() error {
	s.lockBootBlocking()
	defer s.unlockBoot()
	if s.closed {
		if s.ControlSock() != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return s.control(ctx, "exit")
		}
		return nil
	}
	return s.closeLocked()
}

// CloseIfGen closes s only if its current wire generation still matches gen.
// Used by the daemon so a late connection error from an older wire generation cannot close or
// evict a session that has already been refreshed to a newer live wire.
func (s *SSH) CloseIfGen(gen uint64) bool {
	s.lockBootBlocking()
	defer s.unlockBoot()
	if s.closed {
		return false
	}
	if s.currentWireGen() != gen {
		return false
	}
	_ = s.closeLocked()
	return true
}

func (s *SSH) closeLocked() error {
	s.closed = true
	s.ClearSpecCache()
	s.wireMu.Lock()
	w := s.wire
	lease := s.poolLease
	pooled := s.pooled
	s.wire = nil
	s.poolLease = nil
	s.pooled = false
	s.masterPinned = false
	s.masterDead = true
	s.pinnedKey = ""
	if pooled {
		// Pooled client only borrowed the daemon's socket; never close the daemon's master.
		s.controlSock = ""
	}
	s.wireMu.Unlock()
	if pooled {
		if lease != nil {
			_ = lease.Close()
		}
		return nil
	}
	if sw, ok := w.(*sshWire); ok {
		sw.beginClose()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := s.control(ctx, "exit")
		sw.finishClose()
		return err
	}
	if w != nil {
		_ = w.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.control(ctx, "exit")
}
