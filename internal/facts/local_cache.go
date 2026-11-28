package facts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/gregor-tokarev/fam/internal/transport"
)

const localFactsCacheFile = "local-facts-v1.json"

type localFactsEntry struct {
	Fingerprint string `json:"fingerprint"`
	Facts       Facts  `json:"facts"`
}

// localEnvFingerprint computes a fast (~0.08ms) fingerprint of every kernel/OS/process input that
// facts.Script() inspects on the local cockpit, including CPU affinity (taskset), HOME, USER,
// SHELL, hostname, /etc/os-release, /run/systemd/system, and bash binary stat.
func localEnvFingerprint() (string, bool) {
	if os.Getenv("FAM_NO_FACTS_CACHE") == "1" {
		return "", false
	}
	h := sha256.New()
	scriptSum := sha256.Sum256([]byte(script))
	h.Write([]byte("v2\x00" + runtime.GOOS + "\x00" + runtime.GOARCH + "\x00"))
	h.Write(scriptSum[:])
	h.Write([]byte{0})
	if exe, err := os.Executable(); err == nil {
		h.Write([]byte(exe))
		h.Write([]byte{0})
		if ei, err := os.Stat(exe); err == nil {
			h.Write([]byte(ei.ModTime().UTC().String()))
			h.Write([]byte{0})
		}
	}
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	h.Write([]byte(host + "\x00" + home + "\x00" + os.Getenv("USER") + "\x00" + os.Getenv("SHELL") + "\x00" + os.Getenv("PATH") + "\x00"))

	statFile := func(p string) {
		info, err := os.Stat(p)
		if err != nil {
			h.Write([]byte(p + ":none\x00"))
			return
		}
		var ino uint64
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			ino = st.Ino
		}
		h.Write([]byte(p + ":"))
		h.Write([]byte(info.ModTime().UTC().String()))
		h.Write([]byte{0})
		var buf [16]byte
		sz := info.Size()
		for i := 0; i < 8; i++ {
			buf[i] = byte(sz >> (i * 8))
			buf[8+i] = byte(ino >> (i * 8))
		}
		h.Write(buf[:])
	}

	statFile("/etc/os-release")
	statFile("/etc/passwd")
	statFile("/run/systemd/system")
	if bashPath, err := exec.LookPath("bash"); err == nil {
		statFile(bashPath)
	} else {
		h.Write([]byte("bash:none\x00"))
	}
	// On Linux, include Cpus_allowed_list from /proc/self/status so taskset / cgroup CPU affinity
	// changes immediately invalidate the cached CPU count.
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/self/status")
		if err != nil {
			return "", false
		}
		for _, line := range bytes.Split(b, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("Cpus_allowed_list:")) {
				h.Write(line)
				h.Write([]byte{0})
				break
			}
		}
		if online, err := os.ReadFile("/sys/devices/system/cpu/online"); err == nil {
			h.Write(online)
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// CollectLocalCached collects local cockpit facts via facts.Collect, caching the result on disk
// keyed by a fast fingerprint of all local inputs (including CPU affinity, shell, and bash stat).
func CollectLocalCached(ctx context.Context, t transport.Transport, machine string, cockpit bool, tags []string) (Facts, error) {
	fp, ok := localEnvFingerprint()
	dir := factsCacheDir()
	cachePath := ""
	if ok && dir != "" {
		cachePath = filepath.Join(dir, localFactsCacheFile)
		if b, err := os.ReadFile(cachePath); err == nil {
			var e localFactsEntry
			if json.Unmarshal(b, &e) == nil && e.Fingerprint == fp && e.Facts.OS != "" {
				f := e.Facts
				f.Machine = machine
				f.Cockpit = cockpit
				f.Tags = append([]string{}, tags...)
				return f, nil
			}
		}
	}
	f, err := Collect(ctx, t, machine, cockpit, tags)
	if err != nil {
		return Facts{}, err
	}
	if cachePath != "" {
		if b, jerr := json.Marshal(localFactsEntry{Fingerprint: fp, Facts: f}); jerr == nil {
			if os.MkdirAll(dir, 0o700) == nil {
				if tmp, cerr := os.CreateTemp(dir, ".tmp-*"); cerr == nil {
					tmpName := tmp.Name()
					_, werr := tmp.Write(b)
					cerr2 := tmp.Close()
					if werr == nil && cerr2 == nil {
						_ = os.Chmod(tmpName, 0o600)
						if os.Rename(tmpName, cachePath) != nil {
							_ = os.Remove(tmpName)
						}
					} else {
						_ = os.Remove(tmpName)
					}
				}
			}
		}
	}
	return f, nil
}
