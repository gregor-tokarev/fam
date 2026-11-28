# Embedded persistent SSH worker for FAM (Python 3.6+).
#
# Architecture rule: this worker is strictly a dumb remote primitive helper
# (stat, read, write, symlink, skill_check, skill_write, exec, batch, warm_bootstrap).
# All FAM task semantics, conflict rules, block merging, and user-facing error text
# live exclusively in Go (internal/engine/).
#
# Cross-references for the two protocol formats that must agree with Go:
#   - skill digest format (`<sha256> <relpath> <true|false>` sorted + sha256):
#     internal/engine/skills.go (SkillSource.digest / remoteDigest) and internal/transport/local.go (localSkillCheck).
#   - facts raw digest (sha256 of stdout with trailing CR/LF stripped):
#     internal/transport/ssh_bootstrap.go (RawFactsSHA) and internal/facts/cache.go (RawHash).

import base64
import concurrent.futures
import contextlib
try:
    import fcntl
except ImportError:
    fcntl = None
import hashlib
import io
import json
import os
import re
import shutil
import signal
import stat
import subprocess
import sys
import tarfile
import tempfile
import threading
import time

FACTS_RAW = os.environ.pop("FAM_FACTS_RAW", "")
FACTS_SHA = hashlib.sha256(FACTS_RAW.rstrip("\r\n").encode("utf-8")).hexdigest() if FACTS_RAW else ""
SPEC_B64 = os.environ.pop("FAM_SPEC_B64", "")


def _collect_env_watch_paths():
    home = os.path.expanduser("~")
    paths = [
        os.path.join(home, ".bashrc"),
        os.path.join(home, ".bash_profile"),
        os.path.join(home, ".bash_login"),
        os.path.join(home, ".profile"),
        os.path.join(home, ".zshenv"),
        os.path.join(home, ".zshrc"),
        os.path.join(home, ".config", "fam"),
        os.path.join(home, ".config", "fam", "env.sh"),
        os.path.join(home, ".config", "fam", "secrets.env"),
        "/etc/environment",
        "/etc/profile",
        "/etc/profile.d",
    ]
    # Also watch files explicitly sourced by startup scripts (e.g. ~/.bashrc sourcing ~/.my_env).
    extra = set()
    src_re = re.compile(
        r'(?:^|[;&|])[ \t]*(?:\.|source)[ \t]+["\']?((?:~|\$HOME|\$\{HOME\}|/)[^"\'\s;&|]+)',
        re.MULTILINE,
    )
    for rc in paths[:6]:
        try:
            with open(rc, "r", encoding="utf-8", errors="ignore") as f:
                txt = f.read(65536)
            for m in src_re.finditer(txt):
                raw = m.group(1)
                if raw.startswith("~/"):
                    raw = os.path.join(home, raw[2:])
                elif raw.startswith("$HOME/"):
                    raw = os.path.join(home, raw[6:])
                elif raw.startswith("${HOME}/"):
                    raw = os.path.join(home, raw[8:])
                extra.add(raw)
        except OSError:
            pass
    for p in sorted(extra):
        if p not in paths:
            paths.append(p)
    return paths


ENV_WATCH_PATHS = _collect_env_watch_paths()


def _snapshot_env_files():
    # Record both lstat (symlink itself) and stat (resolved target) so edits through a symlinked
    # startup file (e.g. ~/.bashrc -> ~/dotfiles/bashrc) invalidate warm sessions immediately.
    snap = []
    for p in ENV_WATCH_PATHS:
        try:
            lst = os.lstat(p)
            lmeta = (lst.st_mtime_ns, lst.st_size, lst.st_ino)
        except OSError:
            lmeta = None
        try:
            st = os.stat(p)
            smeta = (st.st_mtime_ns, st.st_size, st.st_ino)
        except OSError:
            smeta = None
        snap.append((lmeta, smeta))
    return tuple(snap)


ENV_SNAPSHOT = _snapshot_env_files()

OUT_LOCK = threading.Lock()
EXEC_SEM = threading.Semaphore(max(8, min(16, (os.cpu_count() or 2) * 4)))
STOP_EVT = threading.Event()
ACTIVE_LOCK = threading.Lock()
CANCEL_EVTS = {}
ACTIVE_PROCS = {}


def kill_proc(p):
    try:
        os.killpg(p.pid, signal.SIGKILL)
    except OSError:
        try:
            p.kill()
        except OSError:
            pass


def is_canceled(cancel_evt):
    return STOP_EVT.is_set() or (cancel_evt is not None and cancel_evt.is_set())


def register_proc(rid, p):
    if rid is None:
        return
    with ACTIVE_LOCK:
        ACTIVE_PROCS.setdefault(rid, set()).add(p)


