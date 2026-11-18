package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/gregor-tokarev/fam/internal/pklgen/tasks"
)

// Kinds of skill sources.
const (
	skillLocal   = "local"
	skillGitHub  = "github"
	skillArchive = "archive"
	skillMDFile  = "file"
)

// skillSpec is the parsed source of an AgentSkill task.
type skillSpec struct {
	kind   string
	dir    string // local: the folder on the cockpit
	repo   string // github: owner/name
	folder string // github, archive: the folder in the repository or archive ("" for the top)
	ref    string // github: "" for the latest release
	url    string // archive, file
	zip    bool   // archive: a .zip archive (else .tar.gz)
	name   string // the default name
}

// archiveExt returns the extension of an archive URL path, or "".
func archiveExt(p string) string {
	l := strings.ToLower(p)
	for _, ext := range []string{".tar.gz", ".tgz", ".zip"} {
		if strings.HasSuffix(l, ext) {
			return ext
		}
	}
	return ""
}

// parseSkillSource reads the source, path, and ref of an AgentSkill task. bundleDir is the folder
// of the bundle file ("" for a bundle of a package), bundleName the name of the bundle.
func parseSkillSource(x tasks.AgentSkill, bundleDir, bundleName string) (skillSpec, error) {
	source := strings.TrimSpace(x.GetSource())
	folder := ""
	if p := x.GetPath(); p != nil {
		folder = strings.Trim(path.Clean(*p), "/")
		if folder == "." {
			folder = ""
		}
	}
	ref := ""
	if r := x.GetRef(); r != nil {
		ref = *r
	}
	lower := strings.ToLower(source)
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		if strings.Contains(source, "://") {
			return skillSpec{}, fmt.Errorf("the skill source %q is not a folder or an https URL", source)
		}
		if ref != "" {
			return skillSpec{}, fmt.Errorf("the skill source %q is a folder: `ref` is only for a GitHub repository", source)
		}
		dir := source
		if !filepath.IsAbs(dir) {
			if bundleDir == "" {
				return skillSpec{}, fmt.Errorf("the bundle comes from a package, so it has no folder for the skill %q. Use a URL or an absolute path", source)
			}
			dir = filepath.Join(bundleDir, dir)
		}
		dir = filepath.Join(dir, filepath.FromSlash(folder))
		name := filepath.Base(dir)
		if name == "skill" || name == "." {
			name = bundleName
		}
		return skillSpec{kind: skillLocal, dir: dir, name: name}, nil
	}
	u, err := url.Parse(source)
	if err != nil || u.Host == "" {
		return skillSpec{}, fmt.Errorf("the skill source %q is not a valid URL", source)
	}
	if u.Scheme != "https" {
		return skillSpec{}, fmt.Errorf("the skill source %q must use https", source)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	host := strings.ToLower(u.Host)
	if (host == "github.com" || host == "www.github.com") && u.RawQuery == "" && len(segs) >= 2 && (len(segs) == 2 || segs[2] == "tree" || segs[2] == "blob") {
		repo := segs[0] + "/" + strings.TrimSuffix(segs[1], ".git")
		switch {
		case len(segs) == 2:
			s := skillSpec{kind: skillGitHub, repo: repo, folder: folder, ref: ref, name: path.Base(repo)}
			if folder != "" {
				s.name = path.Base(folder)
			}
			return s, nil
		case len(segs) >= 4 && segs[2] == "tree":
			if x.GetPath() != nil || ref != "" {
				return skillSpec{}, fmt.Errorf("the skill source %q has the ref and the folder in its URL: remove `path` and `ref`, or use https://github.com/%s", source, repo)
			}
			s := skillSpec{kind: skillGitHub, repo: repo, ref: segs[3], folder: strings.Join(segs[4:], "/"), name: path.Base(repo)}
			if s.folder != "" {
				s.name = path.Base(s.folder)
			}
			return s, nil
		case len(segs) >= 5 && segs[2] == "blob" && strings.HasSuffix(strings.ToLower(u.Path), ".md"):
			// A file page of GitHub: get the raw file.
			raw := "https://raw.githubusercontent.com/" + strings.Join(append([]string{segs[0], segs[1]}, segs[3:]...), "/")
			u, _ = url.Parse(raw)
		default:
			return skillSpec{}, fmt.Errorf("the skill source %q is not a GitHub repository, folder (/tree/<ref>/<folder>), or .md file", source)
		}
	}
	if ref != "" {
		return skillSpec{}, fmt.Errorf("the skill source %q is not a GitHub repository: `ref` is only for a GitHub repository", source)
	}
	if ext := archiveExt(u.Path); ext != "" {
		s := skillSpec{kind: skillArchive, url: u.String(), folder: folder, zip: ext == ".zip"}
		if folder != "" {
			s.name = path.Base(folder)
		} else {
			s.name = path.Base(u.Path)
			s.name = s.name[:len(s.name)-len(ext)]
		}
		return s, nil
	}
	if strings.HasSuffix(strings.ToLower(u.Path), ".md") {
		if x.GetPath() != nil {
			return skillSpec{}, fmt.Errorf("the skill source %q is one file: remove `path`", source)
		}
		base := path.Base(u.Path)
		name := strings.TrimSuffix(base, path.Ext(base))
		if strings.EqualFold(base, "SKILL.md") {
			name = path.Base(path.Dir(u.Path))
		}
		return skillSpec{kind: skillMDFile, url: u.String(), name: name}, nil
	}
	return skillSpec{}, fmt.Errorf("FAM cannot tell the type of the skill source %q. Use a GitHub repository, a .tar.gz, .tgz, or .zip archive, or a .md file", source)
}

