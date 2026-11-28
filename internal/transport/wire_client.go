package transport

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed wire_worker.py
var wireWorkerScript string

var wireWorkerB64 = base64.StdEncoding.EncodeToString([]byte(wireWorkerScript))

// WireReq is one JSON request sent to the persistent Python wire worker (directly or via daemon).
// Fields are scoped by Op:
//   - cancel: TargetID
//   - stat / read / write: Path, DataB64, Mode, Sudo, Verify
//   - symlink: Path, Target, Sudo
//   - skill_check / skill_write: Dir, Marker, TarB64
//   - exec: Script, StdinB64, Sudo, TimeoutMS
//   - batch: Ops
//   - spec_batch / warm_bootstrap: FactsScript, SecretsRel, FactsSHA, Reqs
type WireReq struct {
	ID          uint64    `json:"id"`
	Op          string    `json:"op"`
	TargetID    uint64    `json:"target_id,omitempty"`
	Path        string    `json:"path,omitempty"`
	Target      string    `json:"target,omitempty"`
	Dir         string    `json:"dir,omitempty"`
	Marker      string    `json:"marker,omitempty"`
	Script      string    `json:"script,omitempty"`
	StdinB64    string    `json:"stdin_b64,omitempty"`
	DataB64     string    `json:"data_b64,omitempty"`
	TarB64      string    `json:"tar_b64,omitempty"`
	Mode        *uint32   `json:"mode,omitempty"`
	Sudo        bool      `json:"sudo,omitempty"`
	Verify      bool      `json:"verify,omitempty"`
	TimeoutMS   int64     `json:"timeout_ms,omitempty"`
	Ops         []PrimOp  `json:"ops,omitempty"`
	FactsSHA    string    `json:"facts_sha,omitempty"`
	FactsScript string    `json:"facts_script,omitempty"`
	SecretsRel  string    `json:"secrets_rel,omitempty"`
	Reqs        []SpecReq `json:"reqs,omitempty"`
}

// WireResp is one decoded JSON response from the persistent Python wire worker.
type WireResp struct {
	ID          uint64              `json:"id"`
	Err         string              `json:"err,omitempty"`
	Exists      bool                `json:"exists,omitempty"`
	IsDir       bool                `json:"is_dir,omitempty"`
	Managed     bool                `json:"managed,omitempty"`
	NotRegular  bool                `json:"not_regular,omitempty"`
	Type        string              `json:"type,omitempty"`
	Target      string              `json:"target,omitempty"`
	Mode        uint32              `json:"mode,omitempty"`
	SHA256      string              `json:"sha256,omitempty"`
	Digest      string              `json:"digest,omitempty"`
	DataB64     string              `json:"data_b64,omitempty"`
	Code        int                 `json:"code,omitempty"`
	StdoutB64   string              `json:"stdout_b64,omitempty"`
	StderrB64   string              `json:"stderr_b64,omitempty"`
	DurationMS  int64               `json:"duration_ms,omitempty"`
	TimedOut    bool                `json:"timed_out,omitempty"`
	EnvChanged  bool                `json:"env_changed,omitempty"`
	Prim        *PrimResult         `json:"prim,omitempty"`
	SpecKey     string              `json:"spec_key,omitempty"`
	SpecResp    *WireResp           `json:"spec_resp,omitempty"`
	SpecHit     bool                `json:"spec_hit,omitempty"`
	SpecResults map[string]WireResp `json:"spec_results,omitempty"`
	Stale       bool                `json:"stale,omitempty"`
	Done        bool                `json:"done,omitempty"`
}

// WireConn is the transport-agnostic RPC pipe to the remote Python worker.
// Both a direct local sshWire pipe and a daemon Unix-socket relay implement WireConn so all
// SSH operations, observation, and spec caching live in exactly one place (transport.SSH).
type WireConn interface {
	Call(ctx context.Context, req WireReq) (WireResp, error)
	CallStream(ctx context.Context, req WireReq, onResp func(WireResp)) error
	Close() error
}

// SessionPool optionally keeps SSH ControlMaster + persistent wire sessions warm across CLI runs.
type SessionPool interface {
	WarmBootstrap(ctx context.Context, machine, target, sshConfig, expectedKey, factsScript, secretsRel, expectedSHA string, reqs []SpecReq) (BootstrapResult, WireConn, io.Closer, error)
	Refresh(ctx context.Context, machine, target, sshConfig, sessionKey, controlSock string) (WireConn, string, bool, error)
	Reconnect(ctx context.Context, machine, target, sshConfig, sessionKey, controlSock string) error
}

// decodeFileInfo converts a WireResp from stat/write(verify) into FileInfo.
func decodeFileInfo(resp WireResp) FileInfo {
	if !resp.Exists {
		return FileInfo{}
	}
	return FileInfo{
		Exists: true,
		Type:   resp.Type,
		Mode:   os.FileMode(resp.Mode),
		SHA256: resp.SHA256,
		Target: resp.Target,
	}
}

