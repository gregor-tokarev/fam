package infisical

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gregor-tokarev/fam/internal/secrets"
)

// Cache keeps the secrets and the access tokens that FAM read from Infisical, in files with mode
// 0600 in a directory with mode 0700.
type Cache struct {
	Dir string
}

// DefaultCacheDir is <user cache dir>/fam/infisical.
func DefaultCacheDir() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "fam", "infisical"), nil
}

type cachedSecret struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type secretsEntry struct {
	Version   int            `json:"version"`
	FetchedAt time.Time      `json:"fetchedAt"`
	Secrets   []cachedSecret `json:"secrets"`
}

const entryVersion = 1

func cacheKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// ReadSecrets returns the cached secrets of key and when FAM read them.
func (c Cache) ReadSecrets(key string) ([]secrets.Secret, time.Time, bool) {
	var e secretsEntry
	if !c.read("secrets-"+key, &e) || e.Version != entryVersion {
		return nil, time.Time{}, false
	}
	list := make([]secrets.Secret, 0, len(e.Secrets))
	for _, s := range e.Secrets {
		// The cache goes into a shell file: apply the rules of an answer of Infisical again.
		if !secrets.ValidName(s.Name) || !secrets.SafeValue(s.Value) {
			return nil, time.Time{}, false
		}
		list = append(list, secrets.Secret{Name: s.Name, Value: s.Value})
	}
	return list, e.FetchedAt, true
}

// WriteSecrets saves the secrets of key that FAM started to read at the time at. It keeps an entry
// that another FAM process read later, so that an old answer does not replace a newer one.
func (c Cache) WriteSecrets(key string, list []secrets.Secret, at time.Time) error {
	unlock, err := c.lock("secrets-" + key)
	if err != nil {
		return err
	}
	defer unlock()
	if _, newer, ok := c.ReadSecrets(key); ok && newer.After(at) {
		return nil
	}
	e := secretsEntry{Version: entryVersion, FetchedAt: at.UTC(), Secrets: make([]cachedSecret, 0, len(list))}
	for _, s := range list {
		e.Secrets = append(e.Secrets, cachedSecret{Name: s.Name, Value: s.Value})
	}
	return c.write("secrets-"+key, e)
}

// ReadToken returns the cached access token of key.
func (c Cache) ReadToken(key string) (Token, bool) {
	var t Token
	return t, c.read("token-"+key, &t) && t.AccessToken != ""
}

// WriteToken saves the access token of key.
func (c Cache) WriteToken(key string, t Token) error {
	unlock, err := c.lock("token-" + key)
	if err != nil {
		return err
	}
	defer unlock()
	return c.write("token-"+key, t)
}

// DropToken removes the access token of key after Infisical rejected it, but only when the file
// still has that token: another FAM process can have saved a new one.
func (c Cache) DropToken(key, rejected string) {
	unlock, err := c.lock("token-" + key)
	if err != nil {
		return
	}
	defer unlock()
	if t, ok := c.ReadToken(key); ok && t.AccessToken == rejected {
		_ = os.Remove(filepath.Join(c.Dir, "token-"+key+".json"))
	}
}

// ownDir reports whether the cache directory is a real directory of this user that other users
// cannot open.
func (c Cache) ownDir() bool {
	st, err := os.Lstat(c.Dir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0o077 != 0 {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Getuid()
}

// lock takes an exclusive lock between FAM processes for one cache file. It returns the unlock.
func (c Cache) lock(name string) (func(), error) {
	if err := c.prepare(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(c.Dir, ".lock-"+name), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func (c Cache) read(name string, v any) bool {
	if !c.ownDir() {
		return false
	}
	p := filepath.Join(c.Dir, name+".json")
	// O_NONBLOCK: a FIFO in place of a cache file must not stop FAM. The checks below reject it.
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	// Use only a regular file of this user that other users cannot read or change. FAM made such a
	// file; any other file is not FAM's.
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return false
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(f, 32<<20))
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

// prepare makes the cache directory with mode 0700, and stops when it is not a directory of
// this user.
func (c Cache) prepare() error {
	if c.Dir == "" {
		return errors.New("no cache directory")
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return err
	}
	if st, err := os.Lstat(c.Dir); err != nil || !st.IsDir() {
		return fmt.Errorf("%s is not a directory", c.Dir)
	} else if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to another user", c.Dir)
	}
	return os.Chmod(c.Dir, 0o700)
}

// write replaces a cache file. The caller holds the lock of the file.
func (c Cache) write(name string, v any) error {
	if err := c.prepare(); err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(c.Dir, "."+name+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(c.Dir, name+".json")); err != nil {
		return fmt.Errorf("save the Infisical cache: %w", err)
	}
	return nil
}
