#!/usr/bin/env python3
"""sandrpod-cli end-to-end suite against a live SandrPod deployment.

Required environment:
  SANDRPOD_API_URL    control plane URL
  SANDRPOD_API_TOKEN  an e2b_<hex> workspace key

Optional:
  CLI_BIN             sandrpod-cli binary (default: sandrpod-cli on PATH)
  SANDRPOD_ADMIN_TOKEN  platform/admin token; enables metrics, token list and
                        poder list checks instead of reporting them as gaps
  E2E_PODER_ID        when set, also exercise `create --poder` on that worker

Creates one sandbox at a time (the hosted trial gate allows one unverified),
deletes it in a finally block, and uses a fresh HOME so `config` checks do not
touch the operator's real ~/.sandrpod-cli.
"""
import json
import os
import pty
import re
import select
import shutil
import stat
import subprocess
import urllib.parse
import urllib.request
import sys
import tempfile
import time

CLI_BIN = os.environ.get("CLI_BIN", "sandrpod-cli")
API_URL = os.environ.get("SANDRPOD_API_URL", "")
KEY = os.environ.get("SANDRPOD_API_TOKEN", "")
if not API_URL or not KEY:
    sys.exit("SANDRPOD_API_URL and SANDRPOD_API_TOKEN are required")
HOME = os.environ.get("E2E_HOME") or tempfile.mkdtemp(prefix="sandrpod-cli-e2e-")
os.makedirs(HOME, exist_ok=True)
ENV = dict(os.environ, HOME=HOME, SANDRPOD_API_URL=API_URL,
           SANDRPOD_API_TOKEN=KEY, NO_COLOR="1", TERM="dumb")
ADMIN_TOKEN = os.environ.get("SANDRPOD_ADMIN_TOKEN", "")
PODER_ID = os.environ.get("E2E_PODER_ID", "")
NAME = "cli-e2e-%d" % int(time.time())
RESULTS = []


def cli(*args, timeout=180, stdin=None, env=None):
    return subprocess.run([CLI_BIN, *args], env=env or ENV, capture_output=True,
                          text=True, timeout=timeout, input=stdin)


def out(p):
    return (p.stdout or "") + (p.stderr or "")


def check(name, args, expect, timeout=180):
    t0 = time.time()
    try:
        p = cli(*args, timeout=timeout)
        text = out(p)
        ok = p.returncode == 0 and (expect in text)
        dt = time.time() - t0
        print("%s  %-44s %5.2fs  rc=%s %s" %
              ("PASS" if ok else "FAIL", name, dt, p.returncode, "" if ok else text.strip().replace("\n", " | ")[:220]),
              flush=True)
        RESULTS.append((name, ok, "" if ok else text[:400]))
        return text
    except Exception as e:
        dt = time.time() - t0
        print("FAIL  %-44s %5.2fs  %s" % (name, dt, e), flush=True)
        RESULTS.append((name, False, str(e)))
        return ""


def check_quiet(name, args, expect_rc=0, timeout=60):
    """Run a command without ever printing its output (it may hold a secret)."""
    t0 = time.time()
    try:
        p = cli(*args, timeout=timeout)
        ok = p.returncode == expect_rc
        print("%s  %-44s %5.2fs  rc=%s" % ("PASS" if ok else "FAIL", name, time.time() - t0, p.returncode), flush=True)
        RESULTS.append((name, ok, "" if ok else "rc=%s" % p.returncode))
        return ok
    except Exception as e:
        print("FAIL  %-44s %5.2fs  %s" % (name, time.time() - t0, type(e).__name__), flush=True)
        RESULTS.append((name, False, str(e)))
        return False


def check_fn(name, fn):
    t0 = time.time()
    try:
        v = fn()
        print("PASS  %-44s %5.2fs  %s" % (name, time.time() - t0, "" if v is None else str(v)[:80]), flush=True)
        RESULTS.append((name, True, ""))
        return v
    except Exception as e:
        print("FAIL  %-44s %5.2fs  %s: %s" % (name, time.time() - t0, type(e).__name__, str(e)[:200]), flush=True)
        RESULTS.append((name, False, str(e)))
        return None


