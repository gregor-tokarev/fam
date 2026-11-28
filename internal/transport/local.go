package transport

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Local runs commands on the cockpit.
type Local struct {
	Machine string
	// Shell is the login shell of the user. FAM uses it for LoginShell commands.
	Shell string
	// SudoPassword returns the sudo password of the cockpit. FAM gives it to `sudo -S`.
	SudoPassword func(ctx context.Context) ([]byte, error)
	Observer     Observer
	files
}

// NewLocal returns the local transport of the cockpit.
func NewLocal(machine string, sudoPassword func(ctx context.Context) ([]byte, error), obs Observer) *Local {
	l := &Local{Machine: machine, SudoPassword: sudoPassword, Observer: obs}
	l.files.run = l.Run
	return l
}

// Name implements Transport.
func (l *Local) Name() string { return l.Machine }

// Kind implements Transport.
func (l *Local) Kind() string { return "local" }

// Run implements Transport.
func (l *Local) Run(ctx context.Context, c Cmd) (Result, error) {
	if c.Sudo && len(c.Stdin) > 0 {
		return Result{}, errors.New("internal error: a sudo command cannot read stdin")
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	var cmd *exec.Cmd
	stdin := c.Stdin
	switch {
	case c.LoginShell:
		shell := l.Shell
		if shell == "" {
			shell = "/bin/sh"
		}
		// A fresh shell on the cockpit is a login shell (like a new terminal or a session that
		// an agent starts): it reads ~/.profile or ~/.zprofile, where FAM loads its environment.
		args := []string{"-l", "-c", c.Script}
		if c.Interactive {
			args = []string{"-i", "-c", c.Script}
		}
		cmd = exec.CommandContext(ctx, shell, args...)
		// A fresh shell: only the variables that a login gives.
		home, _ := os.UserHomeDir()
		user := os.Getenv("USER")
		cmd.Env = []string{
			"HOME=" + home, "USER=" + user, "LOGNAME=" + user, "SHELL=" + shell,
			"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin", "TERM=dumb",
		}
	case c.Sudo:
		if l.SudoPassword == nil {
			return Result{}, errors.New("sudo on the cockpit needs a password, and no password source is set")
		}
		pw, err := l.SudoPassword(ctx)
		if err != nil {
			return Result{}, err
		}
		if pw == nil {
			// sudo needs no password here.
			cmd = exec.CommandContext(ctx, "sudo", "-n", "sh", "-c", c.Script)
			break
		}
		// -k ignores a cached sudo timestamp, so sudo always reads the password from stdin and
		// never passes it on to the command.
		stdin = append(append([]byte{}, pw...), '\n')
		cmd = exec.CommandContext(ctx, "sudo", "-k", "-S", "-p", "", "sh", "-c", c.Script)
	default:
		cmd = exec.CommandContext(ctx, "sh", "-c", c.Script)
	}
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Duration: time.Since(start)}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			err = fmt.Errorf("run command: %w", err)
			l.observe(c, res, err)
			return res, err
		}
		res.Code = ee.ExitCode()
		if res.Code == -1 && ctx.Err() != nil {
			err = fmt.Errorf("command stopped after the timeout: %w", ctx.Err())
			l.observe(c, res, err)
			return res, err
		}
	}
	l.observe(c, res, nil)
	return res, nil
}

func (l *Local) observe(c Cmd, r Result, err error) {
	if l.Observer != nil {
		l.Observer(l.Machine, c, r, err)
	}
}

// Reconnect implements Transport. A local transport has no connection.
func (l *Local) Reconnect(context.Context) error { return nil }

// Close implements Transport.
func (l *Local) Close() error { return nil }

// MaxConcurrency implements ConcurrencyProvider.
func (l *Local) MaxConcurrency() int { return 32 }

