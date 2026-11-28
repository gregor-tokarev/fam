package facts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/gregor-tokarev/fam/internal/transport"
)

const factsCacheDirName = "facts-cache-v2"

// CachedFacts holds the parsed facts alongside the SHA-256 of the trimmed raw facts script output
// so speculative evaluation can verify exact parity on connect.
type CachedFacts struct {
	Facts     Facts
	RawSHA256 string
}

type cachedFactsDisk struct {
	RawStdout []byte `json:"rawStdout"`
	RawSHA256 string `json:"rawSha256"`
}

// RawHash returns the SHA-256 hex digest of the facts script output with trailing CR/LF trimmed,
// delegating to transport.RawFactsSHA so Go and wire_worker.py share one canonical digest.
func RawHash(stdout []byte) string {
	return transport.RawFactsSHA(stdout)
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

// LoadCache loads the last raw facts output for an SSH target and re-parses it with the current
// Parse logic so parser changes never serve stale derived properties.
func LoadCache(target, sshConfig, machine string, cockpit bool, tags []string) (CachedFacts, bool) {
	p := factsCachePath(target, sshConfig)
	if p == "" {
		return CachedFacts{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return CachedFacts{}, false
	}
	var disk cachedFactsDisk
	if err := json.Unmarshal(b, &disk); err != nil || disk.RawSHA256 == "" || len(disk.RawStdout) == 0 {
		return CachedFacts{}, false
	}
	if RawHash(disk.RawStdout) != disk.RawSHA256 {
		return CachedFacts{}, false
	}
	f, err := Parse(context.Background(), nil, transport.Result{Stdout: disk.RawStdout, Code: 0}, machine, cockpit, tags)
	if err != nil {
		return CachedFacts{}, false
	}
	return CachedFacts{Facts: f, RawSHA256: disk.RawSHA256}, true
}

// SaveCache atomically persists the raw facts stdout and digest for an SSH target.
func SaveCache(target, sshConfig string, rawStdout []byte, rawSHA string) {
	p := factsCachePath(target, sshConfig)
	if p == "" || rawSHA == "" || len(rawStdout) == 0 {
		return
	}
	b, err := json.Marshal(cachedFactsDisk{RawStdout: rawStdout, RawSHA256: rawSHA})
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
