// Code generated from Pkl module `fam.Bundle`. DO NOT EDIT.
package bundle

import "github.com/gregor-tokarev/fam/internal/pklgen/bundle/verifyevent"

// A script that proves that a bundle works.
type Verify struct {
	// A short name for output. When it is `null`, FAM uses the first line of the script.
	Name *string `pkl:"name"`

	// When the script runs.
	Event verifyevent.VerifyEvent `pkl:"event"`

	// A shell script. Exit code `0` means that the check passed.
	Script string `pkl:"script"`
}
