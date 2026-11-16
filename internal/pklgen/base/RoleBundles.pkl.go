// Code generated from Pkl module `fam.base`. DO NOT EDIT.
package base

// The bundles of one role.
type RoleBundles struct {
	// Bundles for every machine with this role.
	//
	// Use `new { bundle = …; state = "absent" }` to remove a bundle of every machine from the
	// machines with this role.
	Bundles []any `pkl:"bundles"`
}
