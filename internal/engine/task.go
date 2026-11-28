package engine

import (
	"context"
	"encoding/base64"
	"os"

	"github.com/gregor-tokarev/fam/internal/transport"
)

// Status is the result of a check.
type Status string

// Check statuses.
const (
	Satisfied Status = "satisfied"
	Change    Status = "change"
	Conflict  Status = "conflict"
	Error     Status = "error"
	Skipped   Status = "skipped"
)

// Apply statuses.
const (
	Unchanged Status = "unchanged"
	Changed   Status = "changed"
	Failed    Status = "failed"
)

// PrimPlan is an optional Go-computed dumb primitive produced directly by Task.Check so the
// apply loop can pipeline independent file/symlink/skill mutations in one remote round trip
// (guarded by CAS preconditions) while keeping 100% of task logic and error rules in Go.
type PrimPlan struct {
	Op         transport.PrimOp
	TargetPath string
	Data       []byte // raw bytes for write
	WantSHA256 string
	WantMode   os.FileMode
	WantTarget string
	WantDigest string
	SudoKind   string
	// Tarball builds the skill tarball lazily only during apply (never during plan).
	Tarball func() ([]byte, error)
}

func writePrimPlan(p string, data []byte, mode os.FileMode, sudo, secret bool, ifMissing bool, ifSHA256, sudoKind string) *PrimPlan {
	m := uint32(mode.Perm())
	return &PrimPlan{
		Op: transport.PrimOp{
			Op:        "write",
			Label:     "write " + p,
			Secret:    secret,
			Path:      p,
			Mode:      &m,
			Sudo:      sudo,
			Verify:    true,
			IfMissing: ifMissing,
			IfSHA256:  ifSHA256,
		},
		TargetPath: p,
		Data:       data,
		WantSHA256: sha(data),
		WantMode:   mode.Perm(),
		SudoKind:   sudoKind,
	}
}

func (p *PrimPlan) finalizeOp(index int, stopAfter bool) (transport.PrimOp, error) {
	op := p.Op
	op.Index = index
	op.StopAfter = stopAfter
	if op.Op == "write" && op.DataB64 == "" && len(p.Data) > 0 {
		op.DataB64 = base64.StdEncoding.EncodeToString(p.Data)
	}
	if op.Op == "skill_write" && op.TarB64 == "" && p.Tarball != nil {
		tb, err := p.Tarball()
		if err != nil {
			return transport.PrimOp{}, err
		}
		op.TarB64 = base64.StdEncoding.EncodeToString(tb)
	}
	return op, nil
}

// Check is the result of a check.
type Check struct {
	Status Status
	Detail string
	// Plan is optionally populated by Task.Check when Status == Change and the apply can run
	// as a CAS-guarded primitive write/symlink/skill_write without re-reading state.
	Plan *PrimPlan
}

// Task is one unit of work on a machine.
type Task interface {
	// Group is the owner of the task, for example "bundle rust" or "environment".
	Group() string
	// Kind is the task type, for example "file".
	Kind() string
	// Name describes the task, for example the path.
	Name() string
	// Check finds the difference between the machine and the config. It changes nothing.
	Check(ctx context.Context, env *Env) (Check, error)
	// Apply makes the change. It returns a short description.
	Apply(ctx context.Context, env *Env) (string, error)
}

// Updater is a task that also runs on apply when its check is satisfied, for example a package update.
type Updater interface {
	// Before reads what Update compares, for example the version. It changes nothing. FAM runs it
	// only after a satisfied check. err is only a connection error: an unknown value is "".
	Before(ctx context.Context, env *Env) (string, error)
	// Updates is true when Update runs a command. Else Update changes nothing.
	Updates() bool
	// Update runs after a satisfied check. before is the result of Before.
	Update(ctx context.Context, env *Env, before string) (Update, error)
}

// Update is the result of an update. FAM treats each update that ran as a possible change of the
// machine: an update script can change files that other tasks own, also without a new version.
type Update struct {
	// Ran is false when the task has no update command, or FAM skipped it: FAM ran nothing.
	// Detail then tells why FAM skipped it, or is "".
	Ran bool
	// Changed is true when the version changed.
	Changed bool
	// Checked is true when the update already did the full check of the task after it. Diff is
	// then the difference that this check found, or "".
	Checked bool
	Diff    string
	Detail  string
}

// Restarter is a task that restarts units at the end of its bundle when it changed.
type Restarter interface {
	Restarts() []Unit
}

// Unit is a service unit that FAM can restart.
type Unit struct {
	Name  string
	Scope string // "user" or "system"
}

type base struct {
	group string
	sudo  bool
}

func (b base) Group() string { return b.group }

// CheckError marks an error of a check that is a conflict.
type CheckError struct {
	Conflict bool
	Msg      string
}

func (e *CheckError) Error() string { return e.Msg }

func conflict(msg string) error { return &CheckError{Conflict: true, Msg: msg} }
