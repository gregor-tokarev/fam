// Code generated from Pkl module `fam.base`. DO NOT EDIT.
package base

// How FAM gives secrets to the machines.
type Secrets struct {
	// The delivery method. Version 0 supports only `"copy-file"`.
	//
	// FAM copies the full file to every machine over SSH, writes it with mode `0600`,
	// and exports every variable in new shells.
	Delivery string `pkl:"delivery"`

	// The secrets file on the cockpit, with one `NAME=value` line for each secret.
	//
	// The file must have mode `0600` and must not be inside the project base directory.
	File string `pkl:"file"`
}
