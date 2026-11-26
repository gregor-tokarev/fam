// Package version holds the FAM version and the pinned tool versions.
package version

// Version is the FAM version. The release build sets it with -ldflags.
var Version = "0.23.1"

// PklVersion is the pinned version of the pkl binary.
const PklVersion = "0.33.0"