// String is the source in output.
func (s skillSpec) String() string {
	switch s.kind {
	case skillLocal:
		return s.dir
	case skillGitHub:
		out := "github.com/" + s.repo
		if s.folder != "" {
			out += "/" + s.folder
		}
		if s.ref != "" {
			out += "@" + s.ref
		}
		return out
	case skillArchive:
		if s.folder != "" {
			return s.url + "#" + s.folder
		}
	}
	return s.url
}

// load reads or downloads the skill.
func (s skillSpec) load(ctx context.Context, name, origin string) (*SkillSource, error) {
	switch s.kind {
	case skillLocal:
		return loadSkill(name, s.dir, origin)
	case skillGitHub:
		return loadGitHubSkill(ctx, s.repo, s.folder, s.ref, name, origin)
	}
	key := s.kind + "\x00" + s.url + "\x00" + s.folder
	_, files, err := remoteSkillOnce(ctx, key, func() (string, []skillFile, error) {
		if s.kind == skillMDFile {
			files, err := downloadCached(ctx, "url:"+s.url, "", s.url, maxSkillFile, func(r io.Reader, _ *io.SectionReader) ([]skillFile, error) {
				tooLarge := fmt.Errorf("%s has more than %d bytes", s.url, maxSkillFile)
				data, err := io.ReadAll(&capReader{r: r, left: maxSkillFile, limitError: tooLarge})
				if err != nil {
					return nil, err
				}
				return []skillFile{{rel: "SKILL.md", data: data}}, nil
			})
			return s.url, files, err
		}
		files, err := downloadCached(ctx, "url:"+s.url, "", s.url, maxArchive, func(r io.Reader, sec *io.SectionReader) ([]skillFile, error) {
			if !s.zip {
				return readSkill(tarEntries(r, s.url), s.folder, s.url, false)
			}
			if sec == nil {
				// No cache file: a zip archive needs random access, so read it into memory.
				data, err := io.ReadAll(r)
				if err != nil {
					return nil, fmt.Errorf("download %s: %w", s.url, err)
				}
				sec = io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data)))
			}
			return readSkill(zipEntries(sec, sec.Size(), s.url), s.folder, s.url, false)
		})
		return s.url, files, err
	})
	if err != nil {
		return nil, fmt.Errorf("skill %s (%s): %w", name, origin, err)
	}
	return &SkillSource{Name: name, Dir: s.String(), Origin: origin, files: files}, nil
}

// AgentSkillName returns the name of the skill of an AgentSkill task in a bundle of a package,
// without reading or downloading it.
func AgentSkillName(x tasks.AgentSkill, bundleName string) string {
	if n := x.GetName(); n != nil {
		return *n
	}
	spec, err := parseSkillSource(x, "", bundleName)
	if err != nil {
		return ""
	}
	return spec.name
}