def unregister_proc(rid, p):
    if rid is None:
        return
    with ACTIVE_LOCK:
        s = ACTIVE_PROCS.get(rid)
        if s is not None:
            s.discard(p)
            if not s:
                ACTIVE_PROCS.pop(rid, None)


def cancel_request(rid):
    if rid is None:
        return
    with ACTIVE_LOCK:
        ev = CANCEL_EVTS.get(rid)
        if ev is not None:
            ev.set()
        procs = list(ACTIVE_PROCS.get(rid, ()))
    for p in procs:
        kill_proc(p)


def cancel_all():
    STOP_EVT.set()
    with ACTIVE_LOCK:
        for ev in CANCEL_EVTS.values():
            ev.set()
        procs = [p for s in ACTIVE_PROCS.values() for p in s]
    for p in procs:
        kill_proc(p)


# Single-source helpers shared by both in-process calls and `sudo -n python3 -B -c` subprocesses.
STAT_HELPER_SRC = """
import hashlib, json, os, stat, sys
def stat_one(p):
    try:
        st = os.lstat(p)
    except (FileNotFoundError, NotADirectoryError):
        return {"exists": False}
    except OSError as e:
        return {"err": f"stat {p}: {e}"}
    m = st.st_mode
    if stat.S_ISLNK(m):
        try:
            return {"exists": True, "type": "link", "target": os.readlink(p)}
        except OSError as e:
            return {"err": f"stat {p}: {e}"}
    if stat.S_ISDIR(m):
        return {"exists": True, "type": "dir", "mode": stat.S_IMODE(m)}
    if stat.S_ISREG(m):
        try:
            h = hashlib.sha256()
            with open(p, "rb") as f:
                while True:
                    b = f.read(65536)
                    if not b:
                        break
                    h.update(b)
            return {"exists": True, "type": "file", "mode": stat.S_IMODE(m), "sha256": h.hexdigest()}
        except OSError as e:
            return {"err": f"stat {p}: {e}"}
    return {"exists": True, "type": "other"}
"""

READ_HELPER_SRC = """
import base64, hashlib, json, os, stat, sys
def read_one(p):
    try:
        st = os.stat(p)
    except (FileNotFoundError, NotADirectoryError):
        try:
            os.lstat(p)
            return {"exists": True, "not_regular": True}
        except OSError:
            return {"exists": False}
    except OSError as e:
        return {"err": f"read {p}: {e}"}
    if not stat.S_ISREG(st.st_mode):
        return {"exists": True, "not_regular": True}
    try:
        with open(p, "rb") as f:
            data = f.read()
    except OSError as e:
        return {"err": f"read {p}: {e}"}
    ftype = "file"
    mode = stat.S_IMODE(st.st_mode)
    try:
        lst = os.lstat(p)
        if stat.S_ISLNK(lst.st_mode):
            ftype = "link"
            mode = 0
    except OSError:
        pass
    return {
        "exists": True,
        "type": ftype,
        "mode": mode,
        "sha256": hashlib.sha256(data).hexdigest(),
        "data_b64": base64.b64encode(data).decode("ascii"),
    }
"""

SUDO_WRITE_PY = """
import os, shutil, sys, tempfile
stage, dst, mode = sys.argv[1], sys.argv[2], int(sys.argv[3])
tmp = None
try:
    d = os.path.dirname(dst) or "."
    os.makedirs(d, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".fam-tmp.", dir=d)
    try:
        with open(stage, "rb") as src, os.fdopen(fd, "wb") as out:
            shutil.copyfileobj(src, out)
    except Exception:
        try:
            os.close(fd)
        except OSError:
            pass
        raise
    os.chmod(tmp, mode)
    os.replace(tmp, dst)
    tmp = None
finally:
    if tmp:
        try:
            os.unlink(tmp)
        except OSError:
            pass
    try:
        os.unlink(stage)
    except OSError:
        pass
"""

SUDO_SYMLINK_PY = """
import os, sys, tempfile
dst, target, mode = sys.argv[1], sys.argv[2], sys.argv[3]
d = os.path.dirname(dst) or "."
os.makedirs(d, exist_ok=True)
if mode == "create":
    os.symlink(target, dst)
else:
    tmp = None
    try:
        fd, tmp = tempfile.mkstemp(prefix=".fam-lnk.", dir=d)
        os.close(fd)
        os.unlink(tmp)
        os.symlink(target, tmp)
        os.replace(tmp, dst)
        tmp = None
    finally:
        if tmp:
            try:
                os.unlink(tmp)
            except OSError:
                pass
"""

