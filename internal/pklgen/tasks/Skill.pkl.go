// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

import "github.com/gregor-tokarev/fam/internal/pklgen/tasks/state"

type Skill interface {
	StateTask

	GetPath() string

	GetName() *string
}

var _ Skill = SkillImpl{}

// A skill folder of this bundle, relative to the bundle file. The folder must contain `SKILL.md`.
//
// ```
// new fam.Skill { path = "skills/release-notes" }
// ```
//
// FAM copies it to `~/.agents/skills/<name>`, marks it as managed, and links
// `~/.claude/skills/<name>` to it. With `state = "absent"`, FAM removes the copy and the link,
// but only when FAM made them.
type SkillImpl struct {
	// The folder, relative to the bundle file.
	Path string `pkl:"path"`

	// The skill name. The default is the folder name, and a folder named `skill` gets the name
	// of the bundle.
	Name *string `pkl:"name"`

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
	// FAM does not restart units for a bundle with the state `"absent"`.
	// A unit managed by a [Service] task in the same bundle uses the scope of that task (when the
	// bundle has a user and a system Service task with this name, the rule below applies).
	// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
	Restarts []string `pkl:"restarts"`
}

// The folder, relative to the bundle file.
func (rcv SkillImpl) GetPath() string {
	return rcv.Path
}

// The skill name. The default is the folder name, and a folder named `skill` gets the name
// of the bundle.
func (rcv SkillImpl) GetName() *string {
	return rcv.Name
}

// The wanted state.
//
// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
func (rcv SkillImpl) GetState() state.State {
	return rcv.State
}

// Run the task with `sudo`.
//
// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
func (rcv SkillImpl) GetSudo() bool {
	return rcv.Sudo
}

// Units that FAM restarts once, at the end of the bundle, when this task made a change.
//
// FAM does not restart units after a failed task.
// FAM does not restart units for a bundle with the state `"absent"`.
// A unit managed by a [Service] task in the same bundle uses the scope of that task (when the
// bundle has a user and a system Service task with this name, the rule below applies).
// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
func (rcv SkillImpl) GetRestarts() []string {
	return rcv.Restarts
}
