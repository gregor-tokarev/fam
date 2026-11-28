package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/apple/pkl-go/pkl"

	"github.com/gregor-tokarev/fam/internal/pklgen/base"
	pklconfig "github.com/gregor-tokarev/fam/internal/pklgen/config"
	"github.com/gregor-tokarev/fam/internal/version"
)

const (
	planCacheDirName         = "plan-cache-v3"
	planCacheMaxFiles        = 128
	planCacheMaxFileSz       = 8 << 20
	planCacheMaxProjectFiles = 2000
)

type cachedInitDisk struct {
	Machines   []Machine `json:"machines"`
	SecretsRaw []byte    `json:"secretsRaw"`
}

// PlanCache caches raw Pkl binary evaluation results on disk keyed by a full SHA-256 fingerprint
// of the project directory, schema checksum, binary build, and evaluation properties. Because it
// stores Pkl's own binary encoding and re-runs pkl.Unmarshal + Resolve on every hit, every schema
// field, nullable empty string, and Go resolver change stays 100% lossless and current.
type PlanCache struct {
	dir        string
	projectKey string
	disabled   bool
}

func planCacheDir() string {
	if x := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "fam", planCacheDirName)
	}
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "fam", planCacheDirName)
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".cache", "fam", planCacheDirName)
	}
	return ""
}

// NewPlanCache fingerprints baseDir and configPath and returns a disk-backed PlanCache.
// If any Pkl file references external URLs, paths outside baseDir, globs, dynamic imports/reads,
// or symlinks escaping baseDir, caching is automatically disabled to guarantee zero stale hits.
func NewPlanCache(baseDir, configPath, pkgChecksum string) *PlanCache {
	c := &PlanCache{dir: planCacheDir()}
	if c.dir == "" || os.Getenv("FAM_NO_PLAN_CACHE") == "1" {
		c.disabled = true
		return c
	}
	key, ok := fingerprintProject(baseDir, configPath, pkgChecksum)
	if !ok {
		c.disabled = true
		return c
	}
	c.projectKey = key
	return c
}

func exeFingerprint() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if ei, err := os.Stat(exe); err == nil {
		return fmt.Sprintf("%s:%d:%d", exe, ei.Size(), ei.ModTime().UnixNano())
	}
	return exe
}

