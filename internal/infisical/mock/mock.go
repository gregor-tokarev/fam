// Package mock is a small in-memory Infisical server: Universal Auth login, access tokens, and the
// raw secrets API (v3) with folders, imports, and recursive reads. It is for development and demos
// of FAM without an Infisical account. It counts requests, so that the FAM cache can be observed.
package mock

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Data is the content of the mock: who can sign in, and the secrets.
type Data struct {
	// Identities are machine identities with Universal Auth.
	Identities []Identity `json:"identities"`
	// Tokens are static access tokens (like service tokens) that never expire.
	Tokens []string `json:"tokens"`
	// Projects maps project ID → environment slug → folder path → folder.
	Projects map[string]map[string]map[string]*Folder `json:"projects"`
}

// Identity is a machine identity with Universal Auth.
type Identity struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

// Folder is one folder of an environment.
type Folder struct {
	// Secrets maps a secret name to its value.
	Secrets map[string]string `json:"secrets"`
	// Imports are other folders whose secrets this folder imports.
	Imports []Import `json:"imports,omitempty"`
}

// Import names an imported folder.
type Import struct {
	Environment string `json:"environment"`
	Path        string `json:"path"`
}

// Demo is the data of the mock when no data file is given.
func Demo() Data {
	return Data{
		Identities: []Identity{{ClientID: "fam-demo-client", ClientSecret: "fam-demo-secret"}},
		Tokens:     []string{"st.fam-demo-token"},
		Projects: map[string]map[string]map[string]*Folder{
			"fam-demo": {
				"prod": {
					"/": {Secrets: map[string]string{"GH_TOKEN": "ghp_demo_value", "OPENAI_API_KEY": "sk-demo-openai"},
						Imports: []Import{{Environment: "shared", Path: "/"}}},
					"/agents": {Secrets: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-demo"}},
				},
				"shared": {
					"/": {Secrets: map[string]string{"CLOUDFLARE_ACCOUNT_ID": "demo-account-id"}},
				},
			},
		},
	}
}

// Stats counts the requests that the mock answered.
type Stats struct {
	Logins       int `json:"logins"`
	SecretReads  int `json:"secretReads"`
	SecretWrites int `json:"secretWrites"`
	Unauthorized int `json:"unauthorized"`
	RateLimited  int `json:"rateLimited"`
}

type secret struct {
	id        string
	value     string
	comment   string
	version   int
	createdAt time.Time
	updatedAt time.Time
}

type folder struct {
	id      string
	secrets map[string]*secret
	imports []Import
}

// Options change how the mock answers.
type Options struct {
	// TokenTTL is the lifetime of an access token from a login. Default 2h.
	TokenTTL time.Duration
	// RateLimit is the number of secret reads per minute, like the limits of Infisical Cloud.
	// 0 means no limit.
	RateLimit int
	// Latency delays each answer.
	Latency time.Duration
	// Log receives one line for each request, without secret values. It can be nil.
	Log io.Writer
	Now func() time.Time
}

// Server is the mock. Use Handler to serve it.
type Server struct {
	opts       Options
	mu         sync.Mutex
	identities map[string]string
	static     map[string]bool
	tokens     map[string]time.Time
	projects   map[string]map[string]map[string]*folder
	stats      Stats
	reads      []time.Time
}

// New returns a mock with the data.
func New(d Data, o Options) *Server {
	if o.TokenTTL <= 0 {
		o.TokenTTL = 2 * time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Server{opts: o, identities: map[string]string{}, static: map[string]bool{}, tokens: map[string]time.Time{},
		projects: map[string]map[string]map[string]*folder{}}
	for _, i := range d.Identities {
		s.identities[i.ClientID] = i.ClientSecret
	}
	for _, t := range d.Tokens {
		s.static[t] = true
	}
	now := o.Now()
	for pid, envs := range d.Projects {
		s.projects[pid] = map[string]map[string]*folder{}
		for env, folders := range envs {
			s.projects[pid][env] = map[string]*folder{"/": {id: newID(), secrets: map[string]*secret{}}}
			for p, f := range folders {
				nf := &folder{id: newID(), secrets: map[string]*secret{}, imports: f.Imports}
				for name, v := range f.Secrets {
					nf.secrets[name] = &secret{id: newID(), value: v, version: 1, createdAt: now, updatedAt: now}
				}
				s.projects[pid][env][cleanPath(p)] = nf
			}
		}
	}
	return s
}

// Stats returns the request counts.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Handler serves the Infisical API and the mock API under /mock.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"date": s.opts.Now().UTC(), "message": "Ok", "emailConfigured": false})
	})
	mux.HandleFunc("POST /api/v1/auth/universal-auth/login", s.login)
	mux.HandleFunc("GET /api/v3/secrets/raw", s.auth(s.limit(s.list)))
	mux.HandleFunc("GET /api/v3/secrets/raw/{name}", s.auth(s.limit(s.get)))
	mux.HandleFunc("POST /api/v3/secrets/raw/{name}", s.auth(s.create))
	mux.HandleFunc("PATCH /api/v3/secrets/raw/{name}", s.auth(s.update))
	mux.HandleFunc("DELETE /api/v3/secrets/raw/{name}", s.auth(s.remove))
	mux.HandleFunc("GET /mock/stats", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, s.Stats()) })
	mux.HandleFunc("DELETE /mock/stats", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.stats = Stats{}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /mock/revoke-tokens", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.tokens = map[string]time.Time{}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apiError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("Route %s:%s not found", r.Method, r.URL.Path))
	})
	return s.logged(mux)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		_ = r.ParseForm()
		in.ClientID, in.ClientSecret = r.PostForm.Get("clientId"), r.PostForm.Get("clientSecret")
	} else if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, "BadRequest", "Invalid request body")
		return
	}
	s.mu.Lock()
	want, ok := s.identities[in.ClientID]
	if !ok || want != in.ClientSecret || in.ClientSecret == "" {
		s.stats.Unauthorized++
		s.mu.Unlock()
		apiError(w, http.StatusUnauthorized, "UnauthorizedError", "Invalid credentials")
		return
	}
	s.stats.Logins++
	tok := "mock." + newID() + newID()
	s.tokens[tok] = s.opts.Now().Add(s.opts.TokenTTL)
	s.mu.Unlock()
	ttl := int(s.opts.TokenTTL / time.Second)
	writeJSON(w, http.StatusOK, map[string]any{"accessToken": tok, "expiresIn": ttl, "accessTokenMaxTTL": ttl, "tokenType": "Bearer"})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		exp, issued := s.tokens[tok]
		valid := ok && (s.static[tok] || (issued && s.opts.Now().Before(exp)))
		if !valid {
			s.stats.Unauthorized++
		}
		s.mu.Unlock()
		if !valid {
			msg := "Token missing"
			if ok {
				msg = "Invalid or expired access token"
			}
			apiError(w, http.StatusUnauthorized, "UnauthorizedError", msg)
			return
		}
		next(w, r)
	}
}

