package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Bootstrapper is implemented by transports that can establish a persistent session while
// collecting initial machine facts and reading the existing secrets file in one round trip.
type Bootstrapper interface {
	Bootstrap(ctx context.Context, factsScript, secretsRelPath string) (factsRes Result, secretsData []byte, secretsExists bool, err error)
}

// wireWorkerScript is a zero-dependency Python 3.6+ multiplexed RPC worker run inline over a
// single persistent `ssh -T` connection. It writes no files on `fam plan` and handles concurrent
// stat, read, write, skill_check, skill_write, exec, and sequential apply_seq RPCs over
// stdin/stdout NDJSON.
const wireWorkerScript = `
import base64, concurrent.futures, hashlib, io, json, os, shutil, signal, stat, subprocess, sys, tarfile, tempfile, threading, time

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

SUDO_STAT_PY = """
import hashlib, json, os, stat, sys
p = sys.argv[1]
try:
    st = os.lstat(p)
except (FileNotFoundError, NotADirectoryError):
    print(json.dumps({"exists": False}))
    sys.exit(0)
except PermissionError as e:
    sys.stderr.write(str(e) + "\\n")
    sys.exit(5)
m = st.st_mode
if stat.S_ISLNK(m):
    print(json.dumps({"exists": True, "type": "link", "target": os.readlink(p)}))
elif stat.S_ISDIR(m):
    print(json.dumps({"exists": True, "type": "dir", "mode": stat.S_IMODE(m)}))
elif stat.S_ISREG(m):
    h = hashlib.sha256()
    with open(p, "rb") as f:
        while True:
            b = f.read(65536)
            if not b:
                break
            h.update(b)
    print(json.dumps({"exists": True, "type": "file", "mode": stat.S_IMODE(m), "sha256": h.hexdigest()}))
else:
    print(json.dumps({"exists": True, "type": "other"}))
"""

SUDO_READ_PY = """
import base64, hashlib, json, os, stat, sys
p = sys.argv[1]
try:
    st = os.stat(p)
except (FileNotFoundError, NotADirectoryError):
    try:
        os.lstat(p)
        print(json.dumps({"exists": True, "not_regular": True}))
    except OSError:
        print(json.dumps({"exists": False}))
    sys.exit(0)
except PermissionError as e:
    sys.stderr.write(str(e) + "\\n")
    sys.exit(5)
if not stat.S_ISREG(st.st_mode):
    print(json.dumps({"exists": True, "not_regular": True}))
    sys.exit(0)
with open(p, "rb") as f:
    data = f.read()
ftype = "file"
mode = stat.S_IMODE(st.st_mode)
try:
    lst = os.lstat(p)
    if stat.S_ISLNK(lst.st_mode):
        ftype = "link"
        mode = 0
except OSError:
    pass
print(json.dumps({
    "exists": True,
    "type": ftype,
    "mode": mode,
    "sha256": hashlib.sha256(data).hexdigest(),
    "data_b64": base64.b64encode(data).decode("ascii"),
}))
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

def send(obj):
    line = (json.dumps(obj, separators=(",", ":")) + "\n").encode("utf-8")
    with OUT_LOCK:
        sys.stdout.buffer.write(line)
        sys.stdout.buffer.flush()

def sh_quote(s):
    if s == "":
        return "''"
    return "'" + s.replace("'", "'\\''") + "'"

def tail_lines(s, n=6):
    s = s.strip()
    if not s:
        return ""
    lines = s.split("\n")
    if len(lines) > n:
        lines = lines[-n:]
    return "\n".join(lines)

def last_line(s):
    s = s.strip()
    i = s.rfind("\n")
    return s[i + 1:] if i >= 0 else s

def first_line(s):
    s = s.strip()
    i = s.find("\n")
    return s[:i].strip() if i >= 0 else s

def sudo_hint(msg, sudo):
    if sudo and ("a password is required" in msg or "a terminal is required" in msg):
        return msg + " (FAM runs \x60sudo -n\x60 on workers: give the SSH user passwordless sudo, or remove \x60sudo = true\x60)"
    return msg

def exit_err(label, code, stderr_b, stdout_b, sudo=False):
    msg = tail_lines(stderr_b.decode("utf-8", "replace"), 6)
    if not msg:
        msg = tail_lines(stdout_b.decode("utf-8", "replace"), 6)
    if msg:
        out = f"{label}: exit code {code}: {msg}"
    else:
        out = f"{label}: exit code {code}"
    return sudo_hint(out, sudo)

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

def run_user_script(prelude, script, sudo, timeout_ms, rid=None, cancel_evt=None):
    q = sh_quote(script)
    full = prelude + "if command -v bash >/dev/null 2>&1; then exec bash -c " + q + "; else exec sh -c " + q + "; fi"
    cmd = ["sudo", "-n", "sh", "-c", full] if sudo else ["sh", "-c", full]
    return run_subproc(cmd, timeout_ms=timeout_ms, rid=rid, cancel_evt=cancel_evt)

def do_stat(req, rid=None, cancel_evt=None):
    p = req["path"]
    if req.get("sudo"):
        with EXEC_SEM:
            code, out, err, dur_ms, timed_out = run_subproc(["sudo", "-n", sys.executable, "-B", "-c", SUDO_STAT_PY, p], timeout_ms=60000, rid=rid, cancel_evt=cancel_evt)
        if timed_out:
            return {"err": f"stat {p}: timed out", "duration_ms": dur_ms}
        if code != 0:
            return {"err": sudo_hint(f"stat {p}: exit code {code}: {err.decode('utf-8', 'replace').strip()}", True), "duration_ms": dur_ms}
        res = json.loads(out.decode("utf-8"))
        res["duration_ms"] = dur_ms
        return res
    t0 = time.monotonic()
    try:
        st = os.lstat(p)
    except (FileNotFoundError, NotADirectoryError):
        return {"exists": False, "duration_ms": int((time.monotonic() - t0) * 1000)}
    except OSError as e:
        return {"err": f"stat {p}: {e}", "duration_ms": int((time.monotonic() - t0) * 1000)}
    m = st.st_mode
    if stat.S_ISLNK(m):
        try:
            tgt = os.readlink(p)
        except OSError as e:
            return {"err": f"stat {p}: {e}", "duration_ms": int((time.monotonic() - t0) * 1000)}
        return {"exists": True, "type": "link", "target": tgt, "duration_ms": int((time.monotonic() - t0) * 1000)}
    if stat.S_ISDIR(m):
        return {"exists": True, "type": "dir", "mode": stat.S_IMODE(m), "duration_ms": int((time.monotonic() - t0) * 1000)}
    if stat.S_ISREG(m):
        try:
            h = hashlib.sha256()
            with open(p, "rb") as f:
                while True:
                    b = f.read(65536)
                    if not b:
                        break
                    h.update(b)
        except OSError as e:
            return {"err": f"stat {p}: {e}", "duration_ms": int((time.monotonic() - t0) * 1000)}
        return {"exists": True, "type": "file", "mode": stat.S_IMODE(m), "sha256": h.hexdigest(), "duration_ms": int((time.monotonic() - t0) * 1000)}
    return {"exists": True, "type": "other", "duration_ms": int((time.monotonic() - t0) * 1000)}

def do_read(req, rid=None, cancel_evt=None):
    p = req["path"]
    if req.get("sudo"):
        with EXEC_SEM:
            code, out, err, dur_ms, timed_out = run_subproc(["sudo", "-n", sys.executable, "-B", "-c", SUDO_READ_PY, p], timeout_ms=60000, rid=rid, cancel_evt=cancel_evt)
        if timed_out:
            return {"err": f"read {p}: timed out", "duration_ms": dur_ms}
        if code != 0:
            return {"err": sudo_hint(f"read {p}: exit code {code}: {err.decode('utf-8', 'replace').strip()}", True), "duration_ms": dur_ms}
        res = json.loads(out.decode("utf-8"))
        res["duration_ms"] = dur_ms
        return res
    t0 = time.monotonic()
    try:
        st = os.stat(p)
    except (FileNotFoundError, NotADirectoryError):
        try:
            os.lstat(p)
            return {"exists": True, "not_regular": True, "duration_ms": int((time.monotonic() - t0) * 1000)}
        except OSError:
            return {"exists": False, "duration_ms": int((time.monotonic() - t0) * 1000)}
    except OSError as e:
        return {"err": f"read {p}: {e}", "duration_ms": int((time.monotonic() - t0) * 1000)}
    if not stat.S_ISREG(st.st_mode):
        return {"exists": True, "not_regular": True, "duration_ms": int((time.monotonic() - t0) * 1000)}
    try:
        with open(p, "rb") as f:
            data = f.read()
    except OSError as e:
        return {"err": f"read {p}: {e}", "duration_ms": int((time.monotonic() - t0) * 1000)}
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
        "duration_ms": int((time.monotonic() - t0) * 1000),
    }

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
            code, out, err, dur_ms, timed_out = run_subproc(["sudo", "-n", sys.executable, "-B", "-c", SUDO_WRITE_PY, stage, p, str(mode)], timeout_ms=120000, rid=rid, cancel_evt=cancel_evt)
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
    err = write_bytes(p, data, mode, sudo, rid=rid, cancel_evt=cancel_evt)
    if err:
        return {"err": err, "duration_ms": int((time.monotonic() - t0) * 1000)}
    if req.get("verify"):
        st = do_stat({"path": p, "sudo": sudo}, rid=rid, cancel_evt=cancel_evt)
        st["duration_ms"] = int((time.monotonic() - t0) * 1000)
        return st
    return {"duration_ms": int((time.monotonic() - t0) * 1000)}

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

def do_skill_write(req, cancel_evt=None):
    d = req["dir"]
    marker = req["marker"]
    tb = base64.b64decode(req.get("tar_b64", ""))
    t0 = time.monotonic()
    parent = os.path.dirname(d) or "."
    tmp = None
    try:
        if is_canceled(cancel_evt):
            return {"err": "canceled", "duration_ms": int((time.monotonic() - t0) * 1000)}
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
        if os.path.isdir(d) and not os.path.islink(d):
            if not os.path.isfile(os.path.join(d, marker)):
                return {"err": f"copy skill: exit code 3: {d} exists and FAM did not make it", "duration_ms": int((time.monotonic() - t0) * 1000)}
            shutil.rmtree(d)
        os.rename(tmp, d)
        tmp = None
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
    return {
        "code": code,
        "stdout_b64": base64.b64encode(out).decode("ascii"),
        "stderr_b64": base64.b64encode(err).decode("ascii"),
        "duration_ms": dur_ms,
        "timed_out": timed_out,
    }

def apply_block_text(cur_s, block, begin, end, top):
    s = cur_s
    while True:
        i = s.find(begin)
        if i < 0:
            break
        j = s.find(end, i)
        if j < 0:
            break
        e = j + len(end)
        if e < len(s) and s[e] == "\n":
            e += 1
        s = s[:i] + s[e:]
    if top:
        return block + s
    if s != "" and not s.endswith("\n"):
        s += "\n"
    return s + block

def written_by_fam(data, prefix_b):
    idx = data.find(b"\n")
    if idx < 0:
        return False
    line, body = data[:idx], data[idx + 1:]
    if not line.startswith(prefix_b) or not line.endswith(b" -->"):
        return False
    want = line[len(prefix_b):-4].decode("ascii", "replace")
    return want == hashlib.sha256(body).hexdigest()

def apply_one_op(op, rid=None, cancel_evt=None):
    kind = op["kind"]
    idx = op["index"]
    t0 = time.monotonic()
    def done(status, detail="", err=""):
        return {"index": idx, "status": status, "detail": detail, "err": err, "duration_ms": int((time.monotonic() - t0) * 1000)}

    if is_canceled(cancel_evt):
        return done("canceled")

    if kind == "file":
        p = op["path"]
        sudo = bool(op.get("sudo"))
        want_sha = op["sha256"]
        want_mode = int(op["mode"]) & 0o777
        st = do_stat({"path": p, "sudo": sudo}, rid=rid, cancel_evt=cancel_evt)
        if st.get("err"):
            return done("failed", "check failed", st["err"])
        if not st.get("exists"):
            detail = "create"
        elif st.get("type") != "file":
            return done("failed", "check failed", f"{p} is a {st.get('type')}, not a regular file")
        else:
            diffs = []
            if st.get("sha256") != want_sha:
                diffs.append("content differs")
            cur_mode = int(st.get("mode", 0)) & 0o777
            if cur_mode != want_mode:
                diffs.append(f"mode {cur_mode:04o} -> {want_mode:04o}")
            if not diffs:
                return done("unchanged")
            detail = ", ".join(diffs)
        if is_canceled(cancel_evt):
            return done("canceled")
        data = base64.b64decode(op.get("data_b64", ""))
        werr = write_bytes(p, data, want_mode, sudo, rid=rid, cancel_evt=cancel_evt)
        if werr == "canceled":
            return done("canceled")
        if werr:
            return done("failed", detail, werr)
        st2 = do_stat({"path": p, "sudo": sudo}, rid=rid, cancel_evt=cancel_evt)
        if st2.get("err"):
            return done("failed", "check after apply failed", st2["err"])
        if not st2.get("exists") or st2.get("type") != "file" or st2.get("sha256") != want_sha or (int(st2.get("mode", 0)) & 0o777) != want_mode:
            return done("failed", detail, "the check still finds a difference after apply")
        return done("changed", detail)

    if kind == "symlink":
        p = op["path"]
        target = op["target"]
        sudo = bool(op.get("sudo"))
        backup = bool(op.get("backup"))
        strict = bool(op.get("strict_conflict"))
        st = do_stat({"path": p, "sudo": sudo}, rid=rid, cancel_evt=cancel_evt)
        if st.get("err"):
            return done("failed", "check failed", st["err"])
        if not st.get("exists"):
            detail = ("link to " + target) if strict else "create link"
        elif st.get("type") == "link" and st.get("target") == target:
            return done("unchanged")
        elif strict:
            return done("conflict", "", f"{p} exists and FAM did not make it. Remove or rename it, or rename the skill")
        elif st.get("type") == "link":
            detail = "link points to " + st.get("target", "")
        elif st.get("type") == "file" and backup:
            detail = "replace the file with a link (FAM saves a copy)"
        else:
            return done("failed", "check failed", f"{p} is a {st.get('type')}, not a link. FAM does not replace it")
        if is_canceled(cancel_evt):
            return done("canceled")
        if sudo:
            yn = "yes" if backup else "no"
            script = f"""set -e
p={sh_quote(p)}
mkdir -p -- "$(dirname -- "$p")"
if [ -f "$p" ] && [ ! -L "$p" ]; then
  if [ {yn} = yes ]; then cp -p -- "$p" "$p.fam-backup-$(date +%Y%m%d%H%M%S)"; else echo "$p is a file" >&2; exit 1; fi
fi
if [ -L "$p" ] || [ -f "$p" ]; then rm -f -- "$p"; fi
ln -s -- {sh_quote(target)} "$p" """
            with EXEC_SEM:
                code, out, err, _, timed_out = run_subproc(["sudo", "-n", "sh", "-c", script], timeout_ms=120000, rid=rid, cancel_evt=cancel_evt)
            if is_canceled(cancel_evt):
                return done("canceled")
            if not timed_out and code == 255:
                return done("conn_error", detail, err.decode("utf-8", "replace"))
            if timed_out or code != 0:
                return done("failed", detail, exit_err("link " + p, code, err, out, True))
        else:
            tmp_link = None
            try:
                parent = os.path.dirname(p) or "."
                os.makedirs(parent, exist_ok=True)
                existed = False
                try:
                    lst = os.lstat(p)
                    existed = True
                    if stat.S_ISREG(lst.st_mode):
                        if backup:
                            shutil.copy2(p, f"{p}.fam-backup-{time.strftime('%Y%m%d%H%M%S')}")
                        else:
                            return done("failed", detail, f"{p} is a file")
                    elif not stat.S_ISLNK(lst.st_mode):
                        return done("failed", detail, f"{p} is not a link or regular file")
                except FileNotFoundError:
                    pass
                if is_canceled(cancel_evt):
                    return done("canceled")
                if not existed:
                    os.symlink(target, p)
                else:
                    tmp_link = os.path.join(parent, f".fam-link-{os.getpid()}-{os.urandom(8).hex()}")
                    os.symlink(target, tmp_link)
                    if is_canceled(cancel_evt):
                        os.unlink(tmp_link)
                        tmp_link = None
                        return done("canceled")
                    os.replace(tmp_link, p)
                    tmp_link = None
            except Exception as e:
                if tmp_link:
                    try:
                        os.unlink(tmp_link)
                    except OSError:
                        pass
                return done("failed", detail, f"link {p}: {e}")
        st2 = do_stat({"path": p, "sudo": sudo}, rid=rid, cancel_evt=cancel_evt)
        if st2.get("err"):
            return done("failed", "check after apply failed", st2["err"])
        if not st2.get("exists") or st2.get("type") != "link" or st2.get("target") != target:
            return done("failed", detail, "the check still finds a difference after apply")
        return done("changed", detail)

    if kind == "block":
        p = op["path"]
        block = op["block"]
        begin = op["block_begin"]
        end = op["block_end"]
        top = bool(op.get("top"))
        only_if_exists = bool(op.get("only_if_exists"))
        default_mode = (int(op["mode"]) & 0o777) if op.get("mode") else 0o644
        rd = do_read({"path": p, "sudo": False}, rid=rid, cancel_evt=cancel_evt)
        if rd.get("err"):
            return done("failed", "check failed", rd["err"])
        if rd.get("not_regular"):
            return done("failed", "check failed", f"{p} exists and is not a regular file")
        exists = bool(rd.get("exists"))
        if not exists:
            if only_if_exists:
                return done("unchanged")
            cur_b = b""
            detail = "create with the FAM block"
            mode = default_mode
        else:
            cur_b = base64.b64decode(rd.get("data_b64", ""))
            want_s = apply_block_text(cur_b.decode("utf-8", "surrogateescape"), block, begin, end, top)
            want_b = want_s.encode("utf-8", "surrogateescape")
            if cur_b == want_b:
                return done("unchanged")
            detail = "update the FAM block" if begin.encode("utf-8") in cur_b else "add the FAM block"
            mode = (int(rd.get("mode", 0)) & 0o777) if rd.get("type") == "file" else default_mode
        if is_canceled(cancel_evt):
            return done("canceled")
        want_s = apply_block_text(cur_b.decode("utf-8", "surrogateescape"), block, begin, end, top)
        want_b = want_s.encode("utf-8", "surrogateescape")
        werr = write_bytes(p, want_b, mode, False, rid=rid, cancel_evt=cancel_evt)
        if werr == "canceled":
            return done("canceled")
        if werr:
            return done("failed", detail, werr)
        rd2 = do_read({"path": p, "sudo": False}, rid=rid, cancel_evt=cancel_evt)
        if rd2.get("err") or rd2.get("not_regular") or not rd2.get("exists"):
            return done("failed", "check after apply failed", rd2.get("err") or "file missing after apply")
        cur2_b = base64.b64decode(rd2.get("data_b64", ""))
        want2_b = apply_block_text(cur2_b.decode("utf-8", "surrogateescape"), block, begin, end, top).encode("utf-8", "surrogateescape")
        if cur2_b != want2_b:
            return done("failed", detail, "the check still finds a difference after apply")
        return done("changed", detail)

    if kind == "skill":
        d = op["dir"]
        marker = op["marker"]
        exp = op["expected_digest"]
        sc = do_skill_check({"dir": d, "marker": marker})
        if sc.get("err"):
            return done("failed", "check failed", sc["err"])
        if not sc.get("exists"):
            detail = "copy"
        elif not sc.get("is_dir"):
            return done("conflict", "", f"{d} exists and FAM did not make it. Remove or rename it, or rename the skill")
        elif not sc.get("managed"):
            return done("conflict", "", f"the skill folder {d} exists and FAM did not make it. Remove or rename it, or rename the skill")
        elif sc.get("digest") == exp:
            return done("unchanged")
        else:
            detail = "update"
        if is_canceled(cancel_evt):
            return done("canceled")
        sw = do_skill_write({"dir": d, "marker": marker, "tar_b64": op.get("tar_b64", "")}, cancel_evt=cancel_evt)
        if sw.get("err") == "canceled":
            return done("canceled")
        if sw.get("err"):
            return done("failed", detail, sw["err"])
        sc2 = do_skill_check({"dir": d, "marker": marker})
        if sc2.get("err"):
            return done("failed", "check after apply failed", sc2["err"])
        if not sc2.get("exists") or not sc2.get("is_dir") or not sc2.get("managed") or sc2.get("digest") != exp:
            return done("failed", detail, "the check still finds a difference after apply")
        return done("changed", detail)

    if kind == "managed_file":
        p = op["path"]
        want_sha = op["sha256"]
        prefix_b = op["managed_prefix"].encode("utf-8")
        st = do_stat({"path": p, "sudo": False}, rid=rid, cancel_evt=cancel_evt)
        if st.get("err"):
            return done("failed", "check failed", st["err"])
        backup_needed = False
        if not st.get("exists"):
            detail = "create"
        elif st.get("type") != "file":
            return done("conflict", "", f"{p} is a {st.get('type')}. FAM needs a regular file here")
        elif st.get("sha256") == want_sha:
            return done("unchanged")
        else:
            rd = do_read({"path": p, "sudo": False}, rid=rid, cancel_evt=cancel_evt)
            if rd.get("err"):
                return done("failed", "check failed", rd["err"])
            cur_b = base64.b64decode(rd.get("data_b64", ""))
            if written_by_fam(cur_b, prefix_b):
                detail = "update"
            else:
                detail = "replace a file that FAM did not write (FAM saves a copy)"
                backup_needed = True
        if is_canceled(cancel_evt):
            return done("canceled")
        apply_detail = "written"
        if backup_needed:
            try:
                bpath = f"{p}.fam-backup-{time.strftime('%Y%m%d%H%M%S')}"
                shutil.copy2(p, bpath)
                apply_detail = "written, copy saved as " + bpath
            except Exception as e:
                return done("failed", detail, f"save a copy of {p}: {e}")
        data = base64.b64decode(op.get("data_b64", ""))
        werr = write_bytes(p, data, 0o644, False, rid=rid, cancel_evt=cancel_evt)
        if werr == "canceled":
            return done("canceled")
        if werr:
            return done("failed", detail, werr)
        st2 = do_stat({"path": p, "sudo": False}, rid=rid, cancel_evt=cancel_evt)
        if st2.get("err"):
            return done("failed", "check after apply failed", st2["err"])
        if not st2.get("exists") or st2.get("type") != "file" or st2.get("sha256") != want_sha:
            return done("failed", detail, "the check still finds a difference after apply")
        if apply_detail != "written" and detail and detail not in apply_detail:
            detail = detail + ": " + apply_detail
        return done("changed", detail)

    if kind == "script":
        sudo = bool(op.get("sudo"))
        creates = op["creates"]
        prelude = op.get("prelude", "")
        timeout_ms = int(op.get("timeout_ms", 1800000))
        st = do_stat({"path": creates, "sudo": sudo}, rid=rid, cancel_evt=cancel_evt)
        if st.get("err"):
            return done("failed", "check failed", st["err"])
        if st.get("exists"):
            return done("unchanged")
        detail = op.get("creates_display", creates) + " does not exist"
        if is_canceled(cancel_evt):
            return done("canceled")
        with EXEC_SEM:
            code, out, err, _, timed_out = run_user_script(prelude, op["apply_script"], sudo, timeout_ms, rid=rid, cancel_evt=cancel_evt)
        if is_canceled(cancel_evt):
            return done("canceled")
        if timed_out:
            return done("failed", detail, "command stopped after the timeout")
        if code == 255:
            return done("conn_error", detail, err.decode("utf-8", "replace"))
        if code != 0:
            return done("failed", detail, exit_err("apply", code, err, out, sudo))
        st2 = do_stat({"path": creates, "sudo": sudo}, rid=rid, cancel_evt=cancel_evt)
        if st2.get("err"):
            return done("failed", "check after apply failed", st2["err"])
        if not st2.get("exists"):
            return done("failed", detail, "the check still finds a difference after apply: " + op.get("creates_display", creates) + " does not exist")
        return done("changed", detail)

    return done("failed", "check failed", f"unknown apply kind {kind}")

def do_apply_seq(rid, req, cancel_evt):
    for op in req.get("ops", []):
        if is_canceled(cancel_evt):
            break
        res = apply_one_op(op, rid=rid, cancel_evt=cancel_evt)
        if res["status"] == "canceled":
            break
        send({"id": rid, "item": res})
        if res["status"] in ("failed", "conflict", "already_installed", "conn_error"):
            break
        if res["status"] == "changed" and op.get("stop_after_change"):
            break
    send({"id": rid, "done": True})

HANDLERS = {
    "stat": lambda req, rid, ev: do_stat(req, rid=rid, cancel_evt=ev),
    "read": lambda req, rid, ev: do_read(req, rid=rid, cancel_evt=ev),
    "write": lambda req, rid, ev: do_write(req, rid=rid, cancel_evt=ev),
    "skill_check": lambda req, rid, ev: do_skill_check(req),
    "skill_write": lambda req, rid, ev: do_skill_write(req, cancel_evt=ev),
    "exec": lambda req, rid, ev: do_exec(req, rid=rid, cancel_evt=ev),
}

def handle(req, cancel_evt):
    rid = req.get("id")
    op = req.get("op")
    try:
        if is_canceled(cancel_evt):
            if op == "apply_seq":
                send({"id": rid, "done": True})
            return
        if op == "apply_seq":
            try:
                do_apply_seq(rid, req, cancel_evt)
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
`