func fingerprintProject(baseDir, configPath, pkgChecksum string) (string, bool) {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", false
	}
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return "", false
	}
	absConfig = filepath.Clean(absConfig)
	realBase, err := filepath.EvalSymlinks(absBase)
	if err != nil {
		realBase = absBase
	}
	type fileEntry struct {
		rel  string
		path string
	}
	var entries []fileEntry
	walkErr := filepath.WalkDir(absBase, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != absBase && name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(p)
			if err != nil || !pathWithin(realBase, target) {
				return fs.ErrInvalid
			}
			info, err := os.Stat(target)
			if err != nil || info.IsDir() {
				return fs.ErrInvalid
			}
		} else if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(absBase, p)
		if err != nil {
			return err
		}
		entries = append(entries, fileEntry{rel: filepath.ToSlash(rel), path: p})
		if len(entries) > planCacheMaxProjectFiles {
			return fs.ErrInvalid
		}
		return nil
	})
	if walkErr != nil {
		return "", false
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	hashedFiles := make(map[string]bool, len(entries))
	for _, e := range entries {
		hashedFiles[filepath.Clean(e.path)] = true
	}
	if !hashedFiles[absConfig] {
		return "", false
	}

	h := sha256.New()
	fmt.Fprintf(h, "v3\x00%s\x00%s\x00%s\x00%s\x00%s\x00", version.Version, exeFingerprint(), pkgChecksum, absBase, absConfig)
	readsEnv := false
	for _, e := range entries {
		info, err := os.Stat(e.path)
		if err != nil || info.Size() > planCacheMaxFileSz {
			return "", false
		}
		b, err := os.ReadFile(e.path)
		if err != nil {
			return "", false
		}
		if isScannedPklModule(e.rel) || filepath.Clean(e.path) == absConfig {
			envUsed, ok := scanPklInputs(b, e.path, absBase, realBase, hashedFiles)
			if !ok {
				return "", false
			}
			if envUsed {
				readsEnv = true
			}
		} else if filepath.Base(e.rel) == "PklProject.deps.json" {
			if !depsWithinProject(b) {
				return "", false
			}
		}
		fsum := sha256.Sum256(b)
		h.Write([]byte(e.rel))
		h.Write([]byte{0})
		h.Write(fsum[:])
		h.Write([]byte{0})
	}
	if readsEnv {
		env := append([]string{}, os.Environ()...)
		sort.Strings(env)
		h.Write([]byte("env\x00"))
		for _, kv := range env {
			h.Write([]byte(kv))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

func isScannedPklModule(rel string) bool {
	return strings.HasSuffix(rel, ".pkl") || filepath.Base(rel) == "PklProject"
}

func pathWithin(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func depsWithinProject(b []byte) bool {
	var deps struct {
		ResolvedDependencies map[string]struct {
			Type string `json:"type"`
			Path string `json:"path"`
		} `json:"resolvedDependencies"`
	}
	if err := json.Unmarshal(b, &deps); err != nil {
		return false
	}
	// Disable plan caching whenever the project depends on any local Pkl package, because
	// `@local/...` imports can load arbitrary extensionless modules and external files inside
	// that package that are not covered by the single-project module scan.
	for _, dep := range deps.ResolvedDependencies {
		if dep.Type != "remote" {
			return false
		}
	}
	return true
}

// scanPklInputs walks Pkl source tokens (skipping comments and plain string literals while still
// scanning inside string interpolations) and verifies every amends/extends/import/read target is
// strictly covered by the project fingerprint. Returns (readsEnv, cacheable).
func scanPklInputs(src []byte, filePath, absBase, realBase string, hashedFiles map[string]bool) (bool, bool) {
	s := pklScanner{src: src, dir: filepath.Dir(filePath), absBase: absBase, realBase: realBase, hashedFiles: hashedFiles}
	if !s.scanCode(0) {
		return false, false
	}
	return s.readsEnv, true
}

type pklScanner struct {
	src         []byte
	i           int
	dir         string
	absBase     string
	realBase    string
	hashedFiles map[string]bool
	readsEnv    bool
}

func isIdentStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == '$'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func (s *pklScanner) skipSpaceAndComments() {
	for s.i < len(s.src) {
		c := s.src[s.i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			s.i++
			continue
		}
		if c == '/' && s.i+1 < len(s.src) {
			if s.src[s.i+1] == '/' {
				s.i += 2
				for s.i < len(s.src) && s.src[s.i] != '\n' {
					s.i++
				}
				continue
			}
			if s.src[s.i+1] == '*' {
				s.i += 2
				depth := 1
				for s.i < len(s.src) && depth > 0 {
					if s.src[s.i] == '/' && s.i+1 < len(s.src) && s.src[s.i+1] == '*' {
						depth++
						s.i += 2
					} else if s.src[s.i] == '*' && s.i+1 < len(s.src) && s.src[s.i+1] == '/' {
						depth--
						s.i += 2
					} else {
						s.i++
					}
				}
				continue
			}
		}
		break
	}
}

// scanCode scans Pkl code until EOF (stopParen == 0) or until the matching ')' closes an
// interpolation block (stopParen > 0).
func (s *pklScanner) scanCode(stopParen int) bool {
	depth := stopParen
	var prev byte
	for s.i < len(s.src) {
		s.skipSpaceAndComments()
		if s.i >= len(s.src) {
			break
		}
		c := s.src[s.i]
		if stopParen > 0 {
			if c == '(' {
				depth++
				s.i++
				prev = c
				continue
			}
			if c == ')' {
				depth--
				s.i++
				if depth == 0 {
					return true
				}
				prev = c
				continue
			}
		}
		if c == '"' || (c == '#' && s.isCustomString()) {
			if !s.skipString() {
				return false
			}
			prev = '"'
			continue
		}
		if isIdentStart(c) {
			start := s.i
			s.i++
			for s.i < len(s.src) && isIdentPart(s.src[s.i]) {
				s.i++
			}
			word := string(s.src[start:s.i])
			if prev != '.' && prev != '`' {
				switch word {
				case "amends", "extends":
					if !s.checkModuleRef(false) {
						return false
					}
					prev = ')'
					continue
				case "import":
					if s.i < len(s.src) && s.src[s.i] == '*' {
						return false
					}
					if !s.checkModuleRef(true) {
						return false
					}
					prev = ')'
					continue
				case "read":
					if s.i < len(s.src) && s.src[s.i] == '*' {
						return false
					}
					save := s.i
					if s.i < len(s.src) && s.src[s.i] == '?' {
						s.i++
					}
					s.skipSpaceAndComments()
					if s.i < len(s.src) && s.src[s.i] == '(' {
						if !s.checkReadCall() {
							return false
						}
						prev = ')'
						continue
					}
					s.i = save
				}
			}
			prev = 'a'
			continue
		}
		prev = c
		s.i++
	}
	return depth == 0
}

func (s *pklScanner) isCustomString() bool {
	j := s.i
	for j < len(s.src) && s.src[j] == '#' {
		j++
	}
	return j > s.i && j < len(s.src) && s.src[j] == '"'
}

func (s *pklScanner) skipString() bool {
	hashes := 0
	for s.i < len(s.src) && s.src[s.i] == '#' {
		hashes++
		s.i++
	}
	multiline := s.i+2 < len(s.src) && s.src[s.i] == '"' && s.src[s.i+1] == '"' && s.src[s.i+2] == '"'
	if multiline {
		s.i += 3
	} else {
		s.i++
	}
	for s.i < len(s.src) {
		if s.src[s.i] == '\\' {
			// Escape sequence: `\` followed by `hashes` '#' chars.
			if s.i+1+hashes < len(s.src) && allHashes(s.src[s.i+1:s.i+1+hashes]) {
				next := s.src[s.i+1+hashes]
				s.i += 2 + hashes
				if next == '(' {
					if !s.scanCode(1) {
						return false
					}
				}
				continue
			}
			s.i++
			continue
		}
		if multiline {
			if s.i+3+hashes <= len(s.src) && s.src[s.i] == '"' && s.src[s.i+1] == '"' && s.src[s.i+2] == '"' && allHashes(s.src[s.i+3:s.i+3+hashes]) {
				s.i += 3 + hashes
				return true
			}
		} else {
			if s.i+1+hashes <= len(s.src) && s.src[s.i] == '"' && allHashes(s.src[s.i+1:s.i+1+hashes]) {
				s.i += 1 + hashes
				return true
			}
		}
		s.i++
	}
	return false
}

func allHashes(b []byte) bool {
	for _, c := range b {
		if c != '#' {
			return false
		}
	}
	return true
}

// readSimpleString parses a plain double-quoted Pkl string literal with no escapes or interpolation.
func (s *pklScanner) readSimpleString() (string, bool) {
	if s.i >= len(s.src) || s.src[s.i] != '"' {
		return "", false
	}
	if s.i+2 < len(s.src) && s.src[s.i+1] == '"' && s.src[s.i+2] == '"' {
		return "", false
	}
	s.i++
	var out strings.Builder
	for s.i < len(s.src) {
		c := s.src[s.i]
		if c == '"' {
			s.i++
			return out.String(), true
		}
		if c == '\\' || c == '\n' || c == '\r' {
			// Reject all escape sequences (e.g. \u{2e}, \() so encoded paths cannot bypass checks.
			return "", false
		}
		out.WriteByte(c)
		s.i++
	}
	return "", false
}

func (s *pklScanner) checkModuleRef(allowParens bool) bool {
	s.skipSpaceAndComments()
	parens := false
	if allowParens && s.i < len(s.src) && s.src[s.i] == '(' {
		parens = true
		s.i++
		s.skipSpaceAndComments()
	}
	target, ok := s.readSimpleString()
	if !ok {
		return false
	}
	if parens {
		s.skipSpaceAndComments()
		if s.i >= len(s.src) || s.src[s.i] != ')' {
			return false
		}
		s.i++
	}
	if strings.HasPrefix(target, "@") || strings.HasPrefix(target, "pkl:") {
		return true
	}
	rel, ok := s.resolveSafeLocalRel(target)
	if !ok {
		return false
	}
	// Use the exact same module predicate as fingerprintProject so every imported local module
	// is guaranteed to have been scanned for transitive imports and reads.
	return isScannedPklModule(rel)
}

func (s *pklScanner) checkReadCall() bool {
	// Caller positioned s.i at '('.
	s.i++
	s.skipSpaceAndComments()
	target, ok := s.readSimpleString()
	if !ok {
		return false
	}
	s.skipSpaceAndComments()
	if s.i >= len(s.src) || s.src[s.i] != ')' {
		return false
	}
	s.i++
	switch {
	case strings.HasPrefix(target, "prop:"):
		return true
	case strings.HasPrefix(target, "env:"):
		s.readsEnv = true
		return true
	default:
		return s.isSafeLocalRel(target)
	}
}

func (s *pklScanner) isSafeLocalRel(target string) bool {
	_, ok := s.resolveSafeLocalRel(target)
	return ok
}

func (s *pklScanner) resolveSafeLocalRel(target string) (string, bool) {
	if target == "" || target == "..." || strings.HasPrefix(target, ".../") || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "\\") || strings.Contains(target, ":") {
		return "", false
	}
	resolved := filepath.Clean(filepath.Join(s.dir, filepath.FromSlash(target)))
	if !pathWithin(s.absBase, resolved) {
		return "", false
	}
	rel, err := filepath.Rel(s.absBase, resolved)
	if err != nil {
		return "", false
	}
	relSlash := filepath.ToSlash(rel)
	for _, seg := range strings.Split(relSlash, "/") {
		if seg == ".git" {
			return "", false
		}
	}
	if _, err := os.Lstat(resolved); err == nil {
		if !s.hashedFiles[resolved] {
			return "", false
		}
	}
	if realTarget, err := filepath.EvalSymlinks(resolved); err == nil && !pathWithin(s.realBase, realTarget) {
		return "", false
	}
	return relSlash, true
}

func (c *PlanCache) entryPath(kind string, props map[string]string, ext string) string {
	h := sha256.New()
	h.Write([]byte(c.projectKey))
	h.Write([]byte{0})
	h.Write([]byte(kind))
	h.Write([]byte{0})
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{'='})
		h.Write([]byte(props[k]))
		h.Write([]byte{0})
	}
	return filepath.Join(c.dir, hex.EncodeToString(h.Sum(nil))+ext)
}

func (c *PlanCache) GetInit(props map[string]string) ([]Machine, *base.Secrets, bool) {
	if c == nil || c.disabled {
		return nil, nil, false
	}
	b, err := os.ReadFile(c.entryPath("init", props, ".json"))
	if err != nil {
		return nil, nil, false
	}
	var disk cachedInitDisk
	if err := json.Unmarshal(b, &disk); err != nil || len(disk.SecretsRaw) == 0 {
		return nil, nil, false
	}
	var sec *base.Secrets
	if err := pkl.Unmarshal(disk.SecretsRaw, &sec); err != nil {
		return nil, nil, false
	}
	return disk.Machines, sec, true
}

func (c *PlanCache) PutInit(props map[string]string, machines []Machine, secretsRaw []byte) {
	if c == nil || c.disabled || len(secretsRaw) == 0 {
		return
	}
	b, err := json.Marshal(cachedInitDisk{Machines: machines, SecretsRaw: secretsRaw})
	if err != nil {
		return
	}
	c.writeEntry(c.entryPath("init", props, ".json"), b)
}

func (c *PlanCache) GetConfig(props map[string]string) (*pklconfig.Config, bool) {
	if c == nil || c.disabled {
		return nil, false
	}
	raw, err := os.ReadFile(c.entryPath("cfg", props, ".pklbin"))
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	var cfg pklconfig.Config
	if err := pkl.Unmarshal(raw, &cfg); err != nil {
		return nil, false
	}
	return &cfg, true
}

func (c *PlanCache) PutConfig(props map[string]string, raw []byte) {
	if c == nil || c.disabled || len(raw) == 0 {
		return
	}
	c.writeEntry(c.entryPath("cfg", props, ".pklbin"), raw)
}

func (c *PlanCache) writeEntry(dst string, data []byte) {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	_ = os.Chmod(tmpName, 0o600)
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return
	}
	c.prune()
}

