package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GitHubBase and CodeloadBase are the GitHub hosts for skill downloads.
var (
	GitHubBase   = "https://github.com"
	CodeloadBase = "https://codeload.github.com"
)

// githubSkills holds the skills that this run read, so that all machines share one read for
// each repository, folder, and ref.
var githubSkills = struct {
	sync.Mutex
	done map[string]*githubSkillResult
}{done: map[string]*githubSkillResult{}}

type githubSkillResult struct {
	once  sync.Once
	ref   string
	files []skillFile
	err   error
}

// loadGitHubSkill downloads a skill folder from a GitHub repository. name is the skill name
// ("" for the last part of the folder path).
func loadGitHubSkill(ctx context.Context, repo, folder string, refp *string, name, origin string) (*SkillSource, error) {
	ref := ""
	if refp != nil {
		ref = *refp
	}
	key := repo + "\x00" + folder + "\x00" + ref
	githubSkills.Lock()
	r, ok := githubSkills.done[key]
	if !ok {
		r = &githubSkillResult{}
		githubSkills.done[key] = r
	}
	githubSkills.Unlock()
	r.once.Do(func() {
		r.ref = ref
		if r.ref == "" {
			r.ref, r.err = latestReleaseOnce(ctx, repo)
			if r.err != nil {
				return
			}
		}
		r.files, r.err = downloadSkill(ctx, repo, folder, r.ref)
	})
	if r.err != nil && ctx.Err() != nil {
		// Do not keep a canceled download: a later call tries again.
		githubSkills.Lock()
		if githubSkills.done[key] == r {
			delete(githubSkills.done, key)
		}
		githubSkills.Unlock()
	}
	if name == "" {
		name = path.Base(folder)
	}
	src := fmt.Sprintf("github.com/%s/%s@%s", repo, folder, r.ref)
	if r.err != nil {
		return nil, fmt.Errorf("skill %s (%s): %w", name, origin, r.err)
	}
	return &SkillSource{Name: name, Dir: src, Origin: origin, files: r.files}, nil
}

// latestTags holds the latest release tag of each repository for this run.
var latestTags = struct {
	sync.Mutex
	done map[string]*latestTag
}{done: map[string]*latestTag{}}

type latestTag struct {
	once sync.Once
	tag  string
	err  error
}

// latestReleaseOnce asks GitHub for the latest release of a repository once in a run, so that
// all skills of one repository come from the same release.
func latestReleaseOnce(ctx context.Context, repo string) (string, error) {
	latestTags.Lock()
	t, ok := latestTags.done[repo]
	if !ok {
		t = &latestTag{}
		latestTags.done[repo] = t
	}
	latestTags.Unlock()
	t.once.Do(func() { t.tag, t.err = latestRelease(ctx, repo) })
	if t.err != nil && ctx.Err() != nil {
		latestTags.Lock()
		if latestTags.done[repo] == t {
			delete(latestTags.done, repo)
		}
		latestTags.Unlock()
	}
	return t.tag, t.err
}