var wireWorkerB64 = base64.StdEncoding.EncodeToString([]byte(wireWorkerScript))

type wireReq struct {
	ID        uint64    `json:"id"`
	Op        string    `json:"op"`
	TargetID  uint64    `json:"target_id,omitempty"`
	Path      string    `json:"path,omitempty"`
	Dir       string    `json:"dir,omitempty"`
	Marker    string    `json:"marker,omitempty"`
	Script    string    `json:"script,omitempty"`
	StdinB64  string    `json:"stdin_b64,omitempty"`
	DataB64   string    `json:"data_b64,omitempty"`
	TarB64    string    `json:"tar_b64,omitempty"`
	Mode      *uint32   `json:"mode,omitempty"`
	Sudo      bool      `json:"sudo,omitempty"`
	Verify    bool      `json:"verify,omitempty"`
	TimeoutMS int64     `json:"timeout_ms,omitempty"`
	Ops       []ApplyOp `json:"ops,omitempty"`
}

type wireResp struct {
	ID         uint64         `json:"id"`
	Err        string         `json:"err,omitempty"`
	Exists     bool           `json:"exists,omitempty"`
	IsDir      bool           `json:"is_dir,omitempty"`
	Managed    bool           `json:"managed,omitempty"`
	NotRegular bool           `json:"not_regular,omitempty"`
	Type       string         `json:"type,omitempty"`
	Target     string         `json:"target,omitempty"`
	Mode       uint32         `json:"mode,omitempty"`
	SHA256     string         `json:"sha256,omitempty"`
	Digest     string         `json:"digest,omitempty"`
	DataB64    string         `json:"data_b64,omitempty"`
	Code       int            `json:"code,omitempty"`
	StdoutB64  string         `json:"stdout_b64,omitempty"`
	StderrB64  string         `json:"stderr_b64,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`
	TimedOut   bool           `json:"timed_out,omitempty"`
	Item       *ApplyOpResult `json:"item,omitempty"`
	Done       bool           `json:"done,omitempty"`
}

