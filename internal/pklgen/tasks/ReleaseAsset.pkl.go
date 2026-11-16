// Code generated from Pkl module `fam.tasks`. DO NOT EDIT.
package tasks

// One release asset. FAM checks a SHA-256 checksum file when the release supplies one.
type ReleaseAsset struct {
	// The asset name. `{tag}` and `{version}` expand to the tag and the tag without `v`.
	Name string `pkl:"name"`

	// The asset format.
	Format string `pkl:"format"`

	// The binary path inside an archive. Required for `tar.gz`, `tar.xz`, and `zip`.
	// It also accepts `{tag}` and `{version}`.
	File *string `pkl:"file"`

	// The checksum asset name. The default finds common SHA-256 checksum file names.
	Checksum *string `pkl:"checksum"`
}
