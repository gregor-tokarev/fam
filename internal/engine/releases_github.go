package engine

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gregor-tokarev/fam/internal/pklgen/tasks"
	"github.com/gregor-tokarev/fam/internal/transport"
	"github.com/ulikunitz/xz"
)

const maxReleaseBinary = 512 << 20
const maxReleaseAsset = 1 << 30

// GitHubReleaseTask installs one binary and compares its version with a release tag.
type GitHubReleaseTask struct {
	base
	Repo, Binary   string
	Assets         map[string]tasks.ReleaseAsset
	Tag            *string
	VersionCommand *string
	Absent         bool
	units          []Unit
}

func (t *GitHubReleaseTask) Kind() string           { return "github-release" }
func (t *GitHubReleaseTask) Name() string           { return t.Binary + " (" + t.Repo + ")" }
func (t *GitHubReleaseTask) Restarts() []Unit       { return t.units }
func (t *GitHubReleaseTask) binary(env *Env) string { return env.Home(".local/bin/" + t.Binary) }
func (t *GitHubReleaseTask) target(ctx context.Context, env *Env) (string, tasks.ReleaseAsset, string, error) {
	platform := env.Facts.OS + "-" + env.Facts.Arch
	if env.Facts.OS == "linux" && env.Facts.Libc == "musl" {
		if _, ok := t.Assets[platform+"-musl"]; ok {
			platform += "-musl"
		}
	}
	a, ok := t.Assets[platform]
	if !ok {
		return "", a, "", fmt.Errorf("release %s has no asset for %s", t.Repo, platform)
	}
	if a.Format != "binary" && a.File == nil {
		return "", a, "", fmt.Errorf("release %s: archive asset %s needs file", t.Repo, a.Name)
	}
	tag := ""
	var err error
	if t.Tag != nil {
		tag = *t.Tag
	} else {
		tag, err = latestReleaseOnce(ctx, t.Repo)
	}
	if err != nil {
		return "", a, "", err
	}
	if a.File != nil {
		file := expandReleaseName(*a.File, tag)
		a.File = &file
	}
	name := expandReleaseName(a.Name, tag)
	a.Name = name
	return tag, a, name, nil
}

func expandReleaseName(name, tag string) string {
	return strings.NewReplacer("{tag}", tag, "{version}", strings.TrimPrefix(tag, "v")).Replace(name)
}

// releaseRecord identifies a binary that FAM installed. Its digest also detects drift
// before FAM runs the binary's version command.
type releaseRecord struct {
	Repo   string `json:"repo"`
	SHA256 string `json:"sha256"`
}

func (t *GitHubReleaseTask) recordPath(env *Env) string {
	return env.Home(".local/state/fam/releases/" + t.Binary + ".json")
}

// ownership never adopts an existing file implicitly, even if its version matches.
func (t *GitHubReleaseTask) ownership(ctx context.Context, env *Env) (transport.FileInfo, *releaseRecord, error) {
	fi, err := env.T.Stat(ctx, t.binary(env), false)
	if err != nil {
		return fi, nil, err
	}
	p := t.recordPath(env)
	ri, err := env.T.Stat(ctx, p, false)
	if err != nil {
		return fi, nil, err
	}
	var record *releaseRecord
	if ri.Exists {
		if ri.Type != "file" {
			return fi, nil, conflict(p + " is not a regular FAM release record. Remove it and the binary, then apply again")
		}
		data, exists, err := env.T.ReadFile(ctx, p, false)
		if err != nil {
			return fi, nil, err
		}
		var r releaseRecord
		if err := json.Unmarshal(data, &r); !exists || err != nil || !releaseDigest.MatchString(r.SHA256) || !strings.EqualFold(r.Repo, t.Repo) {
			return fi, nil, conflict(p + " is invalid or belongs to another repository. Remove the binary and its record, then apply again")
		}
		record = &r
	}
	if fi.Exists && record == nil {
		return fi, nil, conflict(fmt.Sprintf("%s exists and FAM did not make it. Remove the file, then apply to let FAM manage it", t.binary(env)))
	}
	if fi.Exists && fi.Type != "file" && fi.Type != "link" {
		return fi, nil, conflict(t.binary(env) + " is not a regular file or link. Remove it, then apply again")
	}
	return fi, record, nil
}

var releaseDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (t *GitHubReleaseTask) Check(ctx context.Context, env *Env) (Check, error) {
	fi, record, err := t.ownership(ctx, env)
	if err != nil {
		return Check{}, err
	}
	if t.Absent {
		if record != nil {
			return Check{Status: Change, Detail: "remove binary and FAM record"}, nil
		}
		return Check{Status: Satisfied}, nil
	}
	tag, _, _, err := t.target(ctx, env)
	if err != nil {
		return Check{}, err
	}
	if !fi.Exists {
		return Check{Status: Change, Detail: "install " + tag}, nil
	}
	if fi.Type == "link" {
		return Check{Status: Change, Detail: "replace changed binary"}, nil
	}
	if fi.SHA256 != record.SHA256 {
		return Check{Status: Change, Detail: "replace changed binary"}, nil
	}
	if fi.Mode.Perm()&0o111 == 0 {
		return Check{Status: Change, Detail: "repair executable mode"}, nil
	}
	cmd := transport.Quote(t.binary(env)) + " --version"
	if t.VersionCommand != nil {
		cmd = *t.VersionCommand
	}
	r, err := env.UserScript(ctx, cmd, false, "version of "+t.Binary, 2*time.Minute)
	if err != nil {
		return Check{}, err
	}
	if r.Code != 0 {
		return Check{}, &ExitError{Label: "version of " + t.Binary, Code: r.Code, Stderr: r.Err(), Stdout: r.Out()}
	}
	version := strings.TrimSpace(r.Out() + "\n" + r.Err())
	if !versionIs(version, tag) {
		return Check{Status: Change, Detail: "installed " + firstLine(version) + ", release " + tag}, nil
	}
	return Check{Status: Satisfied, Detail: tag}, nil
}