type pendingCall struct {
	ch     chan wireResp
	cancel chan struct{}
	stream bool
}

type sshWire struct {
	machine string
	target  string
	cmd     *exec.Cmd
	stdin   io.WriteCloser

	sendCh  chan []byte
	quitCh  chan struct{}
	nextID  atomic.Uint64
	mu      sync.Mutex
	pending map[uint64]*pendingCall
	closed  bool
	dead    bool
	deadErr error
	done    chan struct{}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (s *SSH) bootstrapScript(factsScript, secretsRelPath string) string {
	return factsScript + "\n" +
		"echo FAM-SECRETS-BEGIN\n" +
		fmt.Sprintf("p=\"$HOME/\"%s\n", Quote(secretsRelPath)) +
		"if [ -f \"$p\" ] && [ -r \"$p\" ]; then\n" +
		"  wc -c < \"$p\"\n" +
		"  cat -- \"$p\"\n" +
		"else\n" +
		"  echo -1\n" +
		"fi\n" +
		"echo ''\n" +
		"if command -v python3 >/dev/null 2>&1; then\n" +
		fmt.Sprintf("  python3 -B -u -c \"import base64,sys; sys.exit(99) if sys.version_info<(3,6) else (sys.stdout.write('FAM-WIRE-PY3\\n'), sys.stdout.flush(), exec(base64.b64decode('%s')))\" || echo FAM-WIRE-NONE\n", wireWorkerB64) +
		"else\n" +
		"  echo FAM-WIRE-NONE\n" +
		"fi\n"
}

// Bootstrap starts the persistent SSH worker (if python3 >= 3.6 is available on the target) and
// returns the initial facts output and existing secrets file in the same first connection.
func (s *SSH) Bootstrap(ctx context.Context, factsScript, secretsRelPath string) (Result, []byte, bool, error) {
	args, err := s.baseArgs()
	if err != nil {
		return Result{}, nil, false, err
	}
	remote := "sh -c " + Quote(s.bootstrapScript(factsScript, secretsRelPath))
	args = append(args, "-T", s.Target, "--", remote)

	cmd := exec.Command("ssh", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, nil, false, fmt.Errorf("ssh stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return Result{}, nil, false, fmt.Errorf("ssh stdout pipe: %w", err)
	}
	errBuf := &lockedBuffer{}
	cmd.Stderr = errBuf
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, nil, false, fmt.Errorf("run ssh: %w", err)
	}

	bootDone := make(chan struct{})
	go func() {
		select {
		case <-bootDone:
		case <-ctx.Done():
			_ = stdin.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}()

	br := bufio.NewReaderSize(stdout, 64*1024)
	var factsBuf bytes.Buffer
	foundSecretsMarker := false
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(string(line), "\r\n")
			if trimmed == "FAM-SECRETS-BEGIN" {
				foundSecretsMarker = true
				break
			}
			factsBuf.Write(line)
		}
		if err != nil {
			break
		}
	}

	if !foundSecretsMarker {
		close(bootDone)
		_ = stdin.Close()
		waitErr := cmd.Wait()
		dur := time.Since(start)
		res := Result{Stdout: factsBuf.Bytes(), Stderr: []byte(errBuf.String()), Duration: dur}
		if waitErr != nil {
			var ee *exec.ExitError
			if errors.As(waitErr, &ee) {
				res.Code = ee.ExitCode()
			} else {
				err = fmt.Errorf("run ssh: %w", waitErr)
				s.observe(Cmd{Label: "collect facts"}, res, err)
				return res, nil, false, err
			}
		}
		if res.Code == 255 {
			ce := &ConnError{Machine: s.Machine, Target: s.Target, Stderr: string(res.Stderr)}
			s.observe(Cmd{Label: "collect facts"}, res, ce)
			return res, nil, false, ce
		}
		s.observe(Cmd{Label: "collect facts"}, res, nil)
		return res, nil, false, nil
	}

	lenLine, err := br.ReadString('\n')
	if err != nil {
		close(bootDone)
		_ = stdin.Close()
		_ = cmd.Wait()
		return s.bootstrapConnFail(errBuf.String(), time.Since(start), err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(lenLine))
	if err != nil {
		close(bootDone)
		_ = stdin.Close()
		_ = cmd.Wait()
		return Result{}, nil, false, fmt.Errorf("bootstrap secrets length: %w", err)
	}
	var secretsData []byte
	secretsExists := false
	if n >= 0 {
		secretsExists = true
		secretsData = make([]byte, n)
		if n > 0 {
			if _, err := io.ReadFull(br, secretsData); err != nil {
				close(bootDone)
				_ = stdin.Close()
				_ = cmd.Wait()
				return s.bootstrapConnFail(errBuf.String(), time.Since(start), err)
			}
		}
	}

	wireMode := ""
	waited := false
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			close(bootDone)
			_ = stdin.Close()
			waitErr := cmd.Wait()
			waited = true
			var ee *exec.ExitError
			if errors.As(waitErr, &ee) && ee.ExitCode() == 255 {
				return s.bootstrapConnFail(errBuf.String(), time.Since(start), rerr)
			}
			if ctx.Err() != nil {
				return Result{}, nil, false, ctx.Err()
			}
			// Non-255 exit (e.g. a broken python3 shim exiting 127): continue without wire.
			wireMode = "FAM-WIRE-NONE"
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "FAM-WIRE-PY3" || trimmed == "FAM-WIRE-NONE" {
			wireMode = trimmed
			break
		}
	}
	if !waited {
		close(bootDone)
	}

	dur := time.Since(start)
	factsRes := Result{Stdout: factsBuf.Bytes(), Stderr: []byte(errBuf.String()), Duration: dur}
	s.observe(Cmd{Label: "collect facts"}, factsRes, nil)
	if secretsExists {
		s.observe(Cmd{Label: "read ~/" + secretsRelPath, Secret: true}, Result{}, nil)
	}

	if wireMode == "FAM-WIRE-PY3" {
		w := &sshWire{
			machine: s.Machine,
			target:  s.Target,
			cmd:     cmd,
			stdin:   stdin,
			sendCh:  make(chan []byte, 256),
			quitCh:  make(chan struct{}),
			pending: map[uint64]*pendingCall{},
			done:    make(chan struct{}),
		}
		go w.writeLoop()
		go w.readLoop(br, errBuf)
		s.wireMu.Lock()
		old := s.wire
		s.wire = w
		s.hasPy3 = true
		s.wireMu.Unlock()
		if old != nil {
			old.close()
		}
	} else if !waited {
		_ = stdin.Close()
		_ = cmd.Wait()
	}
	return factsRes, secretsData, secretsExists, nil
}

