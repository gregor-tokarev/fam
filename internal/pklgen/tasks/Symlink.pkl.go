// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

import "github.com/gregor-tokarev/fam/internal/pklgen/tasks/state"

type Symlink interface {
	StateTask

	GetPath() string

	GetTarget() string
}

var _ Symlink = SymlinkImpl{}

// A symbolic link.
type SymlinkImpl struct {
	// The path of the link.
	Path string `pkl:"path"`

	// The target of the link.
	Target string `pkl:"target"`

	// The wanted state.
	//
	// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
	State state.State `pkl:"state"`

	// Run the task with `sudo`.
	//
	// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
	Sudo bool `pkl:"sudo"`

	// Units that FAM restarts once, at the end of the bundle, when this task made a change.
	//
	// FAM does not restart units after a failed task.
	// A unit managed by a [Service] task in the same bundle uses the scope of that task.
	// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
	Restarts []string `pkl:"restarts"`
}

// The path of the link.
func (rcv SymlinkImpl) GetPath() string {
	return rcv.Path
}

// The target of the link.
func (rcv SymlinkImpl) GetTarget() string {
	return rcv.Target
}

// The wanted state.
//
// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
func (rcv SymlinkImpl) GetState() state.State {
	return rcv.State
}

// Run the task with `sudo`.
//
// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
func (rcv SymlinkImpl) GetSudo() bool {
	return rcv.Sudo
}

// Units that FAM restarts once, at the end of the bundle, when this task made a change.
//
// FAM does not restart units after a failed task.
// A unit managed by a [Service] task in the same bundle uses the scope of that task.
// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
func (rcv SymlinkImpl) GetRestarts() []string {
	return rcv.Restarts
}
