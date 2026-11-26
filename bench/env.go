package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// sandbox holds an isolated HOME, project, caches, and working directories for one fam binary.
type sandbox struct {
	name       string
	bin        string
	root       string
	home       string
	project    string
	work       string
	sshConfig  string
	pklPath    string
	relPklPath string // path of pkl binary relative to home
	projectErr string // non-empty when fixture project setup failed
	steadyErr  string // non-empty when initial steady-state apply failed
}

func newSandbox(ctx context.Context, root, name, bin string, pklSeedDirs []string) (*sandbox, error) {
	sb := &sandbox{
		name:      name,
		bin:       bin,
		root:      filepath.Join(root, name),
		home:      filepath.Join(root, name, "home"),
		project:   filepath.Join(root, name, "project"),
		work:      filepath.Join(root, name, "work"),
		sshConfig: filepath.Join(root, name, "ssh_config"),
	}
	for _, d := range []string{sb.home, sb.work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// Safety belt: block any accidental SSH connection immediately.
	if err := os.WriteFile(sb.sshConfig, []byte("Host *\n  HostName 127.0.0.1\n  Port 9\n  ConnectTimeout 2\n  BatchMode yes\n"), 0o644); err != nil {
		return nil, err
	}

	// Pre-seed cached pkl binaries from the host (or previous sandbox) to avoid redundant downloads.
	// Always copy (never hardlink) so sandboxes cannot mutate each other or the host cache.
	dstPklRoot := filepath.Join(userCacheDir(sb.home), "fam", "pkl")
	for _, src := range pklSeedDirs {
		if src == "" {
			continue
		}
		if st, err := os.Stat(src); err == nil && st.IsDir() {
			_ = copyTree(src, dstPklRoot, false)
		}
	}

	// Ensure the pinned pkl binary for this fam binary exists in the sandbox cache.
	out, err := sb.run(ctx, sb.home, sb.root, "internal", "pkl-path")
	if err != nil {
		return nil, fmt.Errorf("%s: fam internal pkl-path: %w\n%s", name, err, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	sb.pklPath = strings.TrimSpace(lines[len(lines)-1])
	rel, err := filepath.Rel(sb.home, sb.pklPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("%s: pkl path %q is outside sandbox home %q", name, sb.pklPath, sb.home)
	}
	sb.relPklPath = rel

	// Bootstrap the benchmark project with this binary's own schema & pin.
	// Setup failures are recorded on the sandbox so unaffected commands still run and report.
	if out, err := sb.run(ctx, sb.home, sb.root, "new", "--base", sb.project); err != nil {
		sb.projectErr = "setup fam new: " + compactErr(err, out)
		return sb, nil
	}
	// Remove unused starter bundles from `fam new` before writing the benchmark fixture.
	for _, rm := range []string{"bundles/hello.pkl", "bundles/files", "bundles/worker-setup.pkl"} {
		_ = os.RemoveAll(filepath.Join(sb.project, rm))
	}
	if err := writeFixture(sb.project, sb.home); err != nil {
		return nil, fmt.Errorf("%s: write fixture: %w", name, err)
	}
	// Converge sb.home once so plan, apply (steady), apply --update, and verify run against an applied cockpit.
	if out, err := sb.run(ctx, sb.home, sb.project, "apply", "--no-progress", "--base", sb.project); err != nil {
		sb.steadyErr = "setup fam apply: " + compactErr(err, out)
	}
	return sb, nil
}

// pklRoot returns the directory under sb.home containing cached pkl versions.
func (sb *sandbox) pklRoot() string {
	return filepath.Join(userCacheDir(sb.home), "fam", "pkl")
}

// envFor builds a sanitized environment pointing HOME and XDG paths at home.
func (sb *sandbox) envFor(home string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "FAM_SSH_CONFIG", "TERM":
			continue
		}
		if strings.HasPrefix(k, "INFISICAL_") {
			continue
		}
		out = append(out, kv)
	}
	return append(out,
		"HOME="+home,
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"FAM_SSH_CONFIG="+sb.sshConfig,
		"TERM=dumb",
	)
}

func (sb *sandbox) run(ctx context.Context, home, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, sb.bin, args...)
	cmd.Dir = dir
	cmd.Env = sb.envFor(home)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (sb *sandbox) timeRun(ctx context.Context, home, dir string, args ...string) (time.Duration, string, error) {
	cmd := exec.CommandContext(ctx, sb.bin, args...)
	cmd.Dir = dir
	cmd.Env = sb.envFor(home)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	dur := time.Since(start)
	return dur, string(out), err
}

// prepareFreshHome creates a clean HOME directory with only the pkl binary cache,
// seeded fam Pkl package caches, and the secrets source file, ready for a fresh `fam apply`.
func (sb *sandbox) prepareFreshHome(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Hardlink (or copy) the pinned pkl binary and its sha256 stamp.
	dstPkl := filepath.Join(dir, sb.relPklPath)
	if err := linkOrCopyFile(sb.pklPath, dstPkl); err != nil {
		return err
	}
	if _, err := os.Stat(sb.pklPath + ".sha256"); err == nil {
		if err := linkOrCopyFile(sb.pklPath+".sha256", dstPkl+".sha256"); err != nil {
			return err
		}
	}
	// Seed Pkl package caches so fresh apply does not pay package seeding noise.
	for _, rel := range []string{filepath.Join(".cache", "pkl"), filepath.Join(".pkl", "cache")} {
		src := filepath.Join(sb.home, rel)
		if st, err := os.Stat(src); err == nil && st.IsDir() {
			if err := copyTree(src, filepath.Join(dir, rel), true); err != nil {
				return err
			}
		}
	}
	// Copy secrets source file with strict 0700 / 0600 permissions.
	secDst := filepath.Join(dir, ".secrets", "bench.env")
	if err := os.MkdirAll(filepath.Dir(secDst), 0o700); err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(sb.home, ".secrets", "bench.env"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(secDst, b, 0o600); err != nil {
		return err
	}
	return os.Chmod(secDst, 0o600)
}

func userCacheDir(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Caches")
	}
	return filepath.Join(home, ".cache")
}

func linkOrCopyFile(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyTree(src, dst string, hardlink bool) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if _, err := os.Lstat(target); err == nil {
				return nil
			}
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if hardlink {
			return linkOrCopyFile(p, target)
		}
		if _, err := os.Stat(target); err == nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}

func buildFromDir(ctx context.Context, srcDir, outBin string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", outBin, "./cmd/fam")
	cmd.Dir = srcDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build in %s: %w\n%s", srcDir, err, string(out))
	}
	return nil
}

func buildFromRef(ctx context.Context, repoDir, ref, worktreeDir, outBin string) (string, error) {
	shaCmd := exec.CommandContext(ctx, "git", "rev-parse", "--short", ref)
	shaCmd.Dir = repoDir
	shaOut, err := shaCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w\n%s", ref, err, string(shaOut))
	}
	sha := strings.TrimSpace(string(shaOut))

	add := exec.CommandContext(ctx, "git", "worktree", "add", "--detach", worktreeDir, ref)
	add.Dir = repoDir
	if out, err := add.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add %s: %w\n%s", ref, err, string(out))
	}
	defer func() {
		rm := exec.Command("git", "worktree", "remove", "--force", worktreeDir)
		rm.Dir = repoDir
		_ = rm.Run()
	}()
	if err := buildFromDir(ctx, worktreeDir, outBin); err != nil {
		return "", err
	}
	return sha, nil
}