func (s *SSH) bootstrapConnFail(stderr string, dur time.Duration, err error) (Result, []byte, bool, error) {
	ce := &ConnError{Machine: s.Machine, Target: s.Target, Stderr: stderr}
	if strings.TrimSpace(ce.Stderr) == "" && err != nil {
		ce.Stderr = err.Error()
	}
	s.observe(Cmd{Label: "collect facts"}, Result{Stderr: []byte(stderr), Code: 255, Duration: dur}, ce)
	return Result{Code: 255}, nil, false, ce
}

func (w *sshWire) writeLoop() {
	defer w.stdin.Close()
	var batch bytes.Buffer
	for {
		var first []byte
		select {
		case first = <-w.sendCh:
		case <-w.quitCh:
			_, _ = w.stdin.Write([]byte("{\"id\":0,\"op\":\"quit\"}\n"))
			return
		case <-w.done:
			return
		}
		batch.Reset()
		batch.Write(first)
		for draining := true; draining && batch.Len() < 256*1024; {
			select {
			case more := <-w.sendCh:
				batch.Write(more)
			default:
				draining = false
			}
		}
		if _, err := w.stdin.Write(batch.Bytes()); err != nil {
			return
		}
	}
}

func (w *sshWire) readLoop(br *bufio.Reader, errBuf *lockedBuffer) {
	defer close(w.done)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var resp wireResp
			if jerr := json.Unmarshal(bytes.TrimSpace(line), &resp); jerr == nil {
				w.mu.Lock()
				pc, ok := w.pending[resp.ID]
				if ok && (!pc.stream || resp.Done) {
					delete(w.pending, resp.ID)
				}
				w.mu.Unlock()
				if ok {
					select {
					case pc.ch <- resp:
					case <-pc.cancel:
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	waitErr := w.cmd.Wait()
	stderr := errBuf.String()
	var deadErr error
	w.mu.Lock()
	closed := w.closed
	w.mu.Unlock()
	if !closed {
		code := 255
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			code = ee.ExitCode()
		}
		if code == 255 || stderr != "" || waitErr != nil {
			ce := &ConnError{Machine: w.machine, Target: w.target, Stderr: stderr}
			if strings.TrimSpace(ce.Stderr) == "" && waitErr != nil {
				ce.Stderr = waitErr.Error()
			}
			deadErr = ce
		} else {
			deadErr = &ConnError{Machine: w.machine, Target: w.target, Stderr: "remote worker exited unexpectedly"}
		}
	}
	w.mu.Lock()
	w.dead = true
	w.deadErr = deadErr
	for id, pc := range w.pending {
		delete(w.pending, id)
		close(pc.ch)
	}
	w.mu.Unlock()
}

func (w *sshWire) sendCancel(id uint64) {
	payload := []byte(fmt.Sprintf("{\"id\":0,\"op\":\"cancel\",\"target_id\":%d}\n", id))
	select {
	case w.sendCh <- payload:
	case <-w.quitCh:
	case <-w.done:
	default:
	}
}

func (w *sshWire) closedErr() error {
	w.mu.Lock()
	derr := w.deadErr
	w.mu.Unlock()
	if derr == nil {
		return &ConnError{Machine: w.machine, Target: w.target, Stderr: "SSH session closed"}
	}
	return derr
}

func (w *sshWire) call(ctx context.Context, req wireReq) (wireResp, error) {
	id := w.nextID.Add(1)
	req.ID = id
	payload, err := json.Marshal(req)
	if err != nil {
		return wireResp{}, err
	}
	payload = append(payload, '\n')
	pc := &pendingCall{ch: make(chan wireResp, 1), cancel: make(chan struct{})}

	w.mu.Lock()
	if w.dead || w.closed {
		derr := w.deadErr
		w.mu.Unlock()
		if derr == nil {
			derr = &ConnError{Machine: w.machine, Target: w.target, Stderr: "SSH session closed"}
		}
		return wireResp{}, derr
	}
	w.pending[id] = pc
	w.mu.Unlock()

	cleanup := func() {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
		close(pc.cancel)
	}

	select {
	case w.sendCh <- payload:
	case <-w.quitCh:
		cleanup()
		return wireResp{}, w.closedErr()
	case <-w.done:
		cleanup()
		return wireResp{}, w.closedErr()
	case <-ctx.Done():
		cleanup()
		return wireResp{}, ctx.Err()
	}

	select {
	case resp, ok := <-pc.ch:
		if !ok {
			return wireResp{}, w.closedErr()
		}
		return resp, nil
	case <-ctx.Done():
		cleanup()
		w.sendCancel(id)
		return wireResp{}, ctx.Err()
	}
}

func (w *sshWire) callStream(ctx context.Context, req wireReq, onResp func(wireResp)) error {
	id := w.nextID.Add(1)
	req.ID = id
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	pc := &pendingCall{ch: make(chan wireResp, 64), cancel: make(chan struct{}), stream: true}

	w.mu.Lock()
	if w.dead || w.closed {
		derr := w.deadErr
		w.mu.Unlock()
		if derr == nil {
			derr = &ConnError{Machine: w.machine, Target: w.target, Stderr: "SSH session closed"}
		}
		return derr
	}
	w.pending[id] = pc
	w.mu.Unlock()

	cleanup := func() {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
		close(pc.cancel)
	}

	select {
	case w.sendCh <- payload:
	case <-w.quitCh:
		cleanup()
		return w.closedErr()
	case <-w.done:
		cleanup()
		return w.closedErr()
	case <-ctx.Done():
		cleanup()
		return ctx.Err()
	}

	for {
		select {
		case resp, ok := <-pc.ch:
			if !ok {
				return w.closedErr()
			}
			if resp.Done {
				if resp.Err != "" {
					return errors.New(resp.Err)
				}
				return nil
			}
			onResp(resp)
		case <-ctx.Done():
			cleanup()
			w.sendCancel(id)
			return ctx.Err()
		}
	}
}

func (w *sshWire) beginClose() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.quitCh)
	w.mu.Unlock()
}