_NS = {}
exec(STAT_HELPER_SRC, _NS)
exec(READ_HELPER_SRC, _NS)
stat_one = _NS["stat_one"]
read_one = _NS["read_one"]

SUDO_STAT_CLI = STAT_HELPER_SRC + "\nprint(json.dumps(stat_one(sys.argv[1])))\n"
SUDO_READ_CLI = READ_HELPER_SRC + "\nprint(json.dumps(read_one(sys.argv[1])))\n"


def send(obj):
    line = (json.dumps(obj, separators=(",", ":")) + "\n").encode("utf-8")
    with OUT_LOCK:
        sys.stdout.buffer.write(line)
        sys.stdout.buffer.flush()


def sudo_hint(msg, sudo):
    if sudo and ("a password is required" in msg or "a terminal is required" in msg):
        return msg + " (FAM runs `sudo -n` on workers: give the SSH user passwordless sudo, or remove `sudo = true`)"
    return msg


def run_subproc(cmd, stdin_bytes=b"", timeout_ms=0, env=None, rid=None, cancel_evt=None):
    t0 = time.monotonic()
    if is_canceled(cancel_evt):
        return -1, b"", b"", 0, False
    p = subprocess.Popen(
        cmd,
        stdin=subprocess.PIPE if stdin_bytes else subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
        start_new_session=True,
    )
    register_proc(rid, p)
    if is_canceled(cancel_evt):
        kill_proc(p)
    timeout = (timeout_ms / 1000.0) if timeout_ms and timeout_ms > 0 else None
    timed_out = False
    try:
        out, err = p.communicate(input=stdin_bytes if stdin_bytes else None, timeout=timeout)
        code = p.returncode
    except subprocess.TimeoutExpired:
        timed_out = True
        kill_proc(p)
        try:
            out, err = p.communicate(timeout=5)
        except Exception:
            out, err = b"", b""
        code = -1
    finally:
        unregister_proc(rid, p)
    dur_ms = int((time.monotonic() - t0) * 1000)
    return code, out, err, dur_ms, timed_out


def do_stat(req, rid=None, cancel_evt=None):
    p = req["path"]
    if req.get("sudo"):
        with EXEC_SEM:
            code, out, err, dur_ms, timed_out = run_subproc(
                ["sudo", "-n", sys.executable, "-B", "-c", SUDO_STAT_CLI, p],
                timeout_ms=60000,
                rid=rid,
                cancel_evt=cancel_evt,
            )
        if timed_out:
            return {"err": f"stat {p}: timed out", "duration_ms": dur_ms}
        if code != 0:
            return {"err": sudo_hint(f"stat {p}: exit code {code}: {err.decode('utf-8', 'replace').strip()}", True), "duration_ms": dur_ms}
        res = json.loads(out.decode("utf-8"))
        res["duration_ms"] = dur_ms
        return res
    t0 = time.monotonic()
    res = stat_one(p)
    res["duration_ms"] = int((time.monotonic() - t0) * 1000)
    return res


def do_read(req, rid=None, cancel_evt=None):
    p = req["path"]
    if req.get("sudo"):
        with EXEC_SEM:
            code, out, err, dur_ms, timed_out = run_subproc(
                ["sudo", "-n", sys.executable, "-B", "-c", SUDO_READ_CLI, p],
                timeout_ms=60000,
                rid=rid,
                cancel_evt=cancel_evt,
            )
        if timed_out:
            return {"err": f"read {p}: timed out", "duration_ms": dur_ms}
        if code != 0:
            return {"err": sudo_hint(f"read {p}: exit code {code}: {err.decode('utf-8', 'replace').strip()}", True), "duration_ms": dur_ms}
        res = json.loads(out.decode("utf-8"))
        res["duration_ms"] = dur_ms
        return res
    t0 = time.monotonic()
    res = read_one(p)
    res["duration_ms"] = int((time.monotonic() - t0) * 1000)
    return res