// Stat overrides files.Stat with native Go filesystem calls when sudo is false.
func (l *Local) Stat(ctx context.Context, path string, sudo bool) (FileInfo, error) {
	if sudo {
		return l.files.Stat(ctx, path, true)
	}
	start := time.Now()
	fi, err := localStat(path)
	l.observe(Cmd{Label: "stat " + path}, Result{Duration: time.Since(start)}, err)
	return fi, err
}

// ReadFile overrides files.ReadFile with native Go filesystem calls when sudo is false.
func (l *Local) ReadFile(ctx context.Context, path string, sudo bool) ([]byte, bool, error) {
	if sudo {
		return l.files.ReadFile(ctx, path, true)
	}
	data, info, err := l.ReadFileInfo(ctx, path, false)
	return data, info.Exists, err
}

// ReadFileInfo overrides files.ReadFileInfo with native Go filesystem calls when sudo is false.
func (l *Local) ReadFileInfo(ctx context.Context, path string, sudo bool) ([]byte, FileInfo, error) {
	if sudo {
		return l.files.ReadFileInfo(ctx, path, true)
	}
	start := time.Now()
	data, info, err := localReadFileInfo(path)
	l.observe(Cmd{Label: "read " + path, Secret: true}, Result{Duration: time.Since(start)}, err)
	return data, info, err
}

// WriteFile overrides files.WriteFile with native Go atomic writes when sudo is false.
func (l *Local) WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode, sudo bool) error {
	if sudo {
		return l.files.WriteFile(ctx, path, data, mode, true)
	}
	start := time.Now()
	err := localWriteFile(path, data, mode)
	l.observe(Cmd{Label: "write " + path, Stdin: data}, Result{Duration: time.Since(start)}, err)
	return err
}

// WriteFileVerify implements VerifiedWriter natively on the local filesystem.
func (l *Local) WriteFileVerify(ctx context.Context, path string, data []byte, mode os.FileMode, sudo bool) (FileInfo, error) {
	if err := l.WriteFile(ctx, path, data, mode, sudo); err != nil {
		return FileInfo{}, err
	}
	return l.Stat(ctx, path, sudo)
}

// SkillCheck implements SkillOps natively on the local filesystem.
func (l *Local) SkillCheck(ctx context.Context, dir, marker string) (SkillState, error) {
	start := time.Now()
	st, err := localSkillCheck(dir, marker)
	l.observe(Cmd{Label: "digest of " + dir}, Result{Duration: time.Since(start)}, err)
	return st, err
}

// SkillWrite implements SkillOps natively on the local filesystem.
func (l *Local) SkillWrite(ctx context.Context, dir, marker string, tarball []byte) error {
	start := time.Now()
	err := localSkillWrite(dir, marker, tarball)
	l.observe(Cmd{Label: "copy skill " + filepath.Base(dir), Stdin: tarball}, Result{Duration: time.Since(start)}, err)
	return err
}

func isNotExistOrNotDir(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func localStat(p string) (FileInfo, error) {
	info, err := os.Lstat(p)
	if err != nil {
		if isNotExistOrNotDir(err) {
			return FileInfo{}, nil
		}
		return FileInfo{}, fmt.Errorf("stat %s: %w", p, err)
	}
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(p)
		if err != nil {
			return FileInfo{}, fmt.Errorf("stat %s: %w", p, err)
		}
		return FileInfo{Exists: true, Type: "link", Target: target}, nil
	case mode.IsDir():
		return FileInfo{Exists: true, Type: "dir", Mode: mode.Perm()}, nil
	case mode.IsRegular():
		sum, err := hashFileSHA256(p)
		if err != nil {
			return FileInfo{}, fmt.Errorf("stat %s: %w", p, err)
		}
		return FileInfo{Exists: true, Type: "file", Mode: mode.Perm(), SHA256: sum}, nil
	default:
		return FileInfo{Exists: true, Type: "other"}, nil
	}
}