func (w *sshWire) finishClose() {
	select {
	case <-w.done:
	case <-time.After(200 * time.Millisecond):
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
		<-w.done
	}
}

func (w *sshWire) close() {
	w.beginClose()
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		if w.cmd.Process != nil {
			_ = w.cmd.Process.Kill()
		}
		<-w.done
	}
}

func (s *SSH) getWire() *sshWire {
	s.wireMu.Lock()
	defer s.wireMu.Unlock()
	return s.wire
}

func (s *SSH) closeWire() {
	s.wireMu.Lock()
	w := s.wire
	s.wire = nil
	s.wireMu.Unlock()
	if w != nil {
		w.close()
	}
}

// MaxConcurrency implements ConcurrencyProvider.
func (s *SSH) MaxConcurrency() int {
	if s.getWire() != nil {
		return 64
	}
	return 8
}

// RefreshSession implements SessionRefresher. It closes the current persistent worker channel
// and opens a fresh one on the shared SSH connection so subsequent commands inherit updated
// login environment files (~/.bashrc, ~/.config/fam/env.sh, secrets.env) without stale vars.
func (s *SSH) RefreshSession(ctx context.Context) error {
	s.wireMu.Lock()
	hasPy3 := s.hasPy3
	s.wireMu.Unlock()
	if !hasPy3 {
		return nil
	}
	s.closeWire()
	args, err := s.baseArgs()
	if err != nil {
		return err
	}
	script := "if command -v python3 >/dev/null 2>&1; then\n" +
		fmt.Sprintf("  python3 -B -u -c \"import base64,sys; sys.exit(99) if sys.version_info<(3,6) else (sys.stdout.write('FAM-WIRE-PY3\\n'), sys.stdout.flush(), exec(base64.b64decode('%s')))\" || echo FAM-WIRE-NONE\n", wireWorkerB64) +
		"else\n  echo FAM-WIRE-NONE\nfi\n"
	args = append(args, "-T", s.Target, "--", "sh -c "+Quote(script))
	cmd := exec.Command("ssh", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("ssh stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("ssh stdout pipe: %w", err)
	}
	errBuf := &lockedBuffer{}
	cmd.Stderr = errBuf
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("run ssh: %w", err)
	}
	bootDone := make(chan struct{})
	go func() {
		select {
		case <-bootDone:
		case <-ctx.Done():
			_ = stdin.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		}
	}()
	br := bufio.NewReaderSize(stdout, 64*1024)
	wireMode := ""
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			close(bootDone)
			_ = stdin.Close()
			waitErr := cmd.Wait()
			var ee *exec.ExitError
			if errors.As(waitErr, &ee) && ee.ExitCode() == 255 {
				return &ConnError{Machine: s.Machine, Target: s.Target, Stderr: errBuf.String()}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Interpreter unavailable (e.g., python3 shim exiting 127): fall back to direct SSH.
			s.wireMu.Lock()
			s.hasPy3 = false
			s.wireMu.Unlock()
			return nil
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "FAM-WIRE-PY3" || trimmed == "FAM-WIRE-NONE" {
			wireMode = trimmed
			break
		}
	}
	close(bootDone)
	if wireMode == "FAM-WIRE-PY3" {
		w := &sshWire{
			machine: s.Machine,
			target:  s.Target,
			cmd:     cmd,
			stdin:   stdin,
			sendCh:  make(chan []byte, 256),
			quitCh:  make(chan struct{}),
			pending: map[uint64]*pendingCall{},
			done:    make(chan struct{}),
		}
		go w.writeLoop()
		go w.readLoop(br, errBuf)
		s.wireMu.Lock()
		old := s.wire
		s.wire = w
		s.wireMu.Unlock()
		if old != nil {
			old.close()
		}
	} else {
		_ = stdin.Close()
		_ = cmd.Wait()
		s.wireMu.Lock()
		s.hasPy3 = false
		s.wireMu.Unlock()
	}
	return nil
}

