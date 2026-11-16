// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

import (
	"github.com/apple/pkl-go/pkl"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks/state"
)

type Merge interface {
	StateTask

	GetPath() string

	GetFormat() string

	GetValues() pkl.Object

	GetMode() string
}

var _ Merge = MergeImpl{}

// Keys merged into a JSON or TOML file.
//
// FAM keeps all other keys of the file.
// When [state] is `"absent"`, FAM removes the keys of [values] from the file.
//
// ```
//
//	new fam.Merge {
//	  path = "~/.cargo/config.toml"
//	  format = "toml"
//	  values { build { jobs = 4 } }
//	}
//
// ```
type MergeImpl struct {
	// The path of the file on the machine.
	Path string `pkl:"path"`

	// The format of the file.
	Format string `pkl:"format"`

	// The keys to merge. Nested objects merge recursively. Lists replace the old value.
	Values pkl.Object `pkl:"values"`

	// The mode of a new file.
	Mode string `pkl:"mode"`

	// The wanted state.
	//
	// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
	State state.State `pkl:"state"`

	// Run the task with `sudo`.
	//
	// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
	Sudo bool `pkl:"sudo"`

	// Units that FAM restarts once, at the end of the playbook, when this task made a change.
	//
	// FAM does not restart units after a failed task.
	// A unit managed by a [Service] task in the same playbook uses the scope of that task.
	// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
	Restarts []string `pkl:"restarts"`
}

// The path of the file on the machine.
func (rcv MergeImpl) GetPath() string {
	return rcv.Path
}

// The format of the file.
func (rcv MergeImpl) GetFormat() string {
	return rcv.Format
}

// The keys to merge. Nested objects merge recursively. Lists replace the old value.
func (rcv MergeImpl) GetValues() pkl.Object {
	return rcv.Values
}

// The mode of a new file.
func (rcv MergeImpl) GetMode() string {
	return rcv.Mode
}

// The wanted state.
//
// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
func (rcv MergeImpl) GetState() state.State {
	return rcv.State
}

// Run the task with `sudo`.
//
// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
func (rcv MergeImpl) GetSudo() bool {
	return rcv.Sudo
}

// Units that FAM restarts once, at the end of the playbook, when this task made a change.
//
// FAM does not restart units after a failed task.
// A unit managed by a [Service] task in the same playbook uses the scope of that task.
// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
func (rcv MergeImpl) GetRestarts() []string {
	return rcv.Restarts
}
