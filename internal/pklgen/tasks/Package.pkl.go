// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

import "github.com/gregor-tokarev/fam/internal/pklgen/tasks/state"

type Package interface {
	StateTask

	GetPackage() *any

	GetBinary() *string

	GetInstall() *string

	GetUpdate() *string

	GetUninstall() *string

	GetVersionCommand() *string
}

var _ Package = PackageImpl{}

// A command line program.
//
// Give a [package], and FAM installs, updates, and removes it with a package manager:
//
// ```
// new fam.Package { package = "agent-browser" }                                   // npm
// new fam.Package { package = new { package = "agent-browser"; manager = "brew" } }
// ```
//
// Or give your own commands: [install], [update], [uninstall].
//
// FAM installs the program when it is missing, and updates it on each `fam apply`.
// An update changes nothing in the result when the output of [versionCommand] stays the same.
type PackageImpl struct {
	// The package. A string is an npm package. An object (`new { package = …; manager = … }`
	// or a [PackageRef]) selects the package manager.
	Package *any `pkl:"package"`

	// The name of the program in `PATH`, or its path (for example `"~/.local/bin/tool"`).
	//
	// When it is `null`, FAM uses the package name without its npm scope (`@railway/cli` gives `cli`).
	Binary *string `pkl:"binary"`

	// The install command. Required when [package] is `null`.
	Install *string `pkl:"install"`

	// The update command. When it is `null` and [package] is `null`, FAM does not update the program.
	Update *string `pkl:"update"`

	// The uninstall command. Required to remove a package without [package].
	Uninstall *string `pkl:"uninstall"`

	// A command that prints the installed version. When it is `null`, FAM runs `<binary> --version`.
	VersionCommand *string `pkl:"versionCommand"`

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

// The package. A string is an npm package. An object (`new { package = …; manager = … }`
// or a [PackageRef]) selects the package manager.
func (rcv PackageImpl) GetPackage() *any {
	return rcv.Package
}

// The name of the program in `PATH`, or its path (for example `"~/.local/bin/tool"`).
//
// When it is `null`, FAM uses the package name without its npm scope (`@railway/cli` gives `cli`).
func (rcv PackageImpl) GetBinary() *string {
	return rcv.Binary
}

// The install command. Required when [package] is `null`.
func (rcv PackageImpl) GetInstall() *string {
	return rcv.Install
}

// The update command. When it is `null` and [package] is `null`, FAM does not update the program.
func (rcv PackageImpl) GetUpdate() *string {
	return rcv.Update
}

// The uninstall command. Required to remove a package without [package].
func (rcv PackageImpl) GetUninstall() *string {
	return rcv.Uninstall
}

// A command that prints the installed version. When it is `null`, FAM runs `<binary> --version`.
func (rcv PackageImpl) GetVersionCommand() *string {
	return rcv.VersionCommand
}

// The wanted state.
//
// When a bundle has the state `"absent"`, FAM undoes all its typed tasks.
func (rcv PackageImpl) GetState() state.State {
	return rcv.State
}

// Run the task with `sudo`.
//
// On a worker, FAM runs `sudo -n`. On the cockpit, FAM asks for the password once.
func (rcv PackageImpl) GetSudo() bool {
	return rcv.Sudo
}

// Units that FAM restarts once, at the end of the playbook, when this task made a change.
//
// FAM does not restart units after a failed task.
// A unit managed by a [Service] task in the same playbook uses the scope of that task.
// Other units use the system scope when [sudo] is `true`, and the user scope when it is `false`.
func (rcv PackageImpl) GetRestarts() []string {
	return rcv.Restarts
}