def write_bytes(p, data, mode, sudo, rid=None, cancel_evt=None):
    if is_canceled(cancel_evt):
        return "canceled"
    if sudo:
        fd, stage = tempfile.mkstemp(prefix="fam-stage.")
        try:
            with os.fdopen(fd, "wb") as f:
                f.write(data)
        except Exception as e:
            try:
                os.unlink(stage)
            except OSError:
                pass
            return f"stage {p}: {e}"
        if is_canceled(cancel_evt):
            try:
                os.unlink(stage)
            except OSError:
                pass
            return "canceled"
        with EXEC_SEM:
            code, out, err, dur_ms, timed_out = run_subproc(
                ["sudo", "-n", sys.executable, "-B", "-c", SUDO_WRITE_PY, stage, p, str(mode)],
                timeout_ms=120000,
                rid=rid,
                cancel_evt=cancel_evt,
            )
        if code != 0 or timed_out:
            try:
                os.unlink(stage)
            except OSError:
                pass
            if timed_out:
                return f"write {p} with sudo: timed out"
            return sudo_hint(f"write {p} with sudo: exit code {code}: {err.decode('utf-8', 'replace').strip()}", True)
        return ""
    tmp = None
    try:
        d = os.path.dirname(p) or "."
        os.makedirs(d, exist_ok=True)
        fd, tmp = tempfile.mkstemp(prefix=".fam-tmp.", dir=d)
        try:
            with os.fdopen(fd, "wb") as f:
                f.write(data)
        except Exception:
            try:
                os.close(fd)
            except OSError:
                pass
            raise
        os.chmod(tmp, mode)
        if is_canceled(cancel_evt):
            try:
                os.unlink(tmp)
            except OSError:
                pass
            tmp = None
            return "canceled"
        os.replace(tmp, p)
        tmp = None
    except Exception as e:
        if tmp:
            try:
                os.unlink(tmp)
            except OSError:
                pass
        return f"write {p}: {e}"
    return ""


def do_write(req, rid=None, cancel_evt=None):
    p = req["path"]
    data = base64.b64decode(req.get("data_b64", ""))
    mode = (int(req["mode"]) & 0o777) if req.get("mode") is not None else 0o644
    sudo = bool(req.get("sudo"))
    t0 = time.monotonic()

    # Optional compare-and-swap guard so Go-planned batches abort cleanly if the file drifted.
    if req.get("if_missing") or req.get("if_sha256"):
        cur = do_stat({"path": p, "sudo": sudo}, rid=rid, cancel_evt=cancel_evt)
        if cur.get("err"):
            cur["duration_ms"] = int((time.monotonic() - t0) * 1000)
            return cur
        if req.get("if_missing"):
            if cur.get("exists"):
                return {"stale": True, "duration_ms": int((time.monotonic() - t0) * 1000)}
        else:
            if not cur.get("exists") or cur.get("type") != "file" or cur.get("sha256") != req.get("if_sha256"):
                return {"stale": True, "duration_ms": int((time.monotonic() - t0) * 1000)}

    err = write_bytes(p, data, mode, sudo, rid=rid, cancel_evt=cancel_evt)
    if err:
        return {"err": err, "duration_ms": int((time.monotonic() - t0) * 1000)}
    if req.get("verify"):
        st = do_stat({"path": p, "sudo": sudo}, rid=rid, cancel_evt=cancel_evt)
        st["duration_ms"] = int((time.monotonic() - t0) * 1000)
        return st
    return {"duration_ms": int((time.monotonic() - t0) * 1000)}


def do_symlink(req, rid=None, cancel_evt=None):
    p = req["path"]
    target = req["target"]
    sudo = bool(req.get("sudo"))
    t0 = time.monotonic()
    want_missing = bool(req.get("if_missing"))
    want_link_target = req.get("if_link_target")

    # Symlink primitive requires an explicit CAS guard (missing or known existing symlink target).
    cur = do_stat({"path": p, "sudo": sudo}, rid=rid, cancel_evt=cancel_evt)
    if cur.get("err"):
        cur["duration_ms"] = int((time.monotonic() - t0) * 1000)
        return cur
    if want_missing:
        if cur.get("exists"):
            return {"stale": True, "duration_ms": int((time.monotonic() - t0) * 1000)}
    elif want_link_target is not None:
        if not cur.get("exists") or cur.get("type") != "link" or cur.get("target") != want_link_target:
            return {"stale": True, "duration_ms": int((time.monotonic() - t0) * 1000)}
    else:
        return {"stale": True, "duration_ms": int((time.monotonic() - t0) * 1000)}

    if is_canceled(cancel_evt):
        return {"err": "canceled", "duration_ms": int((time.monotonic() - t0) * 1000)}

    if sudo:
        mode = "create" if want_missing else "replace"
        with EXEC_SEM:
            code, out, err, dur_ms, timed_out = run_subproc(
                ["sudo", "-n", sys.executable, "-B", "-c", SUDO_SYMLINK_PY, p, target, mode],
                timeout_ms=60000,
                rid=rid,
                cancel_evt=cancel_evt,
            )
        if timed_out:
            return {"err": f"link {p}: timed out", "duration_ms": int((time.monotonic() - t0) * 1000)}
        if code != 0:
            if want_missing and b"FileExistsError" in err:
                return {"stale": True, "duration_ms": int((time.monotonic() - t0) * 1000)}
            return {"err": sudo_hint(f"link {p}: exit code {code}: {err.decode('utf-8', 'replace').strip()}", True), "duration_ms": int((time.monotonic() - t0) * 1000)}
    else:
        try:
            d = os.path.dirname(p) or "."
            os.makedirs(d, exist_ok=True)
            if want_missing:
                # Direct os.symlink guarantees EEXIST if a concurrent file/FIFO appears.
                os.symlink(target, p)
            else:
                tmp = None
                try:
                    fd, tmp = tempfile.mkstemp(prefix=".fam-lnk.", dir=d)
                    os.close(fd)
                    os.unlink(tmp)
                    os.symlink(target, tmp)
                    os.replace(tmp, p)
                    tmp = None
                finally:
                    if tmp:
                        try:
                            os.unlink(tmp)
                        except OSError:
                            pass
        except FileExistsError:
            return {"stale": True, "duration_ms": int((time.monotonic() - t0) * 1000)}
        except OSError as e:
            return {"err": f"link {p}: {e}", "duration_ms": int((time.monotonic() - t0) * 1000)}

    st = do_stat({"path": p, "sudo": sudo}, rid=rid, cancel_evt=cancel_evt)
    st["duration_ms"] = int((time.monotonic() - t0) * 1000)
    return st


