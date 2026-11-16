// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

import "github.com/gregor-tokarev/fam/internal/pklgen/tasks/state"

type GitHubSkill interface {
	StateTask

	GetRepo() string

	GetPath() string

	GetRef() *string

	GetName() *string
}

var _ GitHubSkill = GitHubSkillImpl{}

// A skill folder in a public GitHub repository. The folder must contain `SKILL.md`.
//
// ```
// new fam.GitHubSkill { repo = "vercel-labs/agent-browser"; path = "skills/agent-browser" }
// ```
//
// On each run, the cockpit downloads the repository archive once (`codeload.github.com`) and
// copies the folder like a [Skill].
type GitHubSkillImpl struct {
	// The repository, `owner/name`.
	Repo string `pkl:"repo"`

	// The skill folder in the repository, for example `"skills/agent-browser"`.
	Path string `pkl:"path"`

	// A tag, branch, or commit. When it is `null`, FAM uses the tag of the latest GitHub release,
	// which is usually the latest version of the tool.
	Ref *string `pkl:"ref"`

	// The skill name. The default is the last part of [path].
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

// The repository, `owner/name`.
func (rcv GitHubSkillImpl) GetRepo() string {
	return rcv.Repo
}

// The skill folder in the repository, for example `"skills/agent-browser"`.
func (rcv GitHubSkillImpl) GetPath() string {
	return rcv.Path
}

// A tag, branch, or commit. When it is `null`, FAM uses the tag of the latest GitHub release,
// which is usually the latest version of the tool.
func (rcv GitHubSkillImpl) GetRef() *string {
	return rcv.Ref
}

// The skill name. The default is the last part of [path].
func (rcv GitHubSkillImpl) GetName() *string {
	return rcv.Name
}

// The wanted state.
//
// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
func (rcv GitHubSkillImpl) GetState() state.State {
	return rcv.State
}

// Run the task with `sudo`.
//
// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
func (rcv GitHubSkillImpl) GetSudo() bool {
	return rcv.Sudo
}

// Units that FAM restarts once, at the end of the bundle, when this task made a change.
//
// FAM does not restart units after a failed task.
// FAM does not restart units for a bundle with the state `"absent"`.
// A unit managed by a [Service] task in the same bundle uses the scope of that task (when the
// bundle has a user and a system Service task with this name, the rule below applies).
// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
func (rcv GitHubSkillImpl) GetRestarts() []string {
	return rcv.Restarts
}