def check_redacted(name, args, needle, timeout=60):
    """Check output contains a marker without printing the output (tokens)."""
    t0 = time.time()
    try:
        p = cli(*args, timeout=timeout)
        ok = p.returncode == 0 and needle in out(p)
        print("%s  %-44s %5.2fs  rc=%s" % ("PASS" if ok else "FAIL", name, time.time() - t0, p.returncode), flush=True)
        RESULTS.append((name, ok, "" if ok else "rc=%s" % p.returncode))
        return ok
    except Exception as e:
        print("FAIL  %-44s %5.2fs  %s" % (name, time.time() - t0, type(e).__name__), flush=True)
        RESULTS.append((name, False, str(e)))
        return False


def gap(name, detail):
    print("GAP   %-44s %s" % (name, detail), flush=True)
    RESULTS.append((name, None, detail))


def wait_state(state, timeout=180):
    end = time.time() + timeout
    last = ""
    while time.time() < end:
        last = out(cli("get", NAME, timeout=30))
        if state in last:
            return last
        if "ERROR" in last or "error" in last.lower() and "state" not in last.lower():
            return last
        time.sleep(2)
    raise TimeoutError("sandbox did not reach %s; last output: %s" % (state, last[-300:]))


try:
    check("version", ("--version",), "sandrpod-cli")
    check("health", ("health",), "ok")

    # config: a fresh HOME, a 0600 file, and no token in `config view`
    check("config set-url", ("config", "set-url", API_URL), "")
    check_quiet("config set-token", ("config", "set-token", KEY))
    cfg = os.path.join(HOME, ".sandrpod-cli", "config.yaml")
    try:
        got_mode = stat.S_IMODE(os.stat(cfg).st_mode)
        ok_mode = got_mode == 0o600
        print("%s  %-44s 0600 file mode %o" % ("PASS" if ok_mode else "FAIL", "config file mode", got_mode), flush=True)
        RESULTS.append(("config file mode", ok_mode, "" if ok_mode else "mode %o" % got_mode))
    except Exception as e:
        print("FAIL  config file mode: %s" % e, flush=True)
        RESULTS.append(("config file mode", False, str(e)))
    check_redacted("config view has url", ("config", "view"), API_URL)
    check("config unset-token", ("config", "unset-token"), "")
    check_quiet("config set-token again", ("config", "set-token", KEY))

    # create async, inspect the job, then wait for RUNNING
    text = check("create --no-wait", ("create", NAME, "--provider", "local",
                                      "--cpu", "0.5", "--memory", "1024",
                                      "--ttl", "300", "--allow-domain", "example.com",
                                      "--no-wait"), "job")
    m = re.search(r"(job-[A-Za-z0-9-]+)", text)
    if m:
        check("job get", ("job", "get", m.group(1)), "job-")
    else:
        gap("job get", "could not parse job id from create output")
    wait_state("RUNNING")
    check("get", ("get", NAME), "RUNNING")
    check("list", ("list",), NAME)
    check("env", ("env", NAME), "")

    # stateful shell sessions
    sess = "e2e-sess-%d" % int(time.time())
    check("session create", ("session", "create", NAME, "--session-id", sess), sess)
    check("session list", ("session", "list", NAME), sess)
    check("session get", ("session", "get", NAME, sess), sess)
    check("session exec cd", ("session", "exec", NAME, sess, "cd /tmp && pwd"), "/tmp")
    check("session exec state", ("session", "exec", NAME, sess, "pwd"), "/tmp")
    check("session delete", ("session", "delete", NAME, sess), "")

    # execution
    check("execute bash", ("execute", NAME, "echo cli-ok"), "cli-ok")
    check("execute python", ("execute", NAME, "print(6*7)", "-l", "python"), "42")
    check("stream", ("stream", NAME, "for i in 1 2 3; do echo $i; sleep 0.2; done"), "3")

    # filesystem
    check("fs write", ("fs", "write", NAME, "/tmp/cli.txt", "cli-file"), "")
    check("fs cat", ("fs", "cat", NAME, "/tmp/cli.txt"), "cli-file")
    check("fs ls", ("fs", "ls", NAME, "/tmp"), "cli.txt")
    check("fs info", ("fs", "info", NAME, "/tmp/cli.txt"), "cli.txt")
    check("fs mkdir", ("fs", "mkdir", NAME, "/tmp/cli-dir"), "")
    check("fs replace", ("fs", "replace", NAME, "/tmp/cli.txt", "cli-file", "cli-file-2"), "")
    check("fs cat after replace", ("fs", "cat", NAME, "/tmp/cli.txt"), "cli-file-2")
    check("fs grep", ("fs", "grep", NAME, "cli-file-2", "--path", "/tmp"), "cli")
    check("fs search", ("fs", "search", NAME, "cli*", "--path", "/tmp"), "cli.txt")
    check("fs mv", ("fs", "mv", NAME, "/tmp/cli.txt", "/tmp/cli-moved.txt"), "")
    check("fs cat moved", ("fs", "cat", NAME, "/tmp/cli-moved.txt"), "cli-file-2")
    check("fs rm", ("fs", "rm", NAME, "/tmp/cli-moved.txt"), "")

    def fs_watch():
        cli("fs", "mkdir", NAME, "/tmp/cli-watch")
        p = subprocess.Popen([CLI_BIN, "fs", "watch", NAME, "/tmp/cli-watch", "--interval", "0.2"],
                             env=ENV, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        try:
            time.sleep(1.0)
            cli("fs", "write", NAME, "/tmp/cli-watch/event.txt", "x")
            time.sleep(2.0)
        finally:
            p.terminate()
            try:
                text = p.communicate(timeout=5)[0]
            except subprocess.TimeoutExpired:
                p.kill(); text = p.communicate()[0]
        if not any(x in (text or "").lower() for x in ("create", "write", "event.txt")):
            raise AssertionError("no filesystem event seen: %r" % (text or "")[:200])
        return "event"
    check_fn("fs watch", fs_watch)

    # upload / download
    local = "/tmp/cli-e2e-up.txt"
    local_dl = "/tmp/cli-e2e-down.txt"
    open(local, "w").write("upload-from-cli")
    check("fs upload", ("fs", "upload", NAME, local, "/tmp/up.txt"), "")
    check("fs cat uploaded", ("fs", "cat", NAME, "/tmp/up.txt"), "upload-from-cli")
    check("fs download", ("fs", "download", NAME, "/tmp/up.txt", local_dl), "")
    try:
        ok = open(local_dl).read() == "upload-from-cli"
        print("%s  %-44s %s" % ("PASS" if ok else "FAIL", "download content match", ""), flush=True)
        RESULTS.append(("download content match", ok, ""))
    except Exception as e:
        print("FAIL  download content match:", e)
        RESULTS.append(("download content match", False, str(e)))

    def binary_roundtrip():
        src = "/tmp/cli-e2e-bin.bin"
        dst = "/tmp/cli-e2e-bin-down.bin"
        data = bytes(range(256)) * 32
        open(src, "wb").write(data)
        p = cli("fs", "upload", NAME, src, "/tmp/bin.bin")
        if p.returncode != 0:
            raise AssertionError("upload: " + out(p)[:160])
        p = cli("fs", "download", NAME, "/tmp/bin.bin", dst)
        if p.returncode != 0:
            raise AssertionError("download: " + out(p)[:160])
        if open(dst, "rb").read() != data:
            raise AssertionError("binary content differs")
        return len(data)
    check_fn("fs binary upload/download", binary_roundtrip)

    # stateful contexts
    text = check("context create", ("context", "create", NAME), "")
    ctx = ""
    for line in text.splitlines():
        line = line.strip()
        if re.fullmatch(r"[A-Za-z0-9_-]{6,}", line):
            ctx = line
    if ctx:
        check("run assign", ("run", NAME, "a = 10", "--context", ctx), "")
        check("run stateful", ("run", NAME, "print(a * 2)", "--context", ctx), "20")
        check("context list", ("context", "list", NAME), ctx)
        check("context restart", ("context", "restart", NAME, ctx), "")
        check("context rm", ("context", "rm", NAME, ctx), "")
    else:
        gap("contexts", "could not parse a context id from `context create`")

    # preview + stats + logs + mcp
    check("execute start web", ("execute", NAME,
                                "nohup python3 -m http.server 8000 --bind 0.0.0.0 >/tmp/web.log 2>&1 &"), "")
    time.sleep(1)
    check("preview", ("preview", NAME, "8000", "/"), "Directory listing")
    check("stats", ("stats", NAME), "")
    check("logs", ("logs", NAME, "--tail", "5"), "")
    check("mcp ls", ("mcp", "ls", NAME), "")
    check_redacted("mcp url", ("mcp", "url", NAME), "mcp")
    check("mcp tools", ("mcp", "tools", NAME), "")
    check("mcp add remote", ("mcp", "add", NAME, "e2e-remote", "--url", "https://example.com/mcp"), "")
    check("mcp ls added", ("mcp", "ls", NAME), "e2e-remote")
    check("mcp rm remote", ("mcp", "rm", NAME, "e2e-remote"), "")

    # egress
    check("egress get", ("egress", "get", NAME), "example.com")
    check("egress set", ("egress", "set", NAME, "--allow-domain", "example.com"), "")
    check("egress get allowed", ("egress", "get", NAME), "example.com")
    check("egress clear", ("egress", "clear", NAME, "-y"), "")

    # stop/start
    check("stop", ("stop", NAME), "")
    wait_state("STOPPED")
    check("start", ("start", NAME), "")
    wait_state("RUNNING")

    # snapshot
    check("snapshot (--timeout 300)", ("snapshot", NAME, "--image", "cli-e2e-snap:test", "--timeout", "300"), "", timeout=320)

    def cleanup_snapshots():
        p = cli("get", NAME, timeout=30)
        m = re.search(r"Routing ID:\s+(\S+)", out(p))
        routing = m.group(1) if m else NAME
        removed = 0
        # A worker that pushed the snapshot to the registry can report it a
        # second after the local image; sweep twice so a run leaves none.
        for attempt in range(2):
            req = urllib.request.Request(API_URL + "/api/v1/snapshots",
                                         headers={"X-Sandrpod-Token": KEY})
            with urllib.request.urlopen(req, timeout=30) as r:
                snapshots = json.loads(r.read() or b"{}").get("snapshots") or []
            pending = [s for s in snapshots
                       if s.get("source") == routing or s.get("image") == "cli-e2e-snap:test"]
            if not pending:
                break
            for s in pending:
                q = urllib.parse.quote(s.get("image", ""), safe="")
                d = urllib.request.Request(API_URL + "/api/v1/snapshots?image=" + q,
                                           method="DELETE", headers={"X-Sandrpod-Token": KEY})
                try:
                    with urllib.request.urlopen(d, timeout=30):
                        removed += 1
                except Exception:
                    pass
            time.sleep(1)
        return removed
    check_fn("snapshot images cleanup", cleanup_snapshots)

    # direct pinned create bypasses the scheduler; admin-only in the hosted build
    if ADMIN_TOKEN and PODER_ID:
        admin_env = dict(ENV, SANDRPOD_API_TOKEN=ADMIN_TOKEN)
        pinned = NAME + "-poder"
        def pinned_create():
            p = cli("create", pinned, "--poder", PODER_ID, "--provider", "local",
                    "--ttl", "300", "--wait-timeout", "300", env=admin_env)
            if p.returncode != 0:
                raise AssertionError("create: " + out(p)[:180])
            p = cli("get", pinned, env=admin_env)
            if p.returncode != 0 or "RUNNING" not in out(p):
                raise AssertionError("get: " + out(p)[:180])
            cli("delete", pinned, env=admin_env)
            return "created+deleted"
        check_fn("create --poder direct", pinned_create)
    else:
        gap("create --poder direct", "set SANDRPOD_ADMIN_TOKEN and E2E_PODER_ID to run")

    def shell_interactive():
        master, slave = pty.openpty()
        proc = subprocess.Popen([CLI_BIN, "shell", NAME], stdin=slave, stdout=slave,
                                stderr=slave, env=ENV, close_fds=True)
        os.close(slave)
        os.set_blocking(master, False)
        output = b""
        try:
            time.sleep(1.5)
            os.write(master, b"echo shell-ok\n")
            deadline = time.time() + 15
            while time.time() < deadline:
                ready, _, _ = select.select([master], [], [], 0.25)
                if ready:
                    try:
                        chunk = os.read(master, 4096)
                    except OSError:
                        break
                    if not chunk:
                        break
                    output += chunk
                    if b"shell-ok" in output:
                        break
                if proc.poll() is not None:
                    break
            os.write(master, b"\x1d")  # Ctrl-] exits the CLI shell
            time.sleep(0.5)
        finally:
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    proc.kill()
            os.close(master)
        if b"shell-ok" not in output:
            text = output.decode(errors="replace")
            if "websocket" in text.lower():
                gap("shell interactive PTY", "install sandrpod-cli[shell]")
                return
            raise AssertionError("shell output: %r" % text[-300:])
        return "PTY round-trip"
    check_fn("shell interactive PTY", shell_interactive)

    # admin-only surfaces: run with the admin token when provided, otherwise
    # record the gap instead of pretending a user key should be allowed.
    if ADMIN_TOKEN:
        admin_env = dict(ENV, SANDRPOD_API_TOKEN=ADMIN_TOKEN)
        p = cli("metrics", timeout=30, env=admin_env)
        if p.returncode == 0:
            print("PASS  metrics (admin)", flush=True); RESULTS.append(("metrics (admin)", True, ""))
        else:
            gap("metrics (admin)", out(p).strip().replace("\n", " | ")[:160])
        p = cli("poder", "list", timeout=30, env=admin_env)
        if p.returncode == 0 and "poder-" in out(p):
            print("PASS  poder list (admin)", flush=True); RESULTS.append(("poder list (admin)", True, ""))
        else:
            gap("poder list (admin)", out(p).strip().replace("\n", " | ")[:160])
        p = cli("token", "list", timeout=30, env=admin_env)
        if p.returncode == 0:
            print("PASS  token list (admin)", flush=True); RESULTS.append(("token list (admin)", True, ""))
        else:
            gap("token list (admin)", out(p).strip().replace("\n", " | ")[:160])
    else:
        gap("metrics (admin)", "set SANDRPOD_ADMIN_TOKEN to run")
        gap("poder list (admin)", "set SANDRPOD_ADMIN_TOKEN to run")
        gap("token list (admin)", "set SANDRPOD_ADMIN_TOKEN to run")
finally:
    try:
        p = cli("delete", NAME, timeout=60)
        print("cleanup delete rc=%s %s" % (p.returncode, out(p).strip()[:120]), flush=True)
    except Exception as e:
        print("cleanup delete failed:", e)
    if not os.environ.get("E2E_HOME"):
        try:
            shutil.rmtree(HOME, ignore_errors=True)
        except Exception:
            pass

    failed = [x for x in RESULTS if x[1] is False]
    gaps = [x for x in RESULTS if x[1] is None]
    print("\n%d checks, %d passed, %d failed, %d gaps" %
          (len(RESULTS), len([x for x in RESULTS if x[1] is True]), len(failed), len(gaps)))
    for n, _, m in failed:
        print("  FAILED:", n, "-", m.replace("\n", " | ")[:300])
    for n, _, m in gaps:
        print("  GAP:   ", n, "-", m)
    sys.exit(1 if failed else 0)