func (s *Server) limit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.RateLimit > 0 {
			s.mu.Lock()
			now := s.opts.Now()
			kept := s.reads[:0]
			for _, t := range s.reads {
				if now.Sub(t) < time.Minute {
					kept = append(kept, t)
				}
			}
			s.reads = kept
			if len(s.reads) >= s.opts.RateLimit {
				s.stats.RateLimited++
				retry := time.Minute - now.Sub(s.reads[0])
				s.mu.Unlock()
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
				apiError(w, http.StatusTooManyRequests, "TooManyRequests", "Rate limit exceeded. Please try again in a minute")
				return
			}
			s.reads = append(s.reads, now)
			s.mu.Unlock()
		}
		next(w, r)
	}
}

// scope reads the project, environment, and folder of a request.
type scope struct {
	project, env, path string
}

func (s *Server) environment(w http.ResponseWriter, sc scope) (map[string]*folder, bool) {
	if sc.project == "" || sc.env == "" {
		apiError(w, http.StatusBadRequest, "BadRequest", "workspaceId and environment are required")
		return nil, false
	}
	envs, ok := s.projects[sc.project]
	if !ok {
		apiError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("Project with ID '%s' not found", sc.project))
		return nil, false
	}
	folders, ok := envs[sc.env]
	if !ok {
		apiError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("Environment with slug '%s' in project with ID %s not found", sc.env, sc.project))
		return nil, false
	}
	return folders, true
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sc := scope{project: q.Get("workspaceId"), env: q.Get("environment"), path: cleanPath(q.Get("secretPath"))}
	recursive := q.Get("recursive") == "true"
	imports := q.Get("include_imports") == "true"
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.SecretReads++
	folders, ok := s.environment(w, sc)
	if !ok {
		return
	}
	if _, ok := folders[sc.path]; !ok {
		apiError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("Folder with path '%s' in environment with slug '%s' not found", sc.path, sc.env))
		return
	}
	var paths []string
	for p := range folders {
		if p == sc.path || (recursive && (sc.path == "/" || strings.HasPrefix(p, sc.path+"/"))) {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	out := []map[string]any{}
	for _, p := range paths {
		out = append(out, s.secretList(sc.project, sc.env, p, folders[p])...)
	}
	res := map[string]any{"secrets": out}
	if imports {
		// Like Infisical, a recursive read also gives the imports of each subfolder.
		imp := []map[string]any{}
		for _, p := range paths {
			for _, i := range folders[p].imports {
				ip := cleanPath(i.Path)
				src, ok := s.projects[sc.project][i.Environment][ip]
				if !ok {
					continue
				}
				imp = append(imp, map[string]any{"secretPath": ip, "environment": i.Environment, "folderId": src.id,
					"secrets": s.secretList(sc.project, i.Environment, ip, src)})
			}
		}
		res["imports"] = imp
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) secretList(project, env, p string, f *folder) []map[string]any {
	names := make([]string, 0, len(f.secrets))
	for n := range f.secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, secretJSON(project, env, p, n, f.secrets[n]))
	}
	return out
}

