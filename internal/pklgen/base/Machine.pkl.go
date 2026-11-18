// Code generated from Pkl module `fam.base`. DO NOT EDIT.
package base

// One machine of the fleet.
type Machine struct {
	// The machine that runs FAM. FAM runs its commands locally, not over SSH.
	//
	// At most one machine can be the cockpit. All other machines are workers.
	Cockpit bool `pkl:"cockpit"`

	// The tags of the machine, for example `tags { "worker"; "gpu" }`.
	//
	// The machine gets the bundles of each tag in `tags` of `config.pkl`, in the order of this list.
	// A tag must be in `tags` of `config.pkl`, also when it has no bundles.
	Tags []string `pkl:"tags"`

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
	// An entry here wins over the bundle with the same `name` in `bundles` or `tags` of `config.pkl`.
	// Use `new { bundle = …; state = "absent" }` to remove such a bundle from this machine.
	Bundles []any `pkl:"bundles"`
}
