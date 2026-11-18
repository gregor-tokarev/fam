package infisical

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gregor-tokarev/fam/internal/pklgen/base"
	"github.com/gregor-tokarev/fam/internal/secrets"
)

// The names of the credentials, the same names that the Infisical CLI reads.
const (
	EnvClientID     = "INFISICAL_UNIVERSAL_AUTH_CLIENT_ID"
	EnvClientSecret = "INFISICAL_UNIVERSAL_AUTH_CLIENT_SECRET"
	EnvToken        = "INFISICAL_TOKEN"
)

// ErrNoCache means that an offline read found no cached secrets.
var ErrNoCache = errors.New("FAM has not read the secrets from Infisical yet, so there is no cache")

// Credentials sign FAM in to Infisical: a machine identity with Universal Auth, or an access token.
type Credentials struct {
	ClientID     string
	ClientSecret string
	Token        string
}

// identity names the full credentials in the cache keys with a hash, so that new credentials
// (also a new client secret for the same client ID) do not use the cache of the old ones.
func (c Credentials) identity() string {
	if c.ClientID != "" {
		sum := sha256.Sum256([]byte(c.ClientID + "\x00" + c.ClientSecret))
		return "universal-auth:" + hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256([]byte(c.Token))
	return "token:" + hex.EncodeToString(sum[:])
}

// ReadCredentials reads the credentials from the file when it is not empty (with the rules of a
// secrets file), and else from the environment. The file wins, so that a token in the shell for
// other tools does not change what FAM reads.
func ReadCredentials(file, projectBase string, getenv func(string) string) (Credentials, error) {
	vals := map[string]string{}
	where := "the environment"
	if file != "" {
		where = file
		list, err := secrets.Load(file, projectBase)
		if err != nil {
			return Credentials{}, fmt.Errorf("Infisical credentials: %w", err)
		}
		for _, s := range list {
			vals[s.Name] = strings.TrimSpace(s.Value)
		}
	} else {
		for _, n := range []string{EnvClientID, EnvClientSecret, EnvToken} {
			vals[n] = strings.TrimSpace(getenv(n))
		}
	}
	c := Credentials{ClientID: vals[EnvClientID], ClientSecret: vals[EnvClientSecret], Token: vals[EnvToken]}
	switch {
	case c.ClientID != "" && c.ClientSecret != "":
		c.Token = ""
		return c, nil
	case c.ClientID != "" || c.ClientSecret != "":
		return Credentials{}, fmt.Errorf("Infisical credentials: %s has only one of %s and %s. Set both", where, EnvClientID, EnvClientSecret)
	case c.Token != "":
		return c, nil
	}
	return Credentials{}, fmt.Errorf("Infisical credentials: %s has no %s and %s, and no %s. Set them for a machine identity of the project", where, EnvClientID, EnvClientSecret, EnvToken)
}

// Options are the inputs of Load.
type Options struct {
	Source *base.Infisical
	// ProjectBase is the base of the FAM project: the credentials file must be outside it.
	ProjectBase string
	// Refresh reads from Infisical also when the cache is fresh.
	Refresh bool
	// Offline never calls Infisical: it uses the cache of any age, or returns ErrNoCache.
	Offline bool
	// CacheDir is the cache directory. Empty means DefaultCacheDir.
	CacheDir string
	Getenv   func(string) string
	Now      func() time.Time
	Client   *Client
}

// Result is the secrets and what the caller must hide and show.
type Result struct {
	Secrets []secrets.Secret
	// Hidden are values that FAM must hide in output, besides the secret values: the credentials
	// and the access token.
	Hidden []string
	// Warnings are names-only messages, for example about an old cache.
	Warnings []string
}

// Load returns the secrets of the source. Inside the cache time it uses the cache. Then it reads
// them from Infisical and saves them. When Infisical does not answer (network, 429, 5xx), it uses
// the cache of any age with a warning.
func Load(ctx context.Context, o Options) (Result, error) {
	src := o.Source
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.CacheDir == "" {
		d, err := DefaultCacheDir()
		if err != nil {
			return Result{}, fmt.Errorf("Infisical cache: %w", err)
		}
		o.CacheDir = d
	}
	if err := CheckSite(src.SiteUrl); err != nil {
		return Result{}, err
	}
	if o.Client == nil {
		o.Client = NewClient(src.SiteUrl)
	}
	credFile := ""
	if src.Credentials != nil {
		credFile = *src.Credentials
		if strings.TrimSpace(credFile) == "" {
			return Result{}, errors.New("Infisical credentials: the path is empty. Set a file, or remove credentials to read the environment")
		}
	}
	creds, err := ReadCredentials(credFile, o.ProjectBase, o.Getenv)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, v := range []string{creds.ClientSecret, creds.Token} {
		if v != "" {
			res.Hidden = append(res.Hidden, v)
		}
	}
	site := strings.TrimRight(src.SiteUrl, "/")
	cache := Cache{Dir: o.CacheDir}
	folder := Folder{ProjectID: src.ProjectId, Environment: src.Environment, Path: src.Path, Recursive: src.Recursive, IncludeImports: src.IncludeImports}
	key := cacheKey(site, creds.identity(), folder.ProjectID, folder.Environment, folder.Path, fmt.Sprint(folder.Recursive), fmt.Sprint(folder.IncludeImports))
	tokenKey := cacheKey(site, creds.identity())
	now := o.Now()
	cached, at, haveCache := cache.ReadSecrets(key)
	age := now.Sub(at)
	ttl := src.Cache.GoDuration()
	if haveCache && (o.Offline || (!o.Refresh && age >= 0 && age < ttl)) {
		res.Secrets = cached
		return res, nil
	}
	if o.Offline {
		return res, ErrNoCache
	}
	list, token, err := fetch(ctx, o.Client, cache, tokenKey, creds, folder, now)
	if token != "" {
		res.Hidden = append(res.Hidden, token)
	}
	if err != nil {
		if haveCache && Temporary(err) {
			res.Secrets = cached
			res.Warnings = append(res.Warnings, fmt.Sprintf("Infisical did not answer (%v). FAM uses the secrets that it read %s ago", err, roundAge(age)))
			return res, nil
		}
		return res, err
	}
	res.Secrets = list
	if err := cache.WriteSecrets(key, list, now); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("FAM cannot save the Infisical cache in %s: %v. The next run reads Infisical again", o.CacheDir, err))
	}
	return res, nil
}