func (c *PlanCache) prune() {
	entries, err := os.ReadDir(c.dir)
	if err != nil || len(entries) <= planCacheMaxFiles {
		return
	}
	type item struct {
		path string
		mod  int64
	}
	var items []item
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		if info, err := e.Info(); err == nil {
			items = append(items, item{path: filepath.Join(c.dir, e.Name()), mod: info.ModTime().UnixNano()})
		}
	}
	if len(items) <= planCacheMaxFiles {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod < items[j].mod })
	for i := 0; i < len(items)-planCacheMaxFiles; i++ {
		_ = os.Remove(items[i].path)
	}
}

// ConfigError wraps a non-Pkl config resolution/validation error while preserving its exact message.
type ConfigError struct {
	Err error
}

func (e *ConfigError) Error() string { return e.Err.Error() }
func (e *ConfigError) Unwrap() error { return e.Err }

// LazyEvaluator wraps PlanCache and only starts the external Pkl process on a cache miss.
type LazyEvaluator struct {
	base       string
	configPath string
	progress   io.Writer
	cache      *PlanCache

	mu sync.Mutex
	ev *Evaluator
}

// NewLazyEvaluator creates a cached evaluator that avoids spawning Pkl when cached results match.
func NewLazyEvaluator(baseDir, configPath, pkgChecksum string, progress io.Writer) *LazyEvaluator {
	return &LazyEvaluator{
		base:       baseDir,
		configPath: configPath,
		progress:   progress,
		cache:      NewPlanCache(baseDir, configPath, pkgChecksum),
	}
}

