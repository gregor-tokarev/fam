// Package infisical reads the secrets of a FAM project from Infisical, and keeps the answers in a
// cache on the cockpit.
package infisical

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gregor-tokarev/fam/internal/secrets"
	"github.com/gregor-tokarev/fam/internal/version"
)

// Client calls the HTTP API of one Infisical site.
type Client struct {
	// Site is the address of the site without `/api`, for example https://app.infisical.com.
	Site string
	HTTP *http.Client
}

// NewClient returns a client with a timeout for each request. It does not follow redirects:
// a 307 or 308 redirect would send the login body, with the client secret, to another address.
func NewClient(site string) *Client {
	return &Client{Site: strings.TrimRight(site, "/"), HTTP: &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// CheckSite stops on a site that is not an HTTPS URL. Plain HTTP is allowed only for a loopback
// host (localhost, 127.0.0.0/8, ::1), for example a local mock. The messages do not repeat the
// URL, because a wrong URL can contain a password.
func CheckSite(site string) error {
	u, err := url.Parse(site)
	if err != nil || u.Host == "" {
		return errors.New("Infisical siteUrl is not a URL. Use for example https://app.infisical.com")
	}
	if u.User != nil {
		return errors.New("Infisical siteUrl contains a user name or a password. Remove them: FAM signs in with the credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("Infisical siteUrl has a query or a fragment. Use only the address of the site")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if ip := net.ParseIP(host); host == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
		return fmt.Errorf("Infisical siteUrl http://%s uses plain HTTP, which sends the credentials without encryption. Use https://, or http:// only for localhost", u.Host)
	}
	return errors.New("Infisical siteUrl must start with https://")
}

// statusMessage is a fixed message for an error status. FAM never shows the text of the answer:
// a proxy or a server can put anything into it, also a secret value.
func statusMessage(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "Infisical rejected the request. Check projectId, environment, and path"
	case http.StatusUnauthorized:
		return "Infisical rejected the credentials or the access token"
	case http.StatusForbidden:
		return "the identity has no access to this project, environment, or folder"
	case http.StatusNotFound:
		return "Infisical has no such project, environment, or folder"
	case http.StatusTooManyRequests:
		return "Infisical limits the number of requests. Try again later, or make cache longer"
	}
	if status/100 == 3 {
		return "Infisical answered with a redirect, and FAM does not follow redirects. Set siteUrl to the final address"
	}
	return http.StatusText(status)
}

// Token is an access token of a machine identity.
type Token struct {
	AccessToken string    `json:"accessToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// Folder selects the secrets that FAM reads.
type Folder struct {
	ProjectID      string
	Environment    string
	Path           string
	Recursive      bool
	IncludeImports bool
}

// APIError is an answer of Infisical with an error status.
type APIError struct {
	Op      string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("Infisical %s: HTTP %d: %s", e.Op, e.Status, msg)
}

// Temporary reports whether err is a failure of the network or of the Infisical service, not a
// problem of the request: a connection error, a timeout, HTTP 429, or HTTP 5xx.
func Temporary(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusTooManyRequests || ae.Status >= 500
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func unauthorized(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden)
}

// Login signs in a machine identity with Universal Auth.
func (c *Client) Login(ctx context.Context, clientID, clientSecret string) (Token, error) {
	body, _ := json.Marshal(map[string]string{"clientId": clientID, "clientSecret": clientSecret})
	var out struct {
		AccessToken string  `json:"accessToken"`
		ExpiresIn   float64 `json:"expiresIn"`
	}
	start := time.Now()
	if err := c.do(ctx, "login", http.MethodPost, "/api/v1/auth/universal-auth/login", "", bytes.NewReader(body), &out); err != nil {
		return Token{}, err
	}
	if out.AccessToken == "" {
		return Token{}, errors.New("Infisical login: the answer has no access token")
	}
	return Token{AccessToken: out.AccessToken, ExpiresAt: start.Add(time.Duration(out.ExpiresIn * float64(time.Second)))}, nil
}

type rawSecret struct {
	Workspace   string `json:"workspace"`
	Environment string `json:"environment"`
	SecretKey   string `json:"secretKey"`
	// SecretValue is a pointer: a missing value must not become an empty value.
	SecretValue *string `json:"secretValue"`
	SecretPath  string  `json:"secretPath"`
	// SecretValueHidden is true when the identity can see the name but not the value. Then
	// secretValue is a placeholder.
	SecretValueHidden bool `json:"secretValueHidden"`
}

// Secrets reads the secrets of a folder. A secret of the folder wins over an imported secret, and a
// later import wins over an earlier one, as in the Infisical CLI. The result is sorted by name.
func (c *Client) Secrets(ctx context.Context, accessToken string, f Folder) ([]secrets.Secret, error) {
	q := url.Values{}
	q.Set("workspaceId", f.ProjectID)
	q.Set("environment", f.Environment)
	q.Set("secretPath", f.Path)
	q.Set("recursive", fmt.Sprint(f.Recursive))
	q.Set("include_imports", fmt.Sprint(f.IncludeImports))
	q.Set("expandSecretReferences", "true")
	var out struct {
		Secrets []rawSecret `json:"secrets"`
		Imports []struct {
			SecretPath  string      `json:"secretPath"`
			Environment string      `json:"environment"`
			Secrets     []rawSecret `json:"secrets"`
		} `json:"imports"`
	}
	if err := c.do(ctx, "read secrets", http.MethodGet, "/api/v3/secrets/raw?"+q.Encode(), accessToken, nil, &out); err != nil {
		return nil, err
	}
	// An answer without the list (for example `{}` from a proxy) must not become an empty cache.
	if out.Secrets == nil {
		return nil, errors.New("Infisical read secrets: the answer has no secrets list")
	}
	values := map[string]*string{}
	folderOf := map[string]string{}
	hidden := map[string]bool{}
	for _, s := range out.Secrets {
		p := s.SecretPath
		if p == "" {
			p = f.Path
		}
		// A service token with one scope makes Infisical answer for that scope, not for the
		// request. Accept only the secrets of the requested project, environment, and folder.
		if (s.Workspace != "" && s.Workspace != f.ProjectID) || (s.Environment != "" && s.Environment != f.Environment) || !inFolder(p, f.Path, f.Recursive) {
			return nil, fmt.Errorf("Infisical sent secrets of another project, environment, or folder than %s:%s. The token is probably scoped to another place: use a token or an identity with access to this one", f.Environment, f.Path)
		}
		if other, ok := folderOf[s.SecretKey]; ok && other != p {
			return nil, fmt.Errorf("Infisical: the secret %s is in the folders %s and %s. FAM needs each name once. Rename one, or read only one folder", s.SecretKey, other, p)
		}
		folderOf[s.SecretKey] = p
		values[s.SecretKey] = s.SecretValue
		hidden[s.SecretKey] = s.SecretValueHidden
	}
	for i := len(out.Imports) - 1; i >= 0; i-- {
		for _, s := range out.Imports[i].Secrets {
			if _, ok := values[s.SecretKey]; !ok {
				values[s.SecretKey] = s.SecretValue
				hidden[s.SecretKey] = s.SecretValueHidden
				folderOf[s.SecretKey] = out.Imports[i].Environment + ":" + out.Imports[i].SecretPath
			}
		}
	}
	list := make([]secrets.Secret, 0, len(values))
	for name, ptr := range values {
		if hidden[name] {
			return nil, fmt.Errorf("Infisical: the identity of FAM can see the secret %s (%s) but cannot read its value. Give it read access to the values, or keep the secret in a folder that FAM does not read", name, folderOf[name])
		}
		if ptr == nil {
			return nil, fmt.Errorf("Infisical: the answer has no value for the secret %s (%s)", name, folderOf[name])
		}
		value := *ptr
		if !secrets.ValidName(name) {
			return nil, fmt.Errorf("Infisical: the secret %q (%s) is not a valid shell variable name. Rename it, or keep it in a folder that FAM does not read", name, folderOf[name])
		}
		if !secrets.SafeValue(value) {
			return nil, fmt.Errorf("Infisical: the value of %s (%s) contains a control character or a new line. FAM cannot write it safely. Keep it in a folder that FAM does not read", name, folderOf[name])
		}
		list = append(list, secrets.Secret{Name: name, Value: value})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

// inFolder reports whether the folder p is the folder want, or with recursive a subfolder of it.
func inFolder(p, want string, recursive bool) bool {
	p, want = "/"+strings.Trim(p, "/"), "/"+strings.Trim(want, "/")
	if p == want {
		return true
	}
	return recursive && (want == "/" || strings.HasPrefix(p, want+"/"))
}

func (c *Client) do(ctx context.Context, op, method, path, token string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.Site+path, body)
	if err != nil {
		return fmt.Errorf("Infisical %s: %w", op, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "fam/"+version.Version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The URL of a url.Error is long and says nothing new: name only the site.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("Infisical %s: %s: %w", op, c.Site, ue.Err)
		}
		return fmt.Errorf("Infisical %s: %w", op, err)
	}
	defer resp.Body.Close()
	// The status decides an error, before FAM reads the body: a body that stops must not turn a
	// rejected request into a network failure.
	if resp.StatusCode/100 != 2 {
		return &APIError{Op: op, Status: resp.StatusCode, Message: statusMessage(resp.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("Infisical %s: %w", op, err)
	}
	// The error of the JSON decoder can contain a part of the answer (for example a number), so
	// FAM shows a fixed message.
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("Infisical %s: the answer is not valid JSON in the form that FAM expects", op)
	}
	return nil
}
