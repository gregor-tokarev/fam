// Package runlog writes each run to ~/.local/state/fam/runs/ on the cockpit:
// the plan, the commands, the exit codes, and the output, without secret values.
package runlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gregor-tokarev/fam/internal/output"
	"github.com/gregor-tokarev/fam/internal/transport"
)

// Log is the log file of one run. It writes JSON lines.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	path string
	rd   *output.Redactor
	err  error
	// FAM keeps the entries in memory and writes them at the end, when it knows all secret
	// values (it finds old values on the machines during the run).
	entries []any
}

// Dir returns the directory of the run logs.
func Dir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "fam", "runs"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "fam", "runs"), nil
}

// Open creates the log file of a run.
func Open(runID string) (*Log, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, runID+".jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, path: p, rd: output.NewRedactor(nil)}, nil
}

// Path is the file path.
func (l *Log) Path() string { return l.path }

// SetSecrets sets the values that the log hides.
func (l *Log) SetSecrets(values []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rd = output.NewRedactor(values)
}

type entry struct {
	Time       string `json:"time"`
	Type       string `json:"type"`
	Machine    string `json:"machine,omitempty"`
	Label      string `json:"label,omitempty"`
	Script     string `json:"script,omitempty"`
	Sudo       bool   `json:"sudo,omitempty"`
	LoginShell bool   `json:"loginShell,omitempty"`
	StdinBytes int    `json:"stdinBytes,omitempty"`
	Code       int    `json:"code"`
	DurationMS int64  `json:"durationMs"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	Error      string `json:"error,omitempty"`
}

// write keeps one entry for the end of the run.
func (l *Log) write(v any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, v)
}

// String hides the secret values in s.
func (l *Log) String(s string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rd.String(s)
}

// flush writes the entries as JSON lines. It hides the secret values field by field before
// the encoding, so each line stays valid JSON. FAM keeps the first write error.
func (l *Log) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, v := range l.entries {
		if e, ok := v.(entry); ok {
			rd := l.rd.String
			e.Machine, e.Label, e.Script, e.Stdout, e.Stderr, e.Error = rd(e.Machine), rd(e.Label), rd(e.Script), rd(e.Stdout), rd(e.Stderr), rd(e.Error)
			v = e
		}
		b, err := json.Marshal(v)
		if err == nil {
			_, err = l.f.Write(append(b, '\n'))
		}
		if err != nil && l.err == nil {
			l.err = err
		}
	}
	l.entries = nil
}

// Observer returns the observer for the transports.
func (l *Log) Observer() transport.Observer {
	return func(machine string, c transport.Cmd, r transport.Result, err error) {
		e := entry{Time: time.Now().UTC().Format(time.RFC3339Nano), Type: "command", Machine: machine, Label: c.Label,
			Script: c.Script, Sudo: c.Sudo, LoginShell: c.LoginShell, StdinBytes: len(c.Stdin), Code: r.Code,
			DurationMS: r.Duration.Milliseconds(), Stderr: string(r.Stderr)}
		if c.Secret {
			e.Stdout = "(not logged: file content)"
		} else {
			e.Stdout = string(r.Stdout)
		}
		if err != nil {
			e.Error = err.Error()
		}
		l.write(e)
	}
}

// Result writes the final result (the plan or the apply result). The engine already hid the
// secret values in it.
func (l *Log) Result(r *output.Run) {
	l.write(map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "type": "result", "result": r})
}

// Close writes the entries and closes the file. It returns the first write error.
func (l *Log) Close() error {
	l.flush()
	cerr := l.f.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	return cerr
}
