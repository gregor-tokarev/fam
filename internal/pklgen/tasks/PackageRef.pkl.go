// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

import "github.com/gregor-tokarev/fam/internal/pklgen/tasks/manager"

// A package with its package manager.
//
// ```
// package = new { package = "agent-browser"; manager = "brew" }
// ```
type PackageRef struct {
	// The package name, for example `"agent-browser"` or `"@railway/cli"`.
	Package string `pkl:"package"`

	// The package manager.
	Manager manager.Manager `pkl:"manager"`

	// Package names for specific managers, for example `names { ["apt"] = "fd-find" }`.
	// FAM uses [package] when the selected manager has no name here.
	Names map[manager.Manager]string `pkl:"names"`
}
