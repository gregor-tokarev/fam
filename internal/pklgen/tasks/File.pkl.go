// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

import "github.com/gregor-tokarev/fam/internal/pklgen/tasks/state"

type File interface {
	StateTask

	GetPath() string

	GetContent() *string

	GetMode() string
}

var _ File = FileImpl{}

// A file with a fixed content.
//
// ```
//
//	new fam.File {
//	  path = "~/.local/bin/hello"
//	  content = read("files/hello.sh").text
//	  mode = "0755"
//	}
//
// ```
type FileImpl struct {
	// The path of the file on the machine.
	Path string `pkl:"path"`

	// The full content of the file. Required when [state] is `"present"`; FAM stops otherwise.
	Content *string `pkl:"content"`

	// The file mode as an octal string, for example `"0644"`.
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
func (rcv FileImpl) GetPath() string {
	return rcv.Path
}

// The full content of the file. Required when [state] is `"present"`; FAM stops otherwise.
func (rcv FileImpl) GetContent() *string {
	return rcv.Content
}

// The file mode as an octal string, for example `"0644"`.
func (rcv FileImpl) GetMode() string {
	return rcv.Mode
}

// The wanted state.
//
// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
func (rcv FileImpl) GetState() state.State {
	return rcv.State
}

// Run the task with `sudo`.
//
// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
func (rcv FileImpl) GetSudo() bool {
	return rcv.Sudo
}

// Units that FAM restarts once, at the end of the playbook, when this task made a change.
//
// FAM does not restart units after a failed task.
// A unit managed by a [Service] task in the same playbook uses the scope of that task.
// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
func (rcv FileImpl) GetRestarts() []string {
	return rcv.Restarts
}
