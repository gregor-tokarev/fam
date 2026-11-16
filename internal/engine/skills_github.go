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
	for attempt := 0; ; attempt++ {
		a, err := cachedArchiveFor(ctx, repo, ref, u, attempt > 0)
		if errors.Is(err, errNoCache) {
			// The cache folder cannot keep the archive: read the download directly.
			body, err := fetchArchive(ctx, u, "")
			if err != nil {
				return nil, err
			}
			defer body.Close()
			return readSkill(body, folder, u)
		}
		if err != nil {
			return nil, err
		}
		files, err := readSkill(a.reader(), folder, u)
		var ae *archiveError
		if err != nil && errors.As(err, &ae) && attempt == 0 {
			// A damaged file in the cache: download it again, without the ETag.
			continue
		}
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

// errNoCache: the cache folder cannot keep an archive (no cache folder, no write access, or a
// full disk).
var errNoCache = errors.New("the cache folder cannot keep the archive")

// A cached archive file starts with a line that holds the ETag of the download. The gzip data
// follows.

// archiveCache holds the archive of each repository and ref that this run checked. The lock of
// an entry makes all skills of one repository and ref wait for one download.
var archiveCache = struct {
	sync.Mutex
	entries map[string]*runArchive
}{entries: map[string]*runArchive{}}

// runArchive is an open cache file that GitHub confirmed in this run. All skills of the run read
// this same file, also when another process replaces or removes its path.
type runArchive struct {
	mu   sync.Mutex
	file *os.File
	data int64 // offset of the gzip data
	size int64
	bad  *os.File // the file that a reader found damaged
}

func (a *runArchive) reader() io.Reader {
	a.mu.Lock()
	defer a.mu.Unlock()
	return io.NewSectionReader(a.file, a.data, a.size-a.data)
}

// cachedArchiveFor returns the checked archive of this run. With repair, a damaged archive is
// downloaded again (once for each damaged file).
func cachedArchiveFor(ctx context.Context, repo, ref, u string, repair bool) (*runArchive, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, errNoCache
	}
	dir = filepath.Join(dir, "fam", "github")
	// The file name is a hash, so that no repository or ref can give a path outside the folder.
	sum := sha256.Sum256([]byte(repo + "\x00" + ref))
	path := filepath.Join(dir, hex.EncodeToString(sum[:16])+".tar.gz")
	archiveCache.Lock()
	a, ok := archiveCache.entries[path]
	if !ok {
		a = &runArchive{}
		archiveCache.entries[path] = a
	}
	archiveCache.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file != nil && repair && a.bad == nil {
		a.bad = a.file
		a.file = nil
	}
	if a.file != nil {
		return a, nil
	}
	pruneOnce.Do(func() { pruneArchives(dir) })
	// Read the ETag from the file that a 304 confirms, also when another process replaces the
	// path meanwhile.
	var old *os.File
	etag := ""
	if !repair {
		if f, err := os.Open(path); err == nil {
			old = f
			etag, _ = readETag(f)
		}
	}
	f, err := refreshArchive(ctx, dir, path, u, old, etag)
	if err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := readETag(f); err != nil {
		f.Close()
		return nil, badArchive(u, err)
	}
	pos, _ := f.Seek(0, io.SeekCurrent)
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	a.file, a.data, a.size = f, pos, st.Size()
	return a, nil
}

// refreshArchive asks GitHub for the archive with the ETag of the cached file old, and returns
// an open cache file. GitHub answers 304 when the archive did not change (also for a branch, or
// a tag that did not move): then old is the result. Otherwise FAM saves the new archive with an
// atomic rename. refreshArchive closes old when it does not return it.
func refreshArchive(ctx context.Context, dir, path, u string, old *os.File, etag string) (*os.File, error) {
	if old != nil && etag == "" {
		old.Close()
		old = nil
	}
	body, err := fetchArchive(ctx, u, etag)
	if err != nil {
		if old != nil {
			old.Close()
		}
		return nil, err
	}
	if body == nil {
		return old, nil // 304
	}
	if old != nil {
		old.Close()
	}
	defer body.Close()
	return saveArchive(dir, path, u, body)
}