def do_skill_check(req):
    d = req["dir"]
    marker = req["marker"]
    t0 = time.monotonic()
    try:
        st = os.lstat(d)
    except (FileNotFoundError, NotADirectoryError):
        return {"exists": False, "duration_ms": int((time.monotonic() - t0) * 1000)}
    except OSError as e:
        return {"err": f"stat {d}: {e}", "duration_ms": int((time.monotonic() - t0) * 1000)}
    if not stat.S_ISDIR(st.st_mode):
        return {"exists": True, "is_dir": False, "duration_ms": int((time.monotonic() - t0) * 1000)}
    mp = os.path.join(d, marker)
    try:
        os.lstat(mp)
    except (FileNotFoundError, NotADirectoryError):
        return {"exists": True, "is_dir": True, "managed": False, "duration_ms": int((time.monotonic() - t0) * 1000)}
    except OSError as e:
        return {"err": f"stat {mp}: {e}", "duration_ms": int((time.monotonic() - t0) * 1000)}
    lines = []
    marker_bytes = os.fsencode(marker)
    d_bytes = os.fsencode(d)
    try:
        for root, dirs, files in os.walk(d_bytes, followlinks=False):
            for fname in files:
                full = os.path.join(root, fname)
                fst = os.lstat(full)
                if not stat.S_ISREG(fst.st_mode):
                    continue
                rel = os.path.relpath(full, d_bytes)
                if os.sep != "/":
                    rel = rel.replace(os.fsencode(os.sep), b"/")
                if rel == marker_bytes:
                    continue
                h = hashlib.sha256()
                with open(full, "rb") as f:
                    while True:
                        b = f.read(65536)
                        if not b:
                            break
                        h.update(b)
                ex = b"true" if (fst.st_mode & 0o100) else b"false"
                lines.append(h.hexdigest().encode("ascii") + b" " + rel + b" " + ex)
    except OSError as e:
        return {"err": f"digest of {d}: {e}", "duration_ms": int((time.monotonic() - t0) * 1000)}
    lines.sort()
    digest = hashlib.sha256(b"\n".join(lines)).hexdigest()
    return {"exists": True, "is_dir": True, "managed": True, "digest": digest, "duration_ms": int((time.monotonic() - t0) * 1000)}


SKILL_LOCKS_GUARD = threading.Lock()
SKILL_LOCKS = {}


def _skill_lock_key(d):
    abs_d = os.path.abspath(d)
    parent = os.path.realpath(os.path.dirname(abs_d) or ".")
    return os.path.join(parent, os.path.basename(abs_d))


def _skill_lock(d):
    key = _skill_lock_key(d)
    with SKILL_LOCKS_GUARD:
        lk = SKILL_LOCKS.get(key)
        if lk is None:
            lk = threading.Lock()
            SKILL_LOCKS[key] = lk
        return lk


class _CanceledError(Exception):
    pass


def _ensure_skill_lock_dir():
    # Use a fixed per-user directory under /tmp (independent of TMPDIR and HOME) with strict
    # ownership and non-symlink checks, matching transport.SSH controlDir().
    uid = os.getuid() if hasattr(os, "getuid") else 0
    lock_dir = os.path.join("/tmp", f"fam-skill-locks-{uid}")
    os.makedirs(lock_dir, mode=0o700, exist_ok=True)
    st = os.lstat(lock_dir)
    if not stat.S_ISDIR(st.st_mode) or stat.S_ISLNK(st.st_mode):
        raise RuntimeError(f"skill lock dir {lock_dir} is not a directory")
    if hasattr(os, "getuid") and st.st_uid != uid:
        raise RuntimeError(f"skill lock dir {lock_dir} is not owned by uid {uid}")
    if stat.S_IMODE(st.st_mode) != 0o700:
        os.chmod(lock_dir, 0o700)
    return lock_dir