// latestRelease returns the tag of the latest release of a repository. It reads the redirect of
// /releases/latest, so it needs no API token.
func latestRelease(ctx context.Context, repo string) (string, error) {
	u := GitHubBase + "/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	c := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := doRetry(ctx, c, req)
	if err != nil {
		return "", fmt.Errorf("find the latest release of %s: %w", repo, err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	// The tag is everything after /releases/tag/. A tag can contain slashes (skills/v1).
	_, tag, ok := strings.Cut(loc, "/releases/tag/")
	if resp.StatusCode < 300 || resp.StatusCode >= 400 || !ok || tag == "" {
		return "", fmt.Errorf("%s has no release (HTTP %d from %s). Set `ref` of the skill to a tag, branch, or commit", repo, resp.StatusCode, u)
	}
	if t, err := url.PathUnescape(tag); err == nil {
		tag = t
	}
	return tag, nil
}

// Limits of a skill download. The archive is the whole repository, so FAM reads at most
// maxArchive uncompressed bytes, and keeps only the files of the skill folder.
const (
	maxArchive    = 1 << 30
	maxSkillFile  = 16 << 20
	maxSkillBytes = 64 << 20
	maxSkillFiles = 5000
)

// downloadSkill reads the files of one folder from the archive of a repository at a ref. FAM
// keeps the archive in its cache folder, and asks GitHub once in a run whether it changed.
func downloadSkill(ctx context.Context, repo, folder, ref string) ([]skillFile, error) {
	segs := strings.Split(ref, "/")
	for i, sg := range segs {
		segs[i] = url.PathEscape(sg)
	}
	u := CodeloadBase + "/" + repo + "/tar.gz/" + strings.Join(segs, "/")
	dir, err := os.UserCacheDir()
	if err != nil {
		// No cache folder: read the download directly.
		body, _, err := fetchArchive(ctx, u, "")
		if err != nil {
			return nil, err
		}
		defer body.Close()
		return readSkill(body, folder, u)
	}
	// The file name is a hash, so that no repository or ref can give a path outside the folder.
	sum := sha256.Sum256([]byte(repo + "\x00" + ref))
	file := filepath.Join(dir, "fam", "github", hex.EncodeToString(sum[:16])+".tar.gz")
	for attempt := 0; ; attempt++ {
		f, err := openArchive(ctx, file, u)
		if err != nil {
			return nil, err
		}
		files, err := readSkill(f, folder, u)
		var ae *archiveError
		if err != nil && errors.As(err, &ae) && attempt == 0 {
			// A damaged file in the cache: remove it and download it again.
			dropArchive(file, f)
			f.Close()
			continue
		}
		f.Close()
		return files, err
	}
}

// archiveError is an error of the archive data (gzip or tar), not of the skill folder in it.
type archiveError struct{ err error }

func (e *archiveError) Error() string { return e.err.Error() }
func (e *archiveError) Unwrap() error { return e.err }

// badArchive marks an error of the archive data. A too large archive is not damaged: a second
// download does not help.
func badArchive(u string, err error) error {
	err = fmt.Errorf("read the archive %s: %w", u, err)
	if errors.Is(err, errArchiveTooLarge) {
		return err
	}
	return &archiveError{err}
}

// archiveCache holds the state of each cached archive in this run. The lock of an entry makes
// the skills of one repository and ref wait for one download.
var archiveCache = struct {
	sync.Mutex
	entries map[string]*cachedArchive
}{entries: map[string]*cachedArchive{}}

type cachedArchive struct {
	sync.Mutex
	checked bool // GitHub confirmed the file in this run
}

func archiveEntry(file string) *cachedArchive {
	archiveCache.Lock()
	defer archiveCache.Unlock()
	e, ok := archiveCache.entries[file]
	if !ok {
		e = &cachedArchive{}
		archiveCache.entries[file] = e
	}
	return e
}

// A cached archive file starts with a line that holds the ETag of the download. The gzip data
// follows.

// openArchive returns the cached archive, after the first check in this run with GitHub. The
// file is positioned at the gzip data. An open file stays readable when another process
// replaces or removes the cache file.
func openArchive(ctx context.Context, file, u string) (*os.File, error) {
	e := archiveEntry(file)
	e.Lock()
	defer e.Unlock()
	for attempt := 0; ; attempt++ {
		if !e.checked {
			if err := refreshArchive(ctx, file, u); err != nil {
				return nil, err
			}
			e.checked = true
		}
		f, err := os.Open(file)
		if errors.Is(err, fs.ErrNotExist) && attempt == 0 {
			// Another process pruned the file after the check.
			e.checked = false
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := readETag(f); err != nil {
			f.Close()
			if attempt == 0 {
				_ = os.Remove(file)
				e.checked = false
				continue
			}
			return nil, err
		}
		now := time.Now()
		_ = os.Chtimes(file, now, now)
		return f, nil
	}
}

// refreshArchive asks GitHub for the archive with the ETag of the cached file. GitHub answers
// 304 when the archive did not change (also for a branch, or a tag that did not move);
// otherwise FAM saves the new archive.
func refreshArchive(ctx context.Context, file, u string) error {
	etag := ""
	if f, err := os.Open(file); err == nil {
		etag, _ = readETag(f)
		f.Close()
	}
	body, newTag, err := fetchArchive(ctx, u, etag)
	if err != nil {
		return err
	}
	if body == nil {
		return nil // 304: the cached file is current
	}
	defer body.Close()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = fmt.Fprintf(tmp, "%s\n", strings.ReplaceAll(newTag, "\n", ""))
	if err == nil {
		_, err = io.Copy(tmp, &capReader{r: body, left: maxArchive})
	}
	if err != nil {
		tmp.Close()
		return fmt.Errorf("download %s: %w", u, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return err
	}
	pruneArchives(filepath.Dir(file))
	return nil
}

// readETag reads the first line of a cached archive file.
func readETag(f *os.File) (string, error) {
	var b [1]byte
	var line []byte
	for len(line) < 1024 {
		if _, err := io.ReadFull(f, b[:]); err != nil {
			return "", fmt.Errorf("the cached archive %s has no ETag line: %w", f.Name(), err)
		}
		if b[0] == '\n' {
			return string(line), nil
		}
		line = append(line, b[0])
	}
	return "", fmt.Errorf("the cached archive %s has no ETag line", f.Name())
}

// dropArchive removes a damaged cache file, but only when it is still the file that f read: a
// newer download stays. The next openArchive asks GitHub again.
func dropArchive(file string, f *os.File) {
	e := archiveEntry(file)
	e.Lock()
	defer e.Unlock()
	e.checked = false
	a, err1 := f.Stat()
	b, err2 := os.Stat(file)
	if err1 == nil && err2 == nil && os.SameFile(a, b) {
		_ = os.Remove(file)
	}
}

// archiveMaxAge: FAM removes a cached archive that no run used for this time.
const archiveMaxAge = 30 * 24 * time.Hour

// pruneArchives removes old archives and old temporary files from the cache folder. A run that
// still reads a removed file keeps its open file; a later open downloads the archive again.
func pruneArchives(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if time.Since(info.ModTime()) > archiveMaxAge {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// archiveStall: a download fails when no data arrives for this time. A large archive on a slow
// line can take many minutes, so there is no limit for the whole download.
const archiveStall = time.Minute

// fetchArchive starts the download of an archive. With an ETag, it returns a nil body when
// GitHub answers 304 (not modified).
func fetchArchive(ctx context.Context, u, etag string) (io.ReadCloser, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		cancel()
		return nil, "", err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = archiveStall
	resp, err := doRetry(ctx, &http.Client{Transport: tr}, req)
	if err != nil {
		cancel()
		return nil, "", fmt.Errorf("download %s: %w", u, err)
	}
	if resp.StatusCode == http.StatusNotModified && etag != "" {
		resp.Body.Close()
		cancel()
		return nil, etag, nil
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, "", fmt.Errorf("download %s: HTTP %d", u, resp.StatusCode)
	}
	sr := &stallReader{body: resp.Body, cancel: cancel}
	sr.timer = time.AfterFunc(archiveStall, func() { sr.stalled.Store(true); cancel() })
	return sr, resp.Header.Get("ETag"), nil
}

// stallReader cancels its download when no data arrives for archiveStall.
type stallReader struct {
	body    io.ReadCloser
	timer   *time.Timer
	cancel  context.CancelFunc
	stalled atomic.Bool
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.body.Read(p)
	if n > 0 {
		s.timer.Reset(archiveStall)
	}
	if err != nil && err != io.EOF && s.stalled.Load() {
		err = fmt.Errorf("no data for %s", archiveStall)
	}
	return n, err
}

func (s *stallReader) Close() error {
	s.timer.Stop()
	err := s.body.Close()
	s.cancel()
	return err
}

// readSkill reads the files of one folder from a .tar.gz archive of a repository.
func readSkill(archive io.Reader, folder, u string) ([]skillFile, error) {
	capped := &capReader{r: archive, left: maxArchive}
	gz, err := gzip.NewReader(capped)
	if err != nil {
		return nil, badArchive(u, err)
	}
	fail := func(format string, a ...any) ([]skillFile, error) {
		return nil, fmt.Errorf("skill %s in %s: %s", folder, u, fmt.Sprintf(format, a...))
	}
	prefix := strings.Trim(folder, "/") + "/"
	var files []skillFile
	byRel := map[string]int{}
	links := map[string]string{} // rel path in the folder -> link target in the repository
	total := 0
	root := ""
	data := &capReader{r: gz, left: maxArchive}
	tr := tar.NewReader(data)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			// Read the rest of the gzip stream, so that its checksum finds a damaged archive.
			if _, err := io.Copy(io.Discard, data); err != nil {
				return nil, badArchive(u, err)
			}
			break
		}
		if err != nil {
			return nil, badArchive(u, err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		// The archive has one top folder (<name>-<ref>/).
		top, inRepo, _ := strings.Cut(h.Name, "/")
		if root == "" {
			root = top
		} else if top != root {
			return fail("the archive has more than one top folder")
		}
		inRepo = path.Clean(inRepo)
		if !strings.HasPrefix(inRepo, prefix) {
			continue
		}
		rel := strings.TrimPrefix(inRepo, prefix)
		// FAM writes its own marker file into each skill folder: skip that name and anything below it.
		if rel == managedMarker || strings.HasPrefix(rel, managedMarker+"/") || strings.HasPrefix(rel, "../") {
			continue
		}
		switch h.Typeflag {
		case tar.TypeReg:
			if h.Size > maxSkillFile {
				return fail("the file %s has %d bytes, more than the limit of %d", rel, h.Size, maxSkillFile)
			}
			total += int(h.Size)
			if total > maxSkillBytes || len(files) >= maxSkillFiles {
				return fail("the folder has more than %d bytes or %d files", maxSkillBytes, maxSkillFiles)
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, badArchive(u, err)
			}
			byRel[rel] = len(files)
			files = append(files, skillFile{rel: rel, data: data, exec: h.Mode&0o100 != 0})
		case tar.TypeSymlink:
			if path.IsAbs(h.Linkname) {
				links[rel] = h.Linkname
			} else {
				links[rel] = path.Clean(path.Join(path.Dir(rel), h.Linkname))
			}
		}
	}
	// A link in the folder becomes a copy of the file that it points to (as for local skills).
	// The target must be a file in the same folder.
	// The copies count against the same limits as regular files.
	for rel, target := range links {
		// Follow a chain of links (alias.md -> other.md -> file), at most 16 steps.
		for hops := 0; hops < 16; hops++ {
			next, isLink := links[target]
			if !isLink {
				break
			}
			target = next
		}
		i, ok := byRel[target]
		if !ok || path.IsAbs(target) {
			return fail("the link %s does not point to a file in the skill folder", rel)
		}
		f := files[i]
		total += len(f.data)
		if total > maxSkillBytes || len(files) >= maxSkillFiles {
			return fail("the folder has more than %d bytes or %d files", maxSkillBytes, maxSkillFiles)
		}
		files = append(files, skillFile{rel: rel, data: f.data, exec: f.exec})
	}
	hasSkill := false
	for _, f := range files {
		if f.rel == "SKILL.md" {
			hasSkill = true
		}
	}
	if !hasSkill {
		return fail("the folder has no SKILL.md")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}

// errArchiveTooLarge: the archive has more than maxArchive bytes.
var errArchiveTooLarge = fmt.Errorf("the archive is larger than %d bytes", int64(maxArchive))

// capReader stops with errArchiveTooLarge after `left` bytes.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errArchiveTooLarge
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// doRetry sends a GET request up to three times when the network fails (for example a DNS
// timeout). An HTTP response, also an error status, is not retried.
func doRetry(ctx context.Context, c *http.Client, req *http.Request) (*http.Response, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		var resp *http.Response
		resp, err = c.Do(req.Clone(ctx))
		if err == nil {
			return resp, nil
		}
		// Only a network failure (DNS, connect, reset, timeout) is worth another try. Client.Do
		// wraps every error in *url.Error, which is itself a net.Error, so look at the cause.
		cause := err
		var ue *url.Error
		if errors.As(err, &ue) {
			cause = ue.Err
		}
		var ne net.Error
		if ctx.Err() != nil || !errors.As(cause, &ne) {
			return nil, err
		}
	}
	return nil, err
}
