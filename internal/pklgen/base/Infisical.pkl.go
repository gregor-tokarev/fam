// Code generated from Pkl module `fam.base`. DO NOT EDIT.
package base

import "github.com/apple/pkl-go/pkl"

// A folder of an Infisical project as the source of the secrets.
//
// FAM reads the secrets on the cockpit and keeps them in a cache with mode `0600`
// (`<user cache dir>/fam/infisical`), so that runs inside [cache] do not call Infisical.
// When Infisical cannot answer, FAM uses the last secrets that it read and shows a warning.
// `fam check` never calls Infisical: it uses only the cache.
type Infisical struct {
	// The Infisical site, without `/api`. For example `"https://eu.infisical.com"`
	// or the address of a self-hosted Infisical. Plain `http://` is allowed only for localhost.
	// FAM does not follow redirects.
	SiteUrl string `pkl:"siteUrl"`

	// The ID of the Infisical project.
	ProjectId string `pkl:"projectId"`

	// The slug of the environment, for example `"prod"`.
	Environment string `pkl:"environment"`

	// The folder of the secrets.
	Path string `pkl:"path"`

	// Also read the secrets in the subfolders of [path]. A name must occur in only one folder.
	Recursive bool `pkl:"recursive"`

	// Also read the secrets that the folder imports. A secret of the folder wins over an imported secret.
	IncludeImports bool `pkl:"includeImports"`

	// A file on the cockpit with the credentials of FAM, as `NAME=value` lines.
	//
	// A machine identity with Universal Auth: `INFISICAL_UNIVERSAL_AUTH_CLIENT_ID` and
	// `INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET`. Or an access token: `INFISICAL_TOKEN`.
	// The file must have mode `0600` and must not be inside the project base directory.
	// When it is `null`, FAM reads these variables from the environment of `fam`.
	// `fam check` also needs the credentials: they select the cache.
	Credentials *string `pkl:"credentials"`

	// How long FAM uses the secrets that it read before it reads them again.
	// `0.s` reads them on each run. `--refresh-secrets` ignores the cache for one run.
	Cache pkl.Duration `pkl:"cache"`
}