@contextlib.contextmanager
def _skill_flock(d, cancel_evt=None):
    # Serialize both threads inside this worker AND separate concurrent worker processes targeting
    # the same canonical skill destination, polling for cancellation while waiting on the lock.
    key = _skill_lock_key(d)
    tlk = _skill_lock(d)
    while not tlk.acquire(timeout=0.02):
        if is_canceled(cancel_evt):
            raise _CanceledError()
    try:
        if is_canceled(cancel_evt):
            raise _CanceledError()
        if fcntl is None:
            yield
            return
        lock_dir = _ensure_skill_lock_dir()
        lock_path = os.path.join(lock_dir, hashlib.sha256(key.encode("utf-8", "surrogateescape")).hexdigest() + ".lock")
        fd = os.open(lock_path, os.O_CREAT | os.O_RDWR, 0o600)
        locked = False
        try:
            while True:
                if is_canceled(cancel_evt):
                    raise _CanceledError()
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    locked = True
                    break
                except BlockingIOError:
                    time.sleep(0.02)
            yield
        finally:
            if locked:
                try:
                    fcntl.flock(fd, fcntl.LOCK_UN)
                except OSError:
                    pass
            os.close(fd)
    finally:
        tlk.release()


def _check_skill_cas(req, d, marker, t0):
    if req.get("if_missing"):
        if os.path.lexists(d):
            return {"stale": True, "duration_ms": int((time.monotonic() - t0) * 1000)}
    elif req.get("if_digest"):
        cur = do_skill_check({"dir": d, "marker": marker})
        if cur.get("err"):
            cur["duration_ms"] = int((time.monotonic() - t0) * 1000)
            return cur
        if not cur.get("exists") or not cur.get("is_dir") or not cur.get("managed") or cur.get("digest") != req.get("if_digest"):
            return {"stale": True, "duration_ms": int((time.monotonic() - t0) * 1000)}
    return None


def do_skill_write(req, cancel_evt=None):
    d = req["dir"]
    marker = req["marker"]
    tb = base64.b64decode(req.get("tar_b64", ""))
    t0 = time.monotonic()
    parent = os.path.dirname(d) or "."
    has_cas = bool(req.get("if_missing") or req.get("if_digest"))
    tmp = None
    try:
        if is_canceled(cancel_evt):
            return {"err": "canceled", "duration_ms": int((time.monotonic() - t0) * 1000)}
        # Fast-fail before extraction if the destination is already stale.
        cas_fail = _check_skill_cas(req, d, marker, t0)
        if cas_fail is not None:
            return cas_fail
        os.makedirs(parent, exist_ok=True)
        tmp = tempfile.mkdtemp(prefix=".fam-tmp-", dir=parent)
        tmp_abs = os.path.realpath(tmp)
        with tarfile.open(fileobj=io.BytesIO(tb), mode="r:") as tf:
            for m in tf.getmembers():
                if is_canceled(cancel_evt):
                    return {"err": "canceled", "duration_ms": int((time.monotonic() - t0) * 1000)}
                mname = m.name
                if os.path.isabs(mname) or ".." in mname.split("/"):
                    return {"err": f"copy skill: invalid archive path {m.name!r}", "duration_ms": int((time.monotonic() - t0) * 1000)}
                dst = os.path.join(tmp_abs, mname)
                dst_norm = os.path.normpath(dst)
                if os.path.commonpath([tmp_abs, dst_norm]) != tmp_abs:
                    return {"err": f"copy skill: archive path {m.name!r} escapes target directory", "duration_ms": int((time.monotonic() - t0) * 1000)}
                if m.isdir():
                    os.makedirs(dst_norm, mode=0o755, exist_ok=True)
                elif m.isfile():
                    os.makedirs(os.path.dirname(dst_norm), exist_ok=True)
                    src = tf.extractfile(m)
                    with open(dst_norm, "wb") as out:
                        if src is not None:
                            shutil.copyfileobj(src, out)
                    os.chmod(dst_norm, m.mode & 0o777)
        os.chmod(tmp, 0o755)
        if is_canceled(cancel_evt):
            return {"err": "canceled", "duration_ms": int((time.monotonic() - t0) * 1000)}
        # Serialize concurrent threads and separate worker processes targeting the same canonical
        # skill directory across the final CAS recheck, cancellation recheck, swap, and verification.
        with _skill_flock(d, cancel_evt=cancel_evt):
            cas_fail = _check_skill_cas(req, d, marker, t0)
            if cas_fail is not None:
                return cas_fail
            if is_canceled(cancel_evt):
                return {"err": "canceled", "duration_ms": int((time.monotonic() - t0) * 1000)}
            if os.path.lexists(d):
                if os.path.islink(d) or not os.path.isdir(d) or not os.path.isfile(os.path.join(d, marker)):
                    if has_cas:
                        return {"stale": True, "duration_ms": int((time.monotonic() - t0) * 1000)}
                    return {"err": f"copy skill: {d} is not a FAM-managed skill folder", "duration_ms": int((time.monotonic() - t0) * 1000)}
                shutil.rmtree(d)
            os.rename(tmp, d)
            tmp = None
            if req.get("verify"):
                st = do_skill_check({"dir": d, "marker": marker})
                st["duration_ms"] = int((time.monotonic() - t0) * 1000)
                return st
    except _CanceledError:
        return {"err": "canceled", "duration_ms": int((time.monotonic() - t0) * 1000)}
    except Exception as e:
        return {"err": f"copy skill: {e}", "duration_ms": int((time.monotonic() - t0) * 1000)}
    finally:
        if tmp:
            shutil.rmtree(tmp, ignore_errors=True)
    return {"duration_ms": int((time.monotonic() - t0) * 1000)}