func (t *GitHubReleaseTask) Apply(ctx context.Context, env *Env) (string, error) {
	_, record, err := t.ownership(ctx, env)
	if err != nil {
		return "", err
	}
	if t.Absent {
		if record == nil {
			return "unchanged", nil
		}
		_, err := env.ShOK(ctx, "rm -f -- "+transport.Quote(t.binary(env))+" "+transport.Quote(t.recordPath(env)), false, "remove "+t.Binary)
		return "removed", err
	}
	tag, a, name, err := t.target(ctx, env)
	if err != nil {
		return "", err
	}
	// Asset links come from GitHub's release fragment. This avoids API rate limits.
	assets, err := releaseAssets(ctx, t.Repo, tag)
	if err != nil {
		return "", err
	}
	if !assets[name] {
		return "", fmt.Errorf("release %s@%s has no asset %s", t.Repo, tag, name)
	}
	data, err := releaseDownload(ctx, t.Repo, tag, name, maxReleaseAsset)
	if err != nil {
		return "", err
	}
	defer removeReleaseDownload(data)
	checksum := ""
	if a.Checksum != nil {
		checksum = expandReleaseName(*a.Checksum, tag)
		if !assets[checksum] {
			return "", fmt.Errorf("release %s@%s has no checksum asset %s", t.Repo, tag, checksum)
		}
	} else {
		checksum = findChecksum(assets, name)
	}
	if checksum != "" {
		checksFile, err := releaseDownload(ctx, t.Repo, tag, checksum, maxSkillFile)
		if err != nil {
			return "", err
		}
		defer removeReleaseDownload(checksFile)
		checks, err := io.ReadAll(checksFile)
		if err != nil {
			return "", err
		}
		if err := checkReleaseDigest(data, checks, name, strings.HasPrefix(checksum, name+".")); err != nil {
			return "", err
		}
	}
	if _, err := data.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	binary, err := releaseBinary(data, a)
	if err != nil {
		return "", fmt.Errorf("release asset %s: %w", name, err)
	}
	// Recheck ownership after the download, before replacing any file.
	fi, _, err := t.ownership(ctx, env)
	if err != nil {
		return "", err
	}
	// Remove a changed link first, so mv cannot follow a link to a directory.
	if fi.Type == "link" {
		if _, err := env.ShOK(ctx, "rm -f -- "+transport.Quote(t.binary(env)), false, "remove changed link "+t.Binary); err != nil {
			return "", err
		}
	}
	if err := env.T.WriteFile(ctx, t.binary(env), binary, 0o755, false); err != nil {
		return "", err
	}
	sum := sha256.Sum256(binary)
	recordData, err := json.Marshal(releaseRecord{Repo: t.Repo, SHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		return "", err
	}
	if err := env.T.WriteFile(ctx, t.recordPath(env), append(recordData, '\n'), 0o600, false); err != nil {
		return "", err
	}
	return "installed " + tag, nil
}

var releaseLink = regexp.MustCompile(`href="([^"]+)"`)

func releaseAssets(ctx context.Context, repo, tag string) (map[string]bool, error) {
	u := GitHubBase + "/" + repo + "/releases/expanded_assets/" + url.PathEscape(tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := doRetry(ctx, &http.Client{Timeout: time.Minute}, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list release assets %s: HTTP %d", u, resp.StatusCode)
	}
	data, err := io.ReadAll(&capReader{r: resp.Body, left: maxSkillFile,
		limitError: fmt.Errorf("release asset list %s is larger than the limit of %d bytes", u, maxSkillFile)})
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	prefix := "/" + repo + "/releases/download/" + tag + "/"
	for _, match := range releaseLink.FindAllSubmatch(data, -1) {
		link, err := url.PathUnescape(html.UnescapeString(string(match[1])))
		if err != nil {
			continue
		}
		if strings.HasPrefix(link, prefix) {
			name := strings.TrimPrefix(link, prefix)
			if name != "" && !strings.Contains(name, "/") {
				out[name] = true
			}
		}
	}
	return out, nil
}

// releaseDownload shares the ETag cache, retry rules, and stall limit of skill downloads.
// Keep the asset on disk, so only the extracted binary needs a large memory buffer.
func releaseDownload(ctx context.Context, repo, tag, name string, limit int64) (*os.File, error) {
	u := GitHubBase + "/" + repo + "/releases/download/" + url.PathEscape(tag) + "/" + url.PathEscape(name)
	a, direct, err := cachedDownloadFor(ctx, "release:"+repo, tag+"\x00"+name, u, false, limit)
	tooLarge := fmt.Errorf("release asset %s is larger than the limit of %d bytes", name, limit)
	var r io.Reader
	if errors.Is(err, errNoCache) {
		if direct == nil {
			direct, err = fetchArchive(ctx, u, "")
			if err != nil {
				return nil, err
			}
		}
		defer direct.Close()
		r = direct
	} else if err != nil {
		if errors.Is(err, errArchiveTooLarge) {
			return nil, tooLarge
		}
		return nil, err
	} else {
		r = a.reader()
	}
	f, err := os.CreateTemp("", "fam-release-*")
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(f, &capReader{r: r, left: limit, limitError: tooLarge}); err != nil {
		removeReleaseDownload(f)
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		removeReleaseDownload(f)
		return nil, err
	}
	return f, nil
}

func removeReleaseDownload(f *os.File) {
	f.Close()
	os.Remove(f.Name())
}

func findChecksum(assets map[string]bool, name string) string {
	for _, n := range []string{name + ".sha256", name + ".sha256sum", name + ".sha256.txt"} {
		if assets[n] {
			return n
		}
	}
	var names []string
	for n := range assets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		low := strings.ToLower(n)
		if strings.HasSuffix(low, ".asc") || strings.HasSuffix(low, ".sig") {
			continue
		}
		if (strings.Contains(low, "sha256") && !strings.Contains(low, ".tar.") && !strings.Contains(low, ".zip.")) || strings.Contains(low, "checksums") || low == "checksum.txt" {
			return n
		}
	}
	return ""
}

func checkReleaseDigest(data io.Reader, checks []byte, name string, single bool) error {
	h := sha256.New()
	if _, err := io.Copy(h, data); err != nil {
		return err
	}
	want := hex.EncodeToString(h.Sum(nil))
	for _, line := range strings.Split(string(checks), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		digest := ""
		if len(fields) == 1 && single {
			digest = fields[0]
		}
		if len(fields) >= 2 && strings.TrimPrefix(fields[1], "*") == name {
			digest = fields[0]
		}
		if len(fields) == 4 && fields[0] == "SHA256" && fields[1] == "("+name+")" && fields[2] == "=" {
			digest = fields[3]
		}
		if digest == "" {
			continue
		}
		if strings.EqualFold(digest, want) {
			return nil
		}
		return fmt.Errorf("SHA-256 checksum does not match release asset %s", name)
	}
	return fmt.Errorf("the checksum file has no SHA-256 for release asset %s", name)
}

func releaseBinary(data *os.File, a tasks.ReleaseAsset) ([]byte, error) {
	read := func(r io.Reader, size int64, name string) ([]byte, error) {
		if size <= 0 || size > maxReleaseBinary {
			return nil, fmt.Errorf("binary %s has %d bytes; size must be between 1 and %d bytes", name, size, maxReleaseBinary)
		}
		b := make([]byte, int(size))
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		// Read through EOF to check the checksum of a zip member.
		var probe [1]byte
		if n, err := r.Read(probe[:]); n != 0 || err != io.EOF {
			if err != nil && err != io.EOF {
				return nil, err
			}
			return nil, fmt.Errorf("binary %s does not match its declared size of %d bytes", name, size)
		}
		return b, nil
	}
	if a.Format == "binary" {
		st, err := data.Stat()
		if err != nil {
			return nil, err
		}
		return read(data, st.Size(), a.Name)
	}
	if a.File == nil {
		return nil, fmt.Errorf("an archive needs file")
	}
	wanted := path.Clean(*a.File)
	switch a.Format {
	case "tar.gz", "tar.xz":
		var decoded io.Reader
		if a.Format == "tar.gz" {
			gz, err := gzip.NewReader(data)
			if err != nil {
				return nil, err
			}
			defer gz.Close()
			decoded = gz
		} else {
			xr, err := xz.NewReader(data)
			if err != nil {
				return nil, err
			}
			decoded = xr
		}
		capped := &capReader{r: decoded, left: maxReleaseAsset,
			limitError: fmt.Errorf("uncompressed archive is larger than the limit of %d bytes", maxReleaseAsset)}
		tr := tar.NewReader(capped)
		var binary []byte
		entries := 0
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			entries++
			if entries > maxSkillFiles {
				return nil, fmt.Errorf("archive has more than %d entries", maxSkillFiles)
			}
			if path.Clean(h.Name) != wanted {
				continue
			}
			if h.Typeflag != tar.TypeReg || binary != nil {
				return nil, fmt.Errorf("%s must be one regular file", wanted)
			}
			binary, err = read(tr, h.Size, wanted)
			if err != nil {
				return nil, err
			}
		}
		if _, err := io.Copy(io.Discard, capped); err != nil {
			return nil, err
		}
		if binary != nil {
			return binary, nil
		}
	case "zip":
		st, err := data.Stat()
		if err != nil {
			return nil, err
		}
		zr, err := zip.NewReader(data, st.Size())
		if err != nil {
			return nil, err
		}
		if len(zr.File) > maxSkillFiles {
			return nil, fmt.Errorf("archive has more than %d entries", maxSkillFiles)
		}
		var binary []byte
		for _, f := range zr.File {
			if path.Clean(f.Name) != wanted {
				continue
			}
			if !f.Mode().IsRegular() || binary != nil {
				return nil, fmt.Errorf("%s must be one regular file", wanted)
			}
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			if f.UncompressedSize64 == 0 || f.UncompressedSize64 > maxReleaseBinary {
				r.Close()
				return nil, fmt.Errorf("binary %s has %d bytes; size must be between 1 and %d bytes", wanted, f.UncompressedSize64, maxReleaseBinary)
			}
			binary, err = read(r, int64(f.UncompressedSize64), wanted)
			r.Close()
			if err != nil {
				return nil, err
			}
		}
		if binary != nil {
			return binary, nil
		}
	default:
		return nil, fmt.Errorf("unknown archive format %q", a.Format)
	}
	return nil, fmt.Errorf("archive has no binary %s", wanted)
}