// decodeExec converts a WireResp from exec into Result + error.
func decodeExec(machine, target string, resp WireResp) (Result, error) {
	outB, _ := base64.StdEncoding.DecodeString(resp.StdoutB64)
	errB, _ := base64.StdEncoding.DecodeString(resp.StderrB64)
	res := Result{
		Stdout:   outB,
		Stderr:   errB,
		Code:     resp.Code,
		Duration: time.Duration(resp.DurationMS) * time.Millisecond,
	}
	switch {
	case resp.TimedOut:
		return res, fmt.Errorf("command on %s stopped after the timeout: %w", machine, context.DeadlineExceeded)
	case resp.Err != "":
		return res, errors.New(resp.Err)
	case res.Code == 255:
		return res, &ConnError{Machine: machine, Target: target, Stderr: string(res.Stderr)}
	default:
		return res, nil
	}
}

type pendingCall struct {
	ch     chan WireResp
	cancel chan struct{}
	stream bool
}

type sshWire struct {
	machine string
	target  string
	cmd     *exec.Cmd
	stdin   io.WriteCloser

	sendCh  chan []byte
	quitCh  chan struct{}
	nextID  atomic.Uint64
	mu      sync.Mutex
	pending map[uint64]*pendingCall
	closed  bool
	dead    bool
	deadErr error
	done    chan struct{}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func wireLaunchSnippet() string {
	return "if command -v python3 >/dev/null 2>&1; then\n" +
		fmt.Sprintf("  python3 -B -u -c \"import base64,sys; sys.exit(99) if sys.version_info<(3,6) else (sys.stdout.write('FAM-WIRE-PY3\\n'), sys.stdout.flush(), exec(base64.b64decode('%s')))\" || echo FAM-WIRE-NONE\n", wireWorkerB64) +
		"else\n  echo FAM-WIRE-NONE\nfi\n"
}

type startedSSH struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	br     *bufio.Reader
	errBuf *lockedBuffer
	done   chan struct{}
}

// startSSHProcess spawns `ssh <args> -T <target> -- sh -c <script>` and wires context cancellation
// during the startup handshake. Callers close s.done once startup finishes.
func (s *SSH) startSSHProcess(ctx context.Context, script string) (*startedSSH, error) {
	args, target, _, err := s.commandArgs(ctx)
	if err != nil {
		return nil, err
	}
	args = append(args, "-T", target, "--", "sh -c "+Quote(script))
	cmd := exec.Command("ssh", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("ssh stdout pipe: %w", err)
	}
	errBuf := &lockedBuffer{}
	cmd.Stderr = errBuf
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("run ssh: %w", err)
	}
	bootDone := make(chan struct{})
	go func() {
		select {
		case <-bootDone:
		case <-ctx.Done():
			_ = stdin.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}()
	return &startedSSH{
		cmd:    cmd,
		stdin:  stdin,
		br:     bufio.NewReaderSize(stdout, 64*1024),
		errBuf: errBuf,
		done:   bootDone,
	}, nil
}

func newSSHWire(machine, target string, proc *startedSSH) *sshWire {
	return &sshWire{
		machine: machine,
		target:  target,
		cmd:     proc.cmd,
		stdin:   proc.stdin,
		sendCh:  make(chan []byte, 256),
		quitCh:  make(chan struct{}),
		pending: map[uint64]*pendingCall{},
		done:    make(chan struct{}),
	}
}

func (w *sshWire) start(br *bufio.Reader, errBuf *lockedBuffer) {
	go w.writeLoop()
	go w.readLoop(br, errBuf)
}

func (w *sshWire) writeLoop() {
	defer w.stdin.Close()
	var batch bytes.Buffer
	for {
		var first []byte
		select {
		case first = <-w.sendCh:
		case <-w.quitCh:
			_, _ = w.stdin.Write([]byte("{\"id\":0,\"op\":\"quit\"}\n"))
			return
		case <-w.done:
			return
		}
		batch.Reset()
		batch.Write(first)
		for draining := true; draining && batch.Len() < 256*1024; {
			select {
			case more := <-w.sendCh:
				batch.Write(more)
			default:
				draining = false
			}
		}
		if _, err := w.stdin.Write(batch.Bytes()); err != nil {
			return
		}
	}
}

