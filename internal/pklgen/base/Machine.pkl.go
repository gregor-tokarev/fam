// Code generated from Pkl module `fam.base`. DO NOT EDIT.
package base

import "github.com/gregor-tokarev/fam/internal/pklgen/base/role"

// One machine of the fleet.
type Machine struct {
	// The role of the machine.
	//
	// Exactly one machine can have the role `"cockpit"`.
	Role role.Role `pkl:"role"`

	// The SSH host name or IP address of a worker.
	//
	// FAM connects to `user@address` with the system `ssh` command,
	// so entries of `~/.ssh/config` apply.
	// A worker must have an address. The cockpit has no address.
	Address *string `pkl:"address"`

	// The SSH user. When it is `null`, `ssh` selects the user.
	User *string `pkl:"user"`

	// Bundles for this machine only.
	//
	// An entry here wins over the bundle with the same `name` in `bundles` or `roles` of `config.pkl`.
	// Use `new { bundle = …; state = "absent" }` to remove such a bundle from this machine.
	Bundles []any `pkl:"bundles"`
}
