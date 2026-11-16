// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

type Script interface {
	Task

	GetName() string

	GetCheck() *string

	GetCreates() *string

	GetApply() string
}

var _ Script = ScriptImpl{}

// A shell script with a check.
//
// FAM cannot undo a script.
//
// Exit codes of [check]:
// - `0`: satisfied. FAM does not run [apply].
// - `100`: changes required. FAM runs [apply], then runs [check] again.
// - any other code: error. FAM never runs [apply] after an error.
//
// FAM rejects a script without [check] or [creates].
type ScriptImpl struct {
	// The name of the script in output.
	Name string `pkl:"name"`

	// The check command. See the exit codes above.
	Check *string `pkl:"check"`

	// A short form of a check: the script is satisfied when this path exists.
	Creates *string `pkl:"creates"`

	// The apply command.
	Apply string `pkl:"apply"`

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

// The name of the script in output.
func (rcv ScriptImpl) GetName() string {
	return rcv.Name
}

// The check command. See the exit codes above.
func (rcv ScriptImpl) GetCheck() *string {
	return rcv.Check
}

// A short form of a check: the script is satisfied when this path exists.
func (rcv ScriptImpl) GetCreates() *string {
	return rcv.Creates
}

// The apply command.
func (rcv ScriptImpl) GetApply() string {
	return rcv.Apply
}

// Run the task with `sudo`.
//
// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
func (rcv ScriptImpl) GetSudo() bool {
	return rcv.Sudo
}

// Units that FAM restarts once, at the end of the bundle, when this task made a change.
//
// FAM does not restart units after a failed task.
// A unit managed by a [Service] task in the same bundle uses the scope of that task.
// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
func (rcv ScriptImpl) GetRestarts() []string {
	return rcv.Restarts
}
