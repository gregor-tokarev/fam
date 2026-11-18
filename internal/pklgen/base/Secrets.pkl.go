// Code generated from Pkl module `fam.base`. DO NOT EDIT.
package base

// How FAM gives secrets to the machines.
//
// The secrets come from one source: a file on the cockpit (`file`) or Infisical (`infisical`).
// Set exactly one of them.
type Secrets struct {
	// The delivery method. Version 0 supports only `"copy-file"`.
	//
	// FAM writes all secrets into one file on every machine over SSH, with mode `0600`,
	// and exports every variable in new shells.
	Delivery string `pkl:"delivery"`

	// The secrets file on the cockpit, with one `NAME=value` line for each secret.
	//
	// The file must have mode `0600` and must not be inside the project base directory.
	// Keep it `null` when the secrets come from [infisical].
	File *string `pkl:"file"`

	// Read the secrets from Infisical instead of a file.
	//
	// ```
	// secrets {
	//   infisical {
	//     projectId = "6f1c…"
	//     environment = "prod"
	//     credentials = "~/.config/fam-secrets/infisical.env"
	//   }
	// }
	// ```
	Infisical *Infisical `pkl:"infisical"`
}