// SupportsApplySeq implements SeqApplier.
func (s *SSH) SupportsApplySeq() bool {
	return s.getWire() != nil
}

// ApplySeq executes an ordered list of ApplyOps sequentially on the persistent remote worker in
// one round trip, streaming each task result back as it completes.
func (s *SSH) ApplySeq(ctx context.Context, ops []ApplyOp, onResult func(ApplyOpResult)) error {
	w := s.getWire()
	if w == nil {
		return errors.New("persistent worker is not active")
	}
	byIdx := make(map[int]ApplyOp, len(ops))
	for _, op := range ops {
		byIdx[op.Index] = op
	}
	var connErr error
	err := w.callStream(ctx, wireReq{Op: "apply_seq", Ops: ops}, func(resp wireResp) {
		if resp.Item == nil {
			return
		}
		it := *resp.Item
		op := byIdx[it.Index]
		if it.Status == "conn_error" {
			ce := &ConnError{Machine: s.Machine, Target: s.Target, Stderr: it.Err}
			s.observe(Cmd{Label: op.Label, Sudo: op.Sudo, Secret: op.Secret}, Result{Code: 255, Duration: time.Duration(it.DurationMS) * time.Millisecond}, ce)
			if connErr == nil {
				connErr = ce
			}
			return
		}
		var opErr error
		code := 0
		if it.Err != "" {
			opErr = errors.New(it.Err)
			code = 1
		}
		s.observe(Cmd{Label: op.Label, Sudo: op.Sudo, Secret: op.Secret}, Result{Code: code, Duration: time.Duration(it.DurationMS) * time.Millisecond}, opErr)
		onResult(it)
	})
	if err != nil {
		if ctx.Err() != nil {
			s.closeWire()
		}
		return err
	}
	return connErr
}
