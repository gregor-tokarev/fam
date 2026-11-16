// Package selfupdate replaces the running fam binary with a release from GitHub.
package selfupdate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ReleasesURL is the GitHub releases page of fam.
var ReleasesURL = "https://github.com/gregor-tokarev/fam/releases"

// platforms are the release builds. They match the platforms of the pinned pkl binary.
var platforms = map[string]bool{"linux/amd64": true, "linux/arm64": true, "darwin/arm64": true}

// AssetName is the release archive of this platform.
func AssetName() (string, error) {
	p := runtime.GOOS + "/" + runtime.GOARCH
	if !platforms[p] {
		if p == "darwin/amd64" {
			return "", errors.New("there is no fam release for Intel Macs: pkl, which fam uses to read the config, has no build for them")
		}
		return "", fmt.Errorf("there is no fam release for %s", p)
	}
	return fmt.Sprintf("fam_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH), nil
}

// Latest returns the version of the latest release, without the "v". It reads the redirect of
// /releases/latest, so it needs no API token and no API rate limit.
func Latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ReleasesURL+"/latest", nil)
	if err != nil {
		return "", err
	}
	c := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot reach GitHub: %w", err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
		return "", fmt.Errorf("cannot find the latest release at %s/latest (HTTP %d)", ReleasesURL, resp.StatusCode)
	}
	tag := path.Base(loc)
	if !strings.HasPrefix(tag, "v") || path.Base(path.Dir(loc)) != "tag" {
		return "", fmt.Errorf("cannot find the latest release at %s/latest (redirect to %s)", ReleasesURL, loc)
	}
	return strings.TrimPrefix(tag, "v"), nil
}

// Compare compares two semantic versions (1.2.3, 1.2.3-rc.1, with or without "v"). It
// returns -1, 0, or 1. A prerelease is lower than its release, and build metadata (+...) does
// not count.
func Compare(a, b string) int {
	ca, pa := splitVersion(a)
	cb, pb := splitVersion(b)
	for i := 0; i < 3; i++ {
		if c := cmpNum(ca[i], cb[i]); c != 0 {
			return c
		}
	}
	switch {
	case pa == "" && pb == "":
		return 0
	case pa == "":
		return 1
	case pb == "":
		return -1
	}
	ia, ib := strings.Split(pa, "."), strings.Split(pb, ".")
	for i := 0; i < len(ia) && i < len(ib); i++ {
		na, ea := strconv.ParseUint(ia[i], 10, 64)
		nb, eb := strconv.ParseUint(ib[i], 10, 64)
		var c int
		switch {
		case ea == nil && eb == nil:
			c = cmpNum(na, nb)
		case ea == nil: // numeric identifiers are lower than alphanumeric ones
			c = -1
		case eb == nil:
			c = 1
		default:
			c = strings.Compare(ia[i], ib[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmpNum(uint64(len(ia)), uint64(len(ib)))
}

// splitVersion returns the three numbers of the core version (missing or invalid numbers are 0)
// and the prerelease part.
func splitVersion(v string) ([3]uint64, string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "+")
	core, pre, _ := strings.Cut(v, "-")
	var n [3]uint64
	for i, part := range strings.SplitN(core, ".", 3) {
		n[i], _ = strconv.ParseUint(part, 10, 64)
	}
	return n, pre
}

func cmpNum(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Exists tells whether the release `version` has a checksums.txt, so that `fam update --version`
// can install it.
func Exists(ctx context.Context, version string) error {
	_, err := get(ctx, fmt.Sprintf("%s/download/v%s/checksums.txt", ReleasesURL, strings.TrimPrefix(version, "v")))
	return err
}

// Install downloads the release `version` (without the "v"), checks its SHA-256 against
// checksums.txt, and replaces the binary at exe.
func Install(ctx context.Context, version, exe string) error {
	asset, err := AssetName()
	if err != nil {
		return err
	}
	base := fmt.Sprintf("%s/download/v%s/", ReleasesURL, strings.TrimPrefix(version, "v"))
	sums, err := get(ctx, base+"checksums.txt")
	if err != nil {
		return err
	}
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && f[1] == asset {
			want = f[0]
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt of release v%s has no entry for %s", version, asset)
	}
	archive, err := get(ctx, base+asset)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("the SHA-256 of %s is %s, but checksums.txt says %s", asset, got, want)
	}
	bin, err := extract(archive, "fam")
	if err != nil {
		return fmt.Errorf("%s: %w", asset, err)
	}
	// Write a new file next to the binary, then rename: the rename is atomic, and a running fam
	// keeps its old file. CreateTemp makes a new file with a random name (O_EXCL), so it never
	// writes through an existing file or link.
	f, err := os.CreateTemp(filepath.Dir(exe), ".fam.new.*")
	if err != nil {
		return writeErr(exe, err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(bin); err != nil {
		f.Close()
		return writeErr(exe, err)
	}
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return writeErr(exe, err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		return writeErr(exe, err)
	}
	ok = true
	return nil
}

func writeErr(exe string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("cannot replace %s: %w. Run `fam update` as the owner of that file, or install fam again with the install script", exe, err)
	}
	return fmt.Errorf("cannot replace %s: %w", exe, err)
}

// Executable returns the path of the running fam binary, with symbolic links resolved.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// downloadClient stops a download that takes longer than 5 minutes.
var downloadClient = &http.Client{Timeout: 5 * time.Minute}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download of %s failed: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download of %s failed: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

func extract(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("the archive has no file %s", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Clean(h.Name) == name {
			return io.ReadAll(io.LimitReader(tr, 256<<20))
		}
	}
}
