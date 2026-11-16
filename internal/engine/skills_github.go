package engine

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	if resp.StatusCode < 300 || resp.StatusCode >= 400 || path.Base(path.Dir(loc)) != "tag" {
		return "", fmt.Errorf("%s has no release (HTTP %d from %s). Set `ref` of the skill to a tag, branch, or commit", repo, resp.StatusCode, u)
	}
	return path.Base(loc), nil
}

// downloadSkill reads the files of one folder from the archive of a repository at a ref.
func downloadSkill(ctx context.Context, repo, folder, ref string) ([]skillFile, error) {
	u := CodeloadBase + "/" + repo + "/tar.gz/" + ref
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
	gz, err := gzip.NewReader(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	prefix := strings.Trim(folder, "/") + "/"
	var files []skillFile
	links := map[string]string{} // rel path in the folder -> link target in the repository
	all := map[string]skillFile{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", u, err)
		}
		// The archive has one top folder (<name>-<ref>/).
		_, inRepo, ok := strings.Cut(h.Name, "/")
		if !ok || inRepo == "" {
			continue
		}
		inRepo = path.Clean(inRepo)
		switch h.Typeflag {
		case tar.TypeReg:
			if !strings.HasPrefix(inRepo, prefix) {
				// Keep small files outside the folder for links that point out of it.
				if h.Size > 1<<20 {
					continue
				}
			}
			data, err := io.ReadAll(io.LimitReader(tr, 64<<20))
			if err != nil {
				return nil, fmt.Errorf("download %s: %w", u, err)
			}
			f := skillFile{rel: strings.TrimPrefix(inRepo, prefix), data: data, exec: h.Mode&0o100 != 0}
			all[inRepo] = f
			if strings.HasPrefix(inRepo, prefix) && f.rel != managedMarker {
				files = append(files, f)
			}
		case tar.TypeSymlink:
			if strings.HasPrefix(inRepo, prefix) {
				links[strings.TrimPrefix(inRepo, prefix)] = path.Clean(path.Join(path.Dir(inRepo), h.Linkname))
			}
		}
	}
	// A link in the folder becomes a copy of the file that it points to (as for local skills).
	for rel, target := range links {
		f, ok := all[target]
		if !ok || strings.HasPrefix(target, "../") {
			return nil, fmt.Errorf("skill %s of %s@%s: the link %s does not point to a file in the repository", folder, repo, ref, rel)
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
		return nil, fmt.Errorf("%s@%s has no %sSKILL.md", repo, ref, prefix)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}
