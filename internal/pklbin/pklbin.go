// Package pklbin downloads the pinned pkl binary into the FAM cache and checks its SHA-256.
package pklbin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gregor-tokarev/fam/internal/version"
)

type asset struct {
	name   string
	sha256 string
}

// assets are the release files of pkl 0.33.0 and their SHA-256 digests.
var assets = map[string]asset{
	"linux/amd64":      {"pkl-linux-amd64", "b9a6ddea6b382b7d4db4cf78e77aa3858e0c8a7e02939d2ba298574b5d1fcc69"},
	"linux/amd64/musl": {"pkl-alpine-linux-amd64", "38e983b0953018888761c8b410ccdc6f27ce77407ad306e73e6369c07bc23d1f"},
	"linux/arm64":      {"pkl-linux-aarch64", "edcea361402ecab0f3a93f86b1a896c41d00a9241f4962f70278254b08fbd26d"},
	"darwin/arm64":     {"pkl-macos-aarch64", "e39c157df0f00a35d34b46270a768784edcaf509aafbf743849b27b4c83ef83b"},
}

// BaseURL is the download location of the pkl release files.
var BaseURL = "https://github.com/apple/pkl/releases/download/" + version.PklVersion + "/"

func platformKey() string {
	key := runtime.GOOS + "/" + runtime.GOARCH
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" && isMusl() {
		key += "/musl"
	}
	return key
}

func isMusl() bool {
	matches, _ := filepath.Glob("/lib/ld-musl-*.so.1")
	return len(matches) > 0
}

// CacheDir is the directory that holds the pinned pkl binary.
func CacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find the cache directory: %w", err)
	}
	return filepath.Join(dir, "fam", "pkl", version.PklVersion), nil
}

// CachedPath finds and validates the pinned binary without downloads or writes.
func CachedPath() (string, error) {
	a, ok := assets[platformKey()]
	if !ok {
		return "", fmt.Errorf("pkl %s has no binary for %s", version.PklVersion, platformKey())
	}
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, a.name)
	if st, err := os.Stat(bin); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
		if b, err := os.ReadFile(bin + ".sha256"); err == nil && strings.TrimSpace(string(b)) == a.sha256 {
			return bin, nil
		}
		if sum, err := fileSHA256(bin); err == nil && sum == a.sha256 {
			return bin, nil
		}
	}
	return "", fmt.Errorf("pkl %s is not cached. Run `fam internal pkl-path`", version.PklVersion)
}

// Path returns the path of the pinned pkl binary. It downloads the binary on first use.
func Path(ctx context.Context, progress io.Writer) (string, error) {
	key := platformKey()
	a, ok := assets[key]
	if !ok {
		return "", fmt.Errorf("pkl %s has no native binary for %s. FAM cannot evaluate the config on this machine", version.PklVersion, key)
	}
	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, a.name)
	stamp := bin + ".sha256"
	if st, err := os.Stat(bin); err == nil && st.Mode()&0o111 != 0 {
		if b, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(b)) == a.sha256 {
			return bin, nil
		}
		// No stamp: check the file again.
		if sum, err := fileSHA256(bin); err == nil && sum == a.sha256 {
			_ = os.WriteFile(stamp, []byte(a.sha256+"\n"), 0o644)
			return bin, nil
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if progress != nil {
		fmt.Fprintf(progress, "Downloading pkl %s (%s) into %s\n", version.PklVersion, a.name, dir)
	}
	if err := download(ctx, BaseURL+a.name, bin, a.sha256); err != nil {
		return "", err
	}
	if err := os.WriteFile(stamp, []byte(a.sha256+"\n"), 0o644); err != nil {
		return "", err
	}
	return bin, nil
}

func download(ctx context.Context, url, dest, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download pkl: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download pkl from %s: HTTP %s", url, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".pkl-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("download pkl: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("download pkl: SHA-256 mismatch for %s: got %s, want %s", url, got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
