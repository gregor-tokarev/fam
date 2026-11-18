// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

import "github.com/gregor-tokarev/fam/internal/pklgen/tasks/state"

type AgentSkill interface {
	StateTask

	GetSource() string

	GetPath() *string

	GetRef() *string

	GetName() *string
}

var _ AgentSkill = AgentSkillImpl{}

// An agent skill: a folder with `SKILL.md`, from a folder on the cockpit or from a URL.
//
// ```
// new fam.AgentSkill { source = "skills/release-notes" }
// new fam.AgentSkill { source = "https://github.com/vercel-labs/agent-browser"; path = "skills/agent-browser" }
// new fam.AgentSkill { source = "https://github.com/anthropics/skills/tree/main/skills/pdf" }
// new fam.AgentSkill { source = "https://example.com/skills/review.zip" }
// new fam.AgentSkill { source = "https://example.com/skills/review/SKILL.md" }
// ```
//
// FAM copies the skill to `~/.agents/skills/<name>`, marks it as managed, and links
// `~/.claude/skills/<name>` to it. With `state = "absent"`, FAM removes the copy and the link,
// but only when FAM made them. Skill tasks run as the SSH user: `sudo = true` is an error.
// `restarts` restarts the units when the skill changes.
type AgentSkillImpl struct {
	// Where FAM gets the skill. The cockpit reads or downloads it once in each run.
	//
	// - A folder on the cockpit: a path relative to the bundle file, or an absolute path.
	// - A public GitHub repository: `https://github.com/<owner>/<repo>`, with the folder in [path]
	//   and the version in [ref]. A `/tree/<ref>/<folder>` URL sets both (the ref is one part).
	// - A `.tar.gz`, `.tgz`, or `.zip` archive at an `https://` URL, with the folder in [path].
	//   When all files of the archive are in one top folder, [path] is relative to that folder.
	// - A `.md` file at an `https://` URL: a skill of one file. FAM saves it as `SKILL.md`.
	//
	// A bundle for every machine, a tag, or one machine selects its skills, so the skills can be
	// different on each machine.
	Source string `pkl:"source"`

	// The skill folder in a GitHub repository or an archive. The default is the top of the
	// repository or the archive.
	Path *string `pkl:"path"`

	// For a GitHub repository: a tag, branch, or commit. When it is `null`, FAM uses the tag of
	// the latest GitHub release, which is usually the latest version of the tool.
	Ref *string `pkl:"ref"`

	// The skill name. The default is the last part of [path], or else the last part of [source]
	// (without the archive or `.md` extension; for a `SKILL.md` file, its folder). A local folder
	// named `skill` gets the name of the bundle.
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

// Where FAM gets the skill. The cockpit reads or downloads it once in each run.
//
//   - A folder on the cockpit: a path relative to the bundle file, or an absolute path.
//   - A public GitHub repository: `https://github.com/<owner>/<repo>`, with the folder in [path]
//     and the version in [ref]. A `/tree/<ref>/<folder>` URL sets both (the ref is one part).
//   - A `.tar.gz`, `.tgz`, or `.zip` archive at an `https://` URL, with the folder in [path].
//     When all files of the archive are in one top folder, [path] is relative to that folder.
//   - A `.md` file at an `https://` URL: a skill of one file. FAM saves it as `SKILL.md`.
//
// A bundle for every machine, a tag, or one machine selects its skills, so the skills can be
// different on each machine.
func (rcv AgentSkillImpl) GetSource() string {
	return rcv.Source
}

// The skill folder in a GitHub repository or an archive. The default is the top of the
// repository or the archive.
func (rcv AgentSkillImpl) GetPath() *string {
	return rcv.Path
}

// For a GitHub repository: a tag, branch, or commit. When it is `null`, FAM uses the tag of
// the latest GitHub release, which is usually the latest version of the tool.
func (rcv AgentSkillImpl) GetRef() *string {
	return rcv.Ref
}

// The skill name. The default is the last part of [path], or else the last part of [source]
// (without the archive or `.md` extension; for a `SKILL.md` file, its folder). A local folder
// named `skill` gets the name of the bundle.
func (rcv AgentSkillImpl) GetName() *string {
	return rcv.Name
}

// The wanted state.
//
// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
func (rcv AgentSkillImpl) GetState() state.State {
	return rcv.State
}

// Run the task with `sudo`.
//
// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
func (rcv AgentSkillImpl) GetSudo() bool {
	return rcv.Sudo
}

// Units that FAM restarts once, at the end of the bundle, when this task made a change.
//
// FAM does not restart units after a failed task.
// FAM does not restart units for a bundle with the state `"absent"`.
// A unit managed by a [Service] task in the same bundle uses the scope of that task (when the
// bundle has a user and a system Service task with this name, the rule below applies).
// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
func (rcv AgentSkillImpl) GetRestarts() []string {
	return rcv.Restarts
}
