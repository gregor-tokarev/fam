package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
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
		// A release tag or a commit does not change, so FAM keeps its archive on disk.
		keep := commitRef.MatchString(ref)
		if r.ref == "" {
			r.ref, r.err = latestReleaseOnce(ctx, repo)
			if r.err != nil {
				return
			}
			keep = true
		}
		r.files, r.err = downloadSkill(ctx, repo, folder, r.ref, keep)
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

// commitRef matches a full commit hash.
var commitRef = regexp.MustCompile(`^[0-9a-f]{40}$`)

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

// downloadSkill reads the files of one folder from the archive of a repository at a ref. When
// keep is true, the ref does not change, and FAM keeps the archive in its cache folder, so that
// the next run does not download it again.
func downloadSkill(ctx context.Context, repo, folder, ref string, keep bool) ([]skillFile, error) {
	segs := strings.Split(ref, "/")
	for i, sg := range segs {
		segs[i] = url.PathEscape(sg)
	}
	u := CodeloadBase + "/" + repo + "/tar.gz/" + strings.Join(segs, "/")
	if keep {
		if files, err := cachedSkill(ctx, repo, folder, ref, u); err == nil || !errors.Is(err, errNoCache) {
			return files, err
		}
	}
	body, err := fetchArchive(ctx, u)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return readSkill(body, repo, folder, ref, u)
}

// errNoCache: the cache folder is not usable, so FAM downloads the archive without it.
var errNoCache = errors.New("no cache folder")

// archiveError is an error of the archive data (gzip or tar), not of the skill folder in it.
type archiveError struct{ err error }

// badArchive marks an error of the archive data. A too large archive is not damaged: a second
// download does not help.
func badArchive(u string, err error) error {
	err = fmt.Errorf("read the archive %s: %w", u, err)
	if errors.Is(err, errArchiveTooLarge) {
		return err
	}
	return &archiveError{err}
}

func (e *archiveError) Error() string { return e.err.Error() }
func (e *archiveError) Unwrap() error { return e.err }

// githubArchives holds the archive downloads of this run, so that the skills of one repository
// share one download.
var githubArchives = struct {
	sync.Mutex
	done map[string]*archiveDownload
}{done: map[string]*archiveDownload{}}

type archiveDownload struct {
	once sync.Once
	err  error
}

// cachedSkill reads a skill folder from the cached archive of a repository at a ref, and
// downloads the archive first when the cache does not have it.
func cachedSkill(ctx context.Context, repo, folder, ref, u string) ([]skillFile, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, errNoCache
	}
	dir = filepath.Join(dir, "fam", "github", filepath.FromSlash(repo))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errNoCache
	}
	file := filepath.Join(dir, url.PathEscape(ref)+".tar.gz")
	for attempt := 0; ; attempt++ {
		if err := ensureArchive(ctx, file, u); err != nil {
			return nil, err
		}
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		_ = os.Chtimes(file, now, now)
		files, err := readSkill(f, repo, folder, ref, u)
		f.Close()
		var ae *archiveError
		if err != nil && errors.As(err, &ae) && attempt == 0 {
			// A damaged file in the cache: remove it and download it again.
			forgetArchive(file)
			continue
		}
		return files, err
	}
}

// ensureArchive downloads an archive into file when file does not exist. A run downloads each
// archive once.
func ensureArchive(ctx context.Context, file, u string) error {
	githubArchives.Lock()
	d, ok := githubArchives.done[file]
	if !ok {
		d = &archiveDownload{}
		githubArchives.done[file] = d
	}
	githubArchives.Unlock()
	d.once.Do(func() {
		if _, err := os.Stat(file); err == nil {
			return
		}
		d.err = saveArchive(ctx, file, u)
		if d.err == nil {
			pruneArchives(filepath.Dir(file))
		}
	})
	if d.err != nil {
		githubArchives.Lock()
		if githubArchives.done[file] == d {
			delete(githubArchives.done, file)
		}
		githubArchives.Unlock()
	}
	return d.err
}

// forgetArchive removes a cached archive, so that the next ensureArchive downloads it again.
func forgetArchive(file string) {
	githubArchives.Lock()
	delete(githubArchives.done, file)
	githubArchives.Unlock()
	_ = os.Remove(file)
}

// saveArchive downloads an archive into a temporary file next to file, then renames it.
func saveArchive(ctx context.Context, file, u string) error {
	body, err := fetchArchive(ctx, u)
	if err != nil {
		return err
	}
	defer body.Close()
	tmp, err := os.CreateTemp(filepath.Dir(file), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, &capReader{r: body, left: maxArchive}); err != nil {
		tmp.Close()
		return fmt.Errorf("download %s: %w", u, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// archiveMaxAge: FAM removes a cached archive that no run used for this time.
const archiveMaxAge = 30 * 24 * time.Hour

// pruneArchives removes the old archives of one repository from the cache.
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

// fetchArchive starts the download of an archive.
func fetchArchive(ctx context.Context, u string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := doRetry(ctx, &http.Client{Timeout: 5 * time.Minute}, req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download %s: HTTP %d", u, resp.StatusCode)
	}
	return resp.Body, nil
}

// readSkill reads the files of one folder from a .tar.gz archive of a repository.
func readSkill(archive io.Reader, repo, folder, ref, u string) ([]skillFile, error) {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return nil, badArchive(u, err)
	}
	fail := func(format string, a ...any) ([]skillFile, error) {
		return nil, fmt.Errorf("skill %s of %s@%s: %s", folder, repo, ref, fmt.Sprintf(format, a...))
	}
	prefix := strings.Trim(folder, "/") + "/"
	var files []skillFile
	byRel := map[string]int{}
	links := map[string]string{} // rel path in the folder -> link target in the repository
	total := 0
	root := ""
	tr := tar.NewReader(&capReader{r: gz, left: maxArchive})
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
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
