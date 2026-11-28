package facts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
)

const factsCacheDirName = "facts-cache-v1"

// CachedFacts stores the last observed facts of a remote SSH target alongside the SHA-256 of the
// trimmed raw facts script output so speculative evaluation can verify exact parity on connect.
type CachedFacts struct {
	Facts     Facts  `json:"facts"`
	RawSHA256 string `json:"rawSha256"`
}

// RawHash returns the SHA-256 hex digest of the facts script output with trailing CR/LF trimmed.
// Both Go and the remote ssh_wire worker compute this exact digest.
func RawHash(stdout []byte) string {
	trimmed := bytes.TrimRight(stdout, "\r\n")
	sum := sha256.Sum256(trimmed)
	return hex.EncodeToString(sum[:])
}

func factsCacheDir() string {
	if x := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, "fam", factsCacheDirName)
	}
	if d, err := os.UserCacheDir(); err == nil && d != "" {
		return filepath.Join(d, "fam", factsCacheDirName)
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".cache", "fam", factsCacheDirName)
	}
	return ""
}

func factsCachePath(target, sshConfig string) string {
	dir := factsCacheDir()
	if dir == "" || os.Getenv("FAM_NO_FACTS_CACHE") == "1" {
		return ""
	}
	h := sha256.Sum256([]byte(target + "\x00" + sshConfig))
	return filepath.Join(dir, hex.EncodeToString(h[:])+".json")
}

// LoadCache returns the cached facts for an SSH target, if available.
func LoadCache(target, sshConfig string) (CachedFacts, bool) {
	p := factsCachePath(target, sshConfig)
	if p == "" {
		return CachedFacts{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return CachedFacts{}, false
	}
	var cf CachedFacts
	if err := json.Unmarshal(b, &cf); err != nil || cf.RawSHA256 == "" || cf.Facts.OS == "" {
		return CachedFacts{}, false
	}
	return cf, true
}

// SaveCache atomically persists the observed facts and raw digest for an SSH target.
func SaveCache(target, sshConfig string, cf CachedFacts) {
	p := factsCachePath(target, sshConfig)
	if p == "" || cf.RawSHA256 == "" || cf.Facts.OS == "" {
		return
	}
	b, err := json.Marshal(cf)
	if err != nil {
		return
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	_ = os.Chmod(tmpName, 0o600)
	if err := os.Rename(tmpName, p); err != nil {
		os.Remove(tmpName)
	}
}