func hashFileSHA256(p string) (string, error) {
	f, err := os.Open(p)
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

func localReadFileInfo(p string) ([]byte, FileInfo, error) {
	st, err := os.Stat(p)
	if err != nil {
		if isNotExistOrNotDir(err) {
			// Check for a dangling symlink.
			if _, lerr := os.Lstat(p); lerr == nil {
				return nil, FileInfo{Exists: true}, fmt.Errorf("%s exists and is not a regular file", p)
			}
			return nil, FileInfo{}, nil
		}
		return nil, FileInfo{}, fmt.Errorf("read %s: %w", p, err)
	}
	if !st.Mode().IsRegular() {
		return nil, FileInfo{Exists: true}, fmt.Errorf("%s exists and is not a regular file", p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, FileInfo{}, fmt.Errorf("read %s: %w", p, err)
	}
	fi := FileInfo{Exists: true, Type: "file", Mode: st.Mode().Perm()}
	if linfo, lerr := os.Lstat(p); lerr == nil && linfo.Mode()&os.ModeSymlink != 0 {
		fi.Type = "link"
		fi.Mode = 0
	}
	sum := sha256.Sum256(data)
	fi.SHA256 = hex.EncodeToString(sum[:])
	return data, fi, nil
}

func localWriteFile(p string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	tmp, err := os.CreateTemp(dir, ".fam-tmp.*")
	if err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := os.Chmod(tmpName, mode.Perm()); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	keep = true
	return nil
}

func localSkillCheck(dir, marker string) (SkillState, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		if isNotExistOrNotDir(err) {
			return SkillState{}, nil
		}
		return SkillState{}, fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return SkillState{Exists: true, IsDir: false}, nil
	}
	if _, err := os.Lstat(filepath.Join(dir, marker)); err != nil {
		if isNotExistOrNotDir(err) {
			return SkillState{Exists: true, IsDir: true, Managed: false}, nil
		}
		return SkillState{}, fmt.Errorf("stat %s/%s: %w", dir, marker, err)
	}
	var lines []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == marker {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		sum, err := hashFileSHA256(p)
		if err != nil {
			return err
		}
		execBit := fi.Mode().Perm()&0o100 != 0
		lines = append(lines, fmt.Sprintf("%s %s %v", sum, rel, execBit))
		return nil
	})
	if err != nil {
		return SkillState{}, fmt.Errorf("digest of %s: %w", dir, err)
	}
	sort.Strings(lines)
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return SkillState{Exists: true, IsDir: true, Managed: true, Digest: hex.EncodeToString(h[:])}, nil
}

func localSkillWrite(dir, marker string, tarball []byte) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("copy skill: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, ".fam-tmp-"+filepath.Base(dir)+".*")
	if err != nil {
		return fmt.Errorf("copy skill: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(tmp)
		}
	}()
	tr := tar.NewReader(bytes.NewReader(tarball))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("copy skill: %w", err)
		}
		cleanName := filepath.Clean(filepath.FromSlash(h.Name))
		if filepath.IsAbs(cleanName) || cleanName == ".." || strings.HasPrefix(cleanName, ".."+string(filepath.Separator)) {
			return fmt.Errorf("copy skill: invalid archive path %q", h.Name)
		}
		dst := filepath.Join(tmp, cleanName)
		rel, rerr := filepath.Rel(tmp, dst)
		if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("copy skill: archive path %q escapes target directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("copy skill: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return fmt.Errorf("copy skill: %w", err)
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return fmt.Errorf("copy skill: %w", err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("copy skill: %w", err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("copy skill: %w", err)
			}
		}
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("copy skill: %w", err)
	}
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		if ms, err := os.Stat(filepath.Join(dir, marker)); err != nil || !ms.Mode().IsRegular() {
			return fmt.Errorf("copy skill: %s exists and FAM did not make it", dir)
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("copy skill: %w", err)
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("copy skill: %w", err)
	}
	keep = true
	return nil
}