def do_exec(req, rid=None, cancel_evt=None):
    script = req["script"]
    stdin_bytes = base64.b64decode(req["stdin_b64"]) if req.get("stdin_b64") else b""
    timeout_ms = int(req.get("timeout_ms", 0))
    cmd = ["sudo", "-n", "sh", "-c", script] if req.get("sudo") else ["sh", "-c", script]
    with EXEC_SEM:
        code, out, err, dur_ms, timed_out = run_subproc(cmd, stdin_bytes=stdin_bytes, timeout_ms=timeout_ms, rid=rid, cancel_evt=cancel_evt)
    env_changed = _snapshot_env_files() != ENV_SNAPSHOT
    res = {
        "code": code,
        "stdout_b64": base64.b64encode(out).decode("ascii"),
        "stderr_b64": base64.b64encode(err).decode("ascii"),
        "duration_ms": dur_ms,
        "timed_out": timed_out,
    }
    if env_changed:
        res["env_changed"] = True
    return res


def _run_spec_reqs(sub_reqs, rid, cancel_evt, on_item):
    if not sub_reqs:
        return

    def run_one(sr):
        if is_canceled(cancel_evt):
            return None
        fn = SPEC_HANDLERS.get(sr.get("op"))
        if fn is None:
            return {"err": f"disallowed speculative op {sr.get('op')}"}
        try:
            return fn(sr, rid, cancel_evt)
        except Exception as e:
            return {"err": str(e)}

    with concurrent.futures.ThreadPoolExecutor(max_workers=min(24, len(sub_reqs))) as sp:
        futs = {sp.submit(run_one, sr): sr.get("key") for sr in sub_reqs}
        for fut in concurrent.futures.as_completed(futs):
            if is_canceled(cancel_evt):
                break
            skey = futs[fut]
            try:
                res = fut.result()
            except Exception as e:
                res = {"err": str(e)}
            if res is not None:
                on_item(skey, res)


def do_spec_batch(rid, req, cancel_evt):
    expected = req.get("facts_sha", "")
    if not expected or expected != FACTS_SHA:
        send({"id": rid, "done": True, "stale": True})
        return
    _run_spec_reqs(
        req.get("reqs") or [],
        rid,
        cancel_evt,
        lambda skey, res: send({"id": rid, "spec_key": skey, "spec_resp": res}),
    )
    send({"id": rid, "done": True})