func (w *sshWire) readLoop(br *bufio.Reader, errBuf *lockedBuffer) {
	defer close(w.done)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var resp WireResp
			if jerr := json.Unmarshal(bytes.TrimSpace(line), &resp); jerr == nil {
				w.mu.Lock()
				pc, ok := w.pending[resp.ID]
				if ok && (!pc.stream || resp.Done) {
					delete(w.pending, resp.ID)
				}
				w.mu.Unlock()
				if ok {
					select {
					case pc.ch <- resp:
					case <-pc.cancel:
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	waitErr := w.cmd.Wait()
	stderr := errBuf.String()
	var deadErr error
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	if !closed {
		code := 255
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			code = ee.ExitCode()
		}
		if code == 255 || stderr != "" || waitErr != nil {
			ce := &ConnError{Machine: w.machine, Target: w.target, Stderr: stderr}
			if strings.TrimSpace(ce.Stderr) == "" && waitErr != nil {
				ce.Stderr = waitErr.Error()
			}
			deadErr = ce
		} else {
			deadErr = &ConnError{Machine: w.machine, Target: w.target, Stderr: "remote worker exited unexpectedly"}
		}
	}
	w.mu.Lock()
	w.dead = true
	w.deadErr = deadErr
	for id, pc := range w.pending {
		delete(w.pending, id)
		close(pc.ch)
	}
	w.mu.Unlock()
}

func (w *sshWire) sendCancel(id uint64) {
	payload := []byte(fmt.Sprintf("{\"id\":0,\"op\":\"cancel\",\"target_id\":%d}\n", id))
	select {
	case w.sendCh <- payload:
	case <-w.quitCh:
	case <-w.done:
	default:
	}
}

func (w *sshWire) closedErr() error {
	w.mu.Lock()
	derr := w.deadErr
	w.mu.Unlock()
	if derr == nil {
		return &ConnError{Machine: w.machine, Target: w.target, Stderr: "SSH session closed"}
	}
	return derr
}

func (w *sshWire) register(req WireReq, stream bool, buf int) (uint64, []byte, *pendingCall, error) {
	id := w.nextID.Add(1)
	req.ID = id
	payload, err := json.Marshal(req)
	if err != nil {
		return 0, nil, nil, err
	}
	payload = append(payload, '\n')
	pc := &pendingCall{ch: make(chan WireResp, buf), cancel: make(chan struct{}), stream: stream}
	w.mu.Lock()
	if w.dead || w.closed {
		derr := w.deadErr
		w.mu.Unlock()
		if derr == nil {
			derr = &ConnError{Machine: w.machine, Target: w.target, Stderr: "SSH session closed"}
		}
		return 0, nil, nil, derr
	}
	w.pending[id] = pc
	w.mu.Unlock()
	return id, payload, pc, nil
}

func (w *sshWire) unregister(id uint64, pc *pendingCall) {
	w.mu.Lock()
	delete(w.pending, id)
	w.mu.Unlock()
	close(pc.cancel)
}

func (w *sshWire) Call(ctx context.Context, req WireReq) (WireResp, error) {
	id, payload, pc, err := w.register(req, false, 1)
	if err != nil {
		return WireResp{}, err
	}
	select {
	case w.sendCh <- payload:
	case <-w.quitCh:
		w.unregister(id, pc)
		return WireResp{}, w.closedErr()
	case <-w.done:
		w.unregister(id, pc)
		return WireResp{}, w.closedErr()
	case <-ctx.Done():
		w.unregister(id, pc)
		return WireResp{}, ctx.Err()
	}
	select {
	case resp, ok := <-pc.ch:
		if !ok {
			return WireResp{}, w.closedErr()
		}
		return resp, nil
	case <-ctx.Done():
		w.unregister(id, pc)
		w.sendCancel(id)
		return WireResp{}, ctx.Err()
	}
}

func (w *sshWire) CallStream(ctx context.Context, req WireReq, onResp func(WireResp)) error {
	id, payload, pc, err := w.register(req, true, 64)
	if err != nil {
		return err
	}
	select {
	case w.sendCh <- payload:
	case <-w.quitCh:
		w.unregister(id, pc)
		return w.closedErr()
	case <-w.done:
		w.unregister(id, pc)
		return w.closedErr()
	case <-ctx.Done():
		w.unregister(id, pc)
		return ctx.Err()
	}
	for {
		select {
		case resp, ok := <-pc.ch:
			if !ok {
				return w.closedErr()
			}
			if resp.Done {
				if resp.Err != "" {
					return errors.New(resp.Err)
				}
				return nil
			}
			onResp(resp)
		case <-ctx.Done():
			w.unregister(id, pc)
			w.sendCancel(id)
			return ctx.Err()
		}
	}
}

func (w *sshWire) beginClose() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.quitCh)
	w.mu.Unlock()
}

func (w *sshWire) finishClose() {
	select {
	case <-w.done:
	case <-time.After(200 * time.Millisecond):
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
		<-w.done
	}
}

func (w *sshWire) Close() error {
	w.beginClose()
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
		<-w.done
	}
	return nil
}
