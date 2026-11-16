// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

import "github.com/gregor-tokarev/fam/internal/pklgen/tasks/state"

type Linger interface {
	StateTask

	GetUser() *string
}

var _ Linger = LingerImpl{}

// Lingering for the SSH user on Linux, so that user units run without a login session.
//
// The default of [sudo] is `true`, because `loginctl enable-linger` usually needs root.
type LingerImpl struct {
	// Run the task with `sudo`.
	//
	// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
	Sudo bool `pkl:"sudo"`

	// The user. When it is `null`, FAM uses the SSH user.
	User *string `pkl:"user"`

	// The wanted state.
	//
	// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
	State state.State `pkl:"state"`

	// Units that FAM restarts once, at the end of the playbook, when this task made a change.
	//
	// FAM does not restart units after a failed task.
	// A unit managed by a [Service] task in the same playbook uses the scope of that task.
	// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
	Restarts []string `pkl:"restarts"`
}

// Run the task with `sudo`.
//
// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
func (rcv LingerImpl) GetSudo() bool {
	return rcv.Sudo
}

// The user. When it is `null`, FAM uses the SSH user.
func (rcv LingerImpl) GetUser() *string {
	return rcv.User
}

// The wanted state.
//
// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
func (rcv LingerImpl) GetState() state.State {
	return rcv.State
}

// Units that FAM restarts once, at the end of the playbook, when this task made a change.
//
// FAM does not restart units after a failed task.
// A unit managed by a [Service] task in the same playbook uses the scope of that task.
// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
func (rcv LingerImpl) GetRestarts() []string {
	return rcv.Restarts
}