def do_warm_bootstrap(rid, req, cancel_evt):
    if _snapshot_env_files() != ENV_SNAPSHOT:
        return {"env_changed": True}
    facts_script = req.get("facts_script", "")
    secrets_rel = req.get("secrets_rel", "")
    expected_sha = req.get("facts_sha", "")
    sub_reqs = req.get("reqs") or []
    with EXEC_SEM:
        fcode, fout, ferr, fdur, fto = run_subproc(["sh", "-c", facts_script], timeout_ms=120000, rid=rid, cancel_evt=cancel_evt)
    live_sha = hashlib.sha256(fout.rstrip(b"\r\n")).hexdigest() if fcode == 0 else ""
    sec_exists = False
    sec_b64 = ""
    if secrets_rel:
        sec_path = os.path.join(os.path.expanduser("~"), secrets_rel)
        try:
            if os.path.isfile(sec_path) and os.access(sec_path, os.R_OK):
                with open(sec_path, "rb") as sf:
                    sec_b64 = base64.b64encode(sf.read()).decode("ascii")
                sec_exists = True
        except OSError:
            pass
    spec_hit = bool(expected_sha and live_sha == expected_sha and fcode == 0)
    spec_results = {}
    if spec_hit and sub_reqs:
        _run_spec_reqs(sub_reqs, rid, cancel_evt, lambda skey, res: spec_results.__setitem__(skey, res))
    return {
        "code": fcode,
        "stdout_b64": base64.b64encode(fout).decode("ascii"),
        "stderr_b64": base64.b64encode(ferr).decode("ascii"),
        "duration_ms": fdur,
        "timed_out": fto,
        "exists": sec_exists,
        "data_b64": sec_b64,
        "spec_hit": spec_hit,
        "spec_results": spec_results,
    }


def do_batch(rid, req, cancel_evt):
    # Sequential execution of Go-planned dumb primitives (write, symlink, skill_write).
    # Stops at the first error or CAS mismatch ("stale") so Go owns all conflict/error rules.
    for op in req.get("ops") or []:
        if is_canceled(cancel_evt):
            break
        idx = op.get("index", 0)
        kind = op.get("op", "")
        fn = HANDLERS.get(kind)
        if fn is None:
            send({"id": rid, "prim": {"index": idx, "err": f"unknown primitive op {kind}"}})
            break
        try:
            res = fn(op, rid, cancel_evt)
        except Exception as e:
            res = {"err": str(e)}
        res["index"] = idx
        send({"id": rid, "prim": res})
        if res.get("stale") or res.get("err") or op.get("stop_after"):
            break
    send({"id": rid, "done": True})


# Strictly read-only handlers allowed inside speculative pre-check batches.
SPEC_HANDLERS = {
    "stat": lambda req, rid, ev: do_stat(req, rid=rid, cancel_evt=ev),
    "read": lambda req, rid, ev: do_read(req, rid=rid, cancel_evt=ev),
    "skill_check": lambda req, rid, ev: do_skill_check(req),
    "exec": lambda req, rid, ev: do_exec(req, rid=rid, cancel_evt=ev),
}

HANDLERS = {
    **SPEC_HANDLERS,
    "write": lambda req, rid, ev: do_write(req, rid=rid, cancel_evt=ev),
    "symlink": lambda req, rid, ev: do_symlink(req, rid=rid, cancel_evt=ev),
    "skill_write": lambda req, rid, ev: do_skill_write(req, cancel_evt=ev),
    "warm_bootstrap": lambda req, rid, ev: do_warm_bootstrap(rid, req, ev),
}


def handle(req, cancel_evt):
    rid = req.get("id")
    op = req.get("op")
    try:
        if is_canceled(cancel_evt):
            if op in ("batch", "spec_batch"):
                send({"id": rid, "done": True})
            return
        if op == "batch":
            try:
                do_batch(rid, req, cancel_evt)
            except Exception as e:
                send({"id": rid, "done": True, "err": str(e)})
            return
        if op == "spec_batch":
            try:
                do_spec_batch(rid, req, cancel_evt)
            except Exception as e:
                send({"id": rid, "done": True, "err": str(e)})
            return
        fn = HANDLERS.get(op)
        if fn is None:
            send({"id": rid, "err": f"unknown op {op}"})
            return
        try:
            res = fn(req, rid, cancel_evt)
        except Exception as e:
            res = {"err": str(e)}
        res["id"] = rid
        send(res)
    finally:
        with ACTIVE_LOCK:
            CANCEL_EVTS.pop(rid, None)


pool = concurrent.futures.ThreadPoolExecutor(max_workers=24)
if SPEC_B64:
    try:
        _spec_req = json.loads(base64.b64decode(SPEC_B64).decode("utf-8"))
        _spec_ev = threading.Event()
        with ACTIVE_LOCK:
            CANCEL_EVTS[_spec_req.get("id")] = _spec_ev
        pool.submit(handle, _spec_req, _spec_ev)
    except Exception:
        pass
try:
    for raw in sys.stdin.buffer:
        raw = raw.strip()
        if not raw:
            continue
        req = json.loads(raw.decode("utf-8"))
        op = req.get("op")
        if op == "quit":
            cancel_all()
            break
        if op == "cancel":
            cancel_request(req.get("target_id"))
            continue
        rid = req.get("id")
        ev = threading.Event()
        with ACTIVE_LOCK:
            CANCEL_EVTS[rid] = ev
        pool.submit(handle, req, ev)
finally:
    cancel_all()
    pool.shutdown(wait=False)
