// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

import "github.com/gregor-tokarev/fam/internal/pklgen/tasks/state"

type Service interface {
	StateTask

	GetName() string

	GetScope() string

	GetContent() *string

	GetEnabled() bool

	GetRunning() bool
}

var _ Service = ServiceImpl{}

// A system service: a systemd unit on Linux, a launchd job on macOS.
//
// FAM stops with an error on a machine without systemd or launchd.
type ServiceImpl struct {
	// The unit name, for example `"fleet-ports.service"`, or the launchd label.
	Name string `pkl:"name"`

	// `"user"` for a user unit or a LaunchAgent. `"system"` for a system unit or a LaunchDaemon.
	//
	// System units need `sudo = true`.
	Scope string `pkl:"scope"`

	// The unit file or the launchd property list.
	//
	// When it is `null`, FAM does not write a file and only enables and starts the unit.
	Content *string `pkl:"content"`

	// Start the unit at boot or login.
	Enabled bool `pkl:"enabled"`

	// Keep the unit running.
	Running bool `pkl:"running"`

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

// The unit name, for example `"fleet-ports.service"`, or the launchd label.
func (rcv ServiceImpl) GetName() string {
	return rcv.Name
}

// `"user"` for a user unit or a LaunchAgent. `"system"` for a system unit or a LaunchDaemon.
//
// System units need `sudo = true`.
func (rcv ServiceImpl) GetScope() string {
	return rcv.Scope
}

// The unit file or the launchd property list.
//
// When it is `null`, FAM does not write a file and only enables and starts the unit.
func (rcv ServiceImpl) GetContent() *string {
	return rcv.Content
}

// Start the unit at boot or login.
func (rcv ServiceImpl) GetEnabled() bool {
	return rcv.Enabled
}

// Keep the unit running.
func (rcv ServiceImpl) GetRunning() bool {
	return rcv.Running
}

// The wanted state.
//
// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
func (rcv ServiceImpl) GetState() state.State {
	return rcv.State
}

// Run the task with `sudo`.
//
// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
func (rcv ServiceImpl) GetSudo() bool {
	return rcv.Sudo
}

// Units that FAM restarts once, at the end of the bundle, when this task made a change.
//
// FAM does not restart units after a failed task.
// A unit managed by a [Service] task in the same bundle uses the scope of that task.
// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
func (rcv ServiceImpl) GetRestarts() []string {
	return rcv.Restarts
}
