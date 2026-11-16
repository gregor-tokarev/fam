// Code generated from Pkl module `fam.base`. DO NOT EDIT.
package base

import (
	"github.com/gregor-tokarev/fam/internal/pklgen/bundle"
	"github.com/gregor-tokarev/fam/internal/pklgen/tasks/state"
)

// A bundle with an explicit state.
//
// Use it in a `bundles` listing when the state is not `"present"`:
//
// ```
//
//	bundles {
//	  new { bundle = import("bundles/posthog.pkl"); state = "absent" }
//	}
//
// ```
type BundleRef struct {
	// The bundle module.
	Bundle bundle.Bundle `pkl:"bundle"`

	// The wanted state of the bundle.
	State state.State `pkl:"state"`
}
