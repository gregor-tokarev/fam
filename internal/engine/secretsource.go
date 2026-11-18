package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/gregor-tokarev/fam/internal/infisical"
	pklbase "github.com/gregor-tokarev/fam/internal/pklgen/base"
	"github.com/gregor-tokarev/fam/internal/secrets"
)

// secretSource is the result of reading the secrets of the config.
type secretSource struct {
	List []secrets.Secret
	// Hidden are other values to hide in output, for example the Infisical credentials.
	Hidden   []string
	Warnings []string
}

// loadSecrets reads the secrets from the source of the config: the secrets file or Infisical.
// offline never calls Infisical (fam check); it returns infisical.ErrNoCache without a cache.
func loadSecrets(ctx context.Context, sec *pklbase.Secrets, projectBase string, refresh, offline bool) (secretSource, error) {
	var out secretSource
	if sec.Delivery != "copy-file" {
		return out, fmt.Errorf("secrets delivery %q is not supported. Use \"copy-file\"", sec.Delivery)
	}
	switch {
	case sec.File != nil && sec.Infisical != nil:
		return out, errors.New("the secrets have a file and infisical. Set only one source")
	case sec.File != nil:
		list, err := secrets.Load(*sec.File, projectBase)
		out.List = list
		return out, err
	case sec.Infisical != nil:
		res, err := infisical.Load(ctx, infisical.Options{Source: sec.Infisical, ProjectBase: projectBase, Refresh: refresh, Offline: offline})
		out.List, out.Hidden, out.Warnings = res.Secrets, res.Hidden, res.Warnings
		return out, err
	}
	return out, errors.New("the secrets have no source. Set file or infisical")
}

// sourceFile is the file for a diagnostic about the secrets source: the secrets file, or the
// config for Infisical (its siteUrl can contain a password, so it is never a location).
func sourceFile(sec *pklbase.Secrets, configPath string) string {
	if sec.File != nil {
		return *sec.File
	}
	return configPath
}

// values returns the secret values and the other hidden values.
func (s secretSource) values() []string {
	out := make([]string, 0, len(s.List)+len(s.Hidden))
	for _, v := range s.List {
		out = append(out, v.Value)
	}
	return append(out, s.Hidden...)
}
