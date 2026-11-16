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
}

// Task is one unit of work on a machine.
type Task interface {
	// Group is the owner of the task, for example "bundle posthog" or "playbook worker".
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
	// Update returns changed=true when the machine changed.
	Update(ctx context.Context, env *Env) (changed bool, detail string, err error)
}

// Restarter is a task that restarts units at the end of the playbook when it changed.
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