// fetch signs in when necessary and reads the secrets. It reuses a cached access token, and signs
// in again once when Infisical rejects it. It returns the access token that it used.
func fetch(ctx context.Context, c *Client, cache Cache, tokenKey string, creds Credentials, f Folder, now time.Time) ([]secrets.Secret, string, error) {
	if creds.Token != "" {
		list, err := c.Secrets(ctx, creds.Token, f)
		return list, "", err
	}
	tok, ok := cache.ReadToken(tokenKey)
	if ok && now.Add(time.Minute).Before(tok.ExpiresAt) {
		list, err := c.Secrets(ctx, tok.AccessToken, f)
		if !unauthorized(err) {
			return list, tok.AccessToken, err
		}
		cache.DropToken(tokenKey, tok.AccessToken)
	}
	tok, err := c.Login(ctx, creds.ClientID, creds.ClientSecret)
	if err != nil {
		return nil, "", err
	}
	// A token that FAM cannot save still works for this run.
	_ = cache.WriteToken(tokenKey, tok)
	list, err := c.Secrets(ctx, tok.AccessToken, f)
	// Infisical rejected a new token: a second login gives the same result, so stop, and do not
	// keep the token.
	if unauthorized(err) {
		cache.DropToken(tokenKey, tok.AccessToken)
	}
	return list, tok.AccessToken, err
}

func roundAge(d time.Duration) time.Duration {
	switch {
	case d < 0:
		return 0
	case d < time.Minute:
		return d.Round(time.Second)
	default:
		return d.Round(time.Minute)
	}
}