func (l *LazyEvaluator) get(ctx context.Context) (*Evaluator, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ev != nil {
		return l.ev, nil
	}
	ev, err := NewEvaluator(ctx, l.base, l.configPath, l.progress)
	if err != nil {
		return nil, err
	}
	l.ev = ev
	return ev, nil
}

// Init returns cached (machines, secrets) or evaluates with Pkl on miss.
func (l *LazyEvaluator) Init(ctx context.Context, props map[string]string) ([]Machine, *base.Secrets, error) {
	if ms, sec, ok := l.cache.GetInit(props); ok {
		return ms, sec, nil
	}
	ev, err := l.get(ctx)
	if err != nil {
		return nil, nil, err
	}
	ms, sec, rawSec, err := ev.InitRaw(ctx, props)
	if err != nil {
		return nil, nil, err
	}
	l.cache.PutInit(props, ms, rawSec)
	return ms, sec, nil
}

// EvalSpec returns a validated+resolved Spec for machine, reusing cached raw Pkl output on hit.
func (l *LazyEvaluator) EvalSpec(ctx context.Context, machine string, props map[string]string) (*Spec, error) {
	if cfg, ok := l.cache.GetConfig(props); ok {
		spec, err := Resolve(cfg, machine)
		if err != nil {
			return nil, &ConfigError{Err: err}
		}
		return spec, nil
	}
	ev, err := l.get(ctx)
	if err != nil {
		return nil, err
	}
	cfg, raw, err := ev.EvalRaw(ctx, props)
	if err != nil {
		return nil, err
	}
	spec, err := Resolve(cfg, machine)
	if err != nil {
		return nil, &ConfigError{Err: err}
	}
	l.cache.PutConfig(props, raw)
	return spec, nil
}

// Close closes the underlying Pkl evaluator if it was started.
func (l *LazyEvaluator) Close() error {
	l.mu.Lock()
	ev := l.ev
	l.ev = nil
	l.mu.Unlock()
	if ev != nil {
		return ev.Close()
	}
	return nil
}
