package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gregor-tokarev/fam/internal/pklgen/bundle"
)

// GitHubBase and CodeloadBase are the GitHub hosts for skill downloads.
var (
	GitHubBase   = "https://github.com"
	CodeloadBase = "https://codeload.github.com"
)

// githubSkills holds the skills that this run downloaded, so that all machines share one
// download for each repository, folder, and ref.
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

// loadGitHubSkill downloads a skill folder from a GitHub repository.
func loadGitHubSkill(ctx context.Context, gs bundle.GitHubSkill, origin string) (*SkillSource, error) {
	ref := ""
	if gs.Ref != nil {
		ref = *gs.Ref
	}
	key := gs.Repo + "\x00" + gs.Path + "\x00" + ref
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
			r.ref, r.err = latestRelease(ctx, gs.Repo)
			if r.err != nil {
				return
			}
		}
		r.files, r.err = downloadSkill(ctx, gs.Repo, gs.Path, r.ref)
	})
	if r.err != nil && ctx.Err() != nil {
		// Do not keep a canceled download: a later call tries again.
		githubSkills.Lock()
		if githubSkills.done[key] == r {
			delete(githubSkills.done, key)
		}
		githubSkills.Unlock()
	}
	name := path.Base(gs.Path)
	src := fmt.Sprintf("github.com/%s/%s@%s", gs.Repo, gs.Path, r.ref)
	if r.err != nil {
		return nil, fmt.Errorf("skill %s (%s): %w", name, origin, r.err)
	}
	return &SkillSource{Name: name, Dir: src, Origin: origin, files: r.files}, nil
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
	resp, err := c.Do(req)
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

// downloadSkill reads the files of one folder from the archive of a repository at a ref.
func downloadSkill(ctx context.Context, repo, folder, ref string) ([]skillFile, error) {
	segs := strings.Split(ref, "/")
	for i, sg := range segs {
		segs[i] = url.PathEscape(sg)
	}
	u := CodeloadBase + "/" + repo + "/tar.gz/" + strings.Join(segs, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", u, resp.StatusCode)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u, err)
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
			return nil, fmt.Errorf("download %s: %w", u, err)
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
				return nil, fmt.Errorf("download %s: %w", u, err)
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

// capReader stops with an error after `left` bytes.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, fmt.Errorf("the archive is larger than %d bytes", int64(maxArchive))
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}