// saveArchive writes a download into a temporary file in the cache folder, renames it to path,
// and returns it open. An error of the cache folder gives errNoCache.
func saveArchive(dir, path, u string, body *stallReader) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errNoCache
	}
	tmp, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return nil, errNoCache
	}
	keep := false
	defer func() {
		if !keep {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	w := &trackWriter{w: tmp}
	if _, err := fmt.Fprintf(w, "%s\n", body.etag); err != nil {
		return nil, errNoCache
	}
	if _, err := io.Copy(w, &capReader{r: body, left: maxArchive}); err != nil {
		if w.err != nil {
			return nil, errNoCache
		}
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, errNoCache
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	keep = true
	return tmp, nil
}

// trackWriter remembers a write error, so that FAM can tell a full disk from a network error.
type trackWriter struct {
	w   io.Writer
	err error
}

func (t *trackWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil {
		t.err = err
	}
	return n, err
}

// validETag: an ETag that FAM can send back (visible ASCII, at most 512 bytes).
func validETag(s string) bool {
	if s == "" || len(s) > 512 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// readETag reads the first line of a cached archive file. An invalid ETag gives "" with no
// error: FAM then downloads without a condition.
func readETag(f *os.File) (string, error) {
	var b [1]byte
	var line []byte
	for len(line) <= 512 {
		if _, err := io.ReadFull(f, b[:]); err != nil {
			return "", fmt.Errorf("the cached archive %s has no ETag line: %w", f.Name(), err)
		}
		if b[0] == '\n' {
			if !validETag(string(line)) {
				return "", nil
			}
			return string(line), nil
		}
		line = append(line, b[0])
	}
	return "", fmt.Errorf("the cached archive %s has no ETag line", f.Name())
}

// archiveMaxAge: FAM removes a cached archive that no run used for this time.
const archiveMaxAge = 30 * 24 * time.Hour

var pruneOnce sync.Once

// pruneArchives removes old archives and temporary files that a stopped run left. A run that
// still reads a removed file keeps its open file.
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
		age := time.Since(info.ModTime())
		if age > archiveMaxAge || (strings.HasPrefix(e.Name(), ".download-") && age > 24*time.Hour) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// archiveStall: a download fails when no data arrives for this time. A large archive on a slow
// line can take many minutes, so there is no limit for the whole download.
const archiveStall = time.Minute

// fetchArchive starts the download of an archive. With an ETag, it returns a nil body when
// GitHub answers 304 (not modified).
func fetchArchive(ctx context.Context, u, etag string) (*stallReader, error) {
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	if validETag(etag) {
		req.Header.Set("If-None-Match", etag)
	} else {
		etag = ""
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = archiveStall
	// No Accept-Encoding: the ETag of the archive is the same in each run.
	tr.DisableCompression = true
	resp, err := doRetry(ctx, &http.Client{Transport: tr}, req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	if resp.StatusCode == http.StatusNotModified && etag != "" {
		resp.Body.Close()
		cancel()
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("download %s: HTTP %d", u, resp.StatusCode)
	}
	newTag := resp.Header.Get("ETag")
	if !validETag(newTag) {
		newTag = ""
	}
	sr := &stallReader{body: resp.Body, cancel: cancel, etag: newTag}
	sr.last.Store(time.Now().UnixNano())
	sr.timer = time.AfterFunc(archiveStall, sr.check)
	return sr, nil
}

// stallReader cancels its download when no data arrives for archiveStall.
type stallReader struct {
	body    io.ReadCloser
	timer   *time.Timer
	cancel  context.CancelFunc
	etag    string
	last    atomic.Int64 // time of the last data, in Unix nanoseconds
	stalled atomic.Bool
	closed  atomic.Bool
}

// check runs from the timer. It cancels the download only when the last data is older than
// archiveStall; otherwise it waits for the rest of the time.
func (s *stallReader) check() {
	if s.closed.Load() {
		return
	}
	idle := time.Since(time.Unix(0, s.last.Load()))
	if idle < archiveStall {
		s.timer.Reset(archiveStall - idle)
		return
	}
	s.stalled.Store(true)
	s.cancel()
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.body.Read(p)
	if n > 0 {
		s.last.Store(time.Now().UnixNano())
	}
	if err != nil && err != io.EOF && s.stalled.Load() {
		err = fmt.Errorf("no data for %s", archiveStall)
	}
	return n, err
}

func (s *stallReader) Close() error {
	s.closed.Store(true)
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
