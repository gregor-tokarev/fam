package engine

import (
	"context"
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

// Check is the result of a check.
type Check struct {
	Status Status
	Detail string
	// version is the installed version that the check found, for the update after it. It is
	// valid when versionKnown is true.
	version      string
	versionKnown bool
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
	// Update gets the satisfied check of the task, which is current.
	Update(ctx context.Context, env *Env, c Check) (Update, error)
}

// Update is the result of an update.
type Update struct {
	// Ran is false when the task has no update command: FAM ran nothing.
	Ran bool
	// Changed is true when the machine changed, for example a new version.
	Changed bool
	// Unknown is true when FAM cannot tell whether the update changed the machine.
	Unknown bool
	// Checked is true when the update already checked the task again after it.
	Checked bool
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
