// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

import "github.com/gregor-tokarev/fam/internal/pklgen/tasks/state"

type GitHubRelease interface {
	StateTask

	GetRepo() string

	GetBinary() string

	GetVersionCommand() *string

	GetAssets() map[string]ReleaseAsset

	GetTag() *string
}

var _ GitHubRelease = GitHubReleaseImpl{}

// A binary from a public GitHub release, installed into `~/.local/bin` without sudo.
// FAM records each installed binary and its SHA-256. An existing binary without a record is a conflict.
// FAM replaces a managed binary after a hand change and compares its version with the release tag.
// Prefer static or musl builds for the plain Linux keys when available, because they run everywhere.
type GitHubReleaseImpl struct {
	// The repository, `owner/name`.
	Repo string `pkl:"repo"`

	// The name of the installed binary.
	Binary string `pkl:"binary"`

	// A command that prints the installed version. The default is `<binary> --version`.
	// Its output must contain the release tag as a word, with or without the leading `v`.
	VersionCommand *string `pkl:"versionCommand"`

	// Assets by platform, for example `assets { ["linux-amd64"] { name = "tool.tar.gz"; file = "tool" } }`.
	// On musl Linux, FAM uses the matching `-musl` key when present, else the plain Linux key.
	Assets map[string]ReleaseAsset `pkl:"assets"`

	// A pinned release tag. The default is the latest release.
	Tag *string `pkl:"tag"`

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
func (rcv GitHubReleaseImpl) GetRepo() string {
	return rcv.Repo
}

// The name of the installed binary.
func (rcv GitHubReleaseImpl) GetBinary() string {
	return rcv.Binary
}

// A command that prints the installed version. The default is `<binary> --version`.
// Its output must contain the release tag as a word, with or without the leading `v`.
func (rcv GitHubReleaseImpl) GetVersionCommand() *string {
	return rcv.VersionCommand
}

// Assets by platform, for example `assets { ["linux-amd64"] { name = "tool.tar.gz"; file = "tool" } }`.
// On musl Linux, FAM uses the matching `-musl` key when present, else the plain Linux key.
func (rcv GitHubReleaseImpl) GetAssets() map[string]ReleaseAsset {
	return rcv.Assets
}

// A pinned release tag. The default is the latest release.
func (rcv GitHubReleaseImpl) GetTag() *string {
	return rcv.Tag
}

// The wanted state.
//
// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
func (rcv GitHubReleaseImpl) GetState() state.State {
	return rcv.State
}

// Run the task with `sudo`.
//
// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
func (rcv GitHubReleaseImpl) GetSudo() bool {
	return rcv.Sudo
}

// Units that FAM restarts once, at the end of the bundle, when this task made a change.
//
// FAM does not restart units after a failed task.
// FAM does not restart units for a bundle with the state `"absent"`.
// A unit managed by a [Service] task in the same bundle uses the scope of that task (when the
// bundle has a user and a system Service task with this name, the rule below applies).
// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
func (rcv GitHubReleaseImpl) GetRestarts() []string {
	return rcv.Restarts
}
