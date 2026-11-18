// Code generated from Pkl module `fam.base`. DO NOT EDIT.
package base

// The bundles of one tag.
type TagBundles struct {
	// Bundles for every machine with this tag.
	//
	// Use `new { bundle = …; state = "absent" }` to remove a bundle of every machine from the
	// machines with this tag.
	Bundles []any `pkl:"bundles"`
}