func secretJSON(project, env, p, name string, sec *secret) map[string]any {
	return map[string]any{
		"id": sec.id, "_id": sec.id, "workspace": project, "environment": env, "version": sec.version, "type": "shared",
		"secretKey": name, "secretValue": sec.value, "secretComment": sec.comment, "secretPath": p,
		"secretReminderNote": nil, "secretReminderRepeatDays": nil, "skipMultilineEncoding": false, "tags": []any{},
		"createdAt": sec.createdAt.UTC(), "updatedAt": sec.updatedAt.UTC(),
	}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sc := scope{project: q.Get("workspaceId"), env: q.Get("environment"), path: cleanPath(q.Get("secretPath"))}
	name := r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.SecretReads++
	folders, ok := s.environment(w, sc)
	if !ok {
		return
	}
	f, ok := folders[sc.path]
	if !ok || f.secrets[name] == nil {
		apiError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("Secret with name '%s' not found", name))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secret": secretJSON(sc.project, sc.env, sc.path, name, f.secrets[name])})
}

type writeBody struct {
	WorkspaceID   string  `json:"workspaceId"`
	Environment   string  `json:"environment"`
	SecretPath    string  `json:"secretPath"`
	SecretValue   *string `json:"secretValue"`
	SecretComment *string `json:"secretComment"`
}

func (s *Server) writeScope(w http.ResponseWriter, r *http.Request) (writeBody, scope, map[string]*folder, bool) {
	var b writeBody
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			apiError(w, http.StatusBadRequest, "BadRequest", "Invalid request body")
			return b, scope{}, nil, false
		}
	}
	sc := scope{project: b.WorkspaceID, env: b.Environment, path: cleanPath(b.SecretPath)}
	s.stats.SecretWrites++
	folders, ok := s.environment(w, sc)
	return b, sc, folders, ok
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, sc, folders, ok := s.writeScope(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	f := folders[sc.path]
	if f == nil {
		f = &folder{id: newID(), secrets: map[string]*secret{}}
		folders[sc.path] = f
	}
	if f.secrets[name] != nil {
		apiError(w, http.StatusBadRequest, "BadRequest", "Secret already exist")
		return
	}
	now := s.opts.Now()
	sec := &secret{id: newID(), version: 1, createdAt: now, updatedAt: now}
	if b.SecretValue != nil {
		sec.value = *b.SecretValue
	}
	if b.SecretComment != nil {
		sec.comment = *b.SecretComment
	}
	f.secrets[name] = sec
	writeJSON(w, http.StatusOK, map[string]any{"secret": secretJSON(sc.project, sc.env, sc.path, name, sec)})
}

func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, sc, folders, ok := s.writeScope(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	f := folders[sc.path]
	if f == nil || f.secrets[name] == nil {
		apiError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("Secret with name '%s' not found", name))
		return
	}
	sec := f.secrets[name]
	if b.SecretValue != nil {
		sec.value = *b.SecretValue
	}
	if b.SecretComment != nil {
		sec.comment = *b.SecretComment
	}
	sec.version++
	sec.updatedAt = s.opts.Now()
	writeJSON(w, http.StatusOK, map[string]any{"secret": secretJSON(sc.project, sc.env, sc.path, name, sec)})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, sc, folders, ok := s.writeScope(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	f := folders[sc.path]
	if f == nil || f.secrets[name] == nil {
		apiError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("Secret with name '%s' not found", name))
		return
	}
	sec := f.secrets[name]
	delete(f.secrets, name)
	writeJSON(w, http.StatusOK, map[string]any{"secret": secretJSON(sc.project, sc.env, sc.path, name, sec)})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Latency > 0 {
			select {
			case <-time.After(s.opts.Latency):
			case <-r.Context().Done():
				return
			}
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		if s.opts.Log != nil {
			// Only the path: a caller can put a token into the query.
			fmt.Fprintf(s.opts.Log, "%s %s %s %d %s\n", time.Now().Format(time.TimeOnly), r.Method, r.URL.EscapedPath(), sw.status, time.Since(start).Round(time.Microsecond))
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, kind, msg string) {
	writeJSON(w, status, map[string]any{"reqId": "req-" + newID(), "statusCode": status, "message": msg, "error": kind})
}

func cleanPath(p string) string {
	p = "/" + strings.Trim(p, "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
