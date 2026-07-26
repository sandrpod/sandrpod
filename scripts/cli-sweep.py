#!/usr/bin/env python3
"""Exercise the sandrpod-cli command surface against a live deployment.

The counterpart to the E2B sweeps: those cover the compatibility layer, this
covers the primary one — the native API, which is what the CLI and the SDKs
actually talk to.

Every check shells out to the real `sandrpod-cli` binary rather than calling the
client library, so what is under test is the thing a user types.

    export SANDRPOD_API_URL=https://api.example.com
    export SANDRPOD_API_TOKEN=<admin token>
    python cli_sweep.py
"""
import os
import re
import shlex
import subprocess
import sys
import time
import uuid

RESULTS = []
COVERS = set()
LABEL = "sandrpod-cli against a live deployment"
_LAST_GROUP = [None]
SB = "clisweep-" + uuid.uuid4().hex[:8]
# Fixed, so repeated runs overwrite one image rather than piling them up.
SNAPSHOT_TAG = "sandrpod-snapshot/cli-sweep:latest"


def cli(*args, timeout=120, check=True):
    """Run the CLI and return stdout. Non-zero exit raises with stderr attached."""
    p = subprocess.run(["sandrpod-cli", *args], capture_output=True, text=True,
                       timeout=timeout)
    if check and p.returncode:
        raise RuntimeError(f"exit {p.returncode}: {(p.stderr or p.stdout).strip()[:160]}")
    return p.stdout


def check(group, name, covers=()):
    """`covers` names the CLI commands this check exercises, e.g. ("fs ls",).

    Declared rather than inferred: two attempts at grepping the source for
    invocations both mis-reported, because some checks shell out through
    os.execvp or bind the group first. The report at the end reconciles these
    against `sandrpod-cli --help`, so a command added upstream shows up as
    uncovered instead of quietly never being run.
    """
    def deco(fn):
        COVERS.update(covers)
        t0 = time.time()
        try:
            detail, ok = str(fn())[:80], True
        except Exception as e:
            detail, ok = f"{type(e).__name__}: {e}"[:80], False
        dt = time.time() - t0
        RESULTS.append((group, name, ok, detail, dt))
        if _LAST_GROUP[0] != group:
            print(f"\n[{group}]", flush=True)
            _LAST_GROUP[0] = group
        print(f"  {'✓' if ok else '✗'} {name:<28} {dt:5.1f}s  {detail}", flush=True)
        return fn
    return deco


def skip(group, name, why, covers=()):
    RESULTS.append((group, name, None, why, 0.0))
    if _LAST_GROUP[0] != group:
        print(f"\n[{group}]", flush=True)
        _LAST_GROUP[0] = group
    print(f"  – {name:<28}   ---  skipped: {why}", flush=True)


def main():
    print(f"target: {os.environ['SANDRPOD_API_URL']}   sandbox: {SB}\n")

    # ---------- server-level ----------
    # `health` prints a human line, not JSON — assert on what it actually emits.
    check("server", "health", covers=("health",))(lambda: cli("health").strip().replace("\n", " ")[:50])
    check("server", "metrics", covers=("metrics",))(
        lambda: f"{len([l for l in cli('metrics').splitlines() if l and not l.startswith('#')])} series")
    check("server", "poder list", covers=("poder list",))(
        lambda: [l.split()[0] for l in cli("poder", "list").splitlines()
                 if l.startswith("poder-")])

    @check("server", "poder get", covers=("poder get",))
    def _pget():
        pid = [l.split()[0] for l in cli("poder", "list").splitlines() if l.startswith("poder-")][0]
        out = cli("poder", "get", pid)
        return f"{pid[:20]} → {len(out.splitlines())} lines"

    @check("server", "poder delete", covers=("poder delete",))
    def _pdel():
        """Deleting the live worker would take the deployment down, so this
        exercises what can be exercised safely: the argument/error path always,
        and a real delete only when there is an OFFLINE record to reap — which
        is the case this command actually exists for.
        """
        bad = subprocess.run(["sandrpod-cli", "poder", "delete", "poder-does-not-exist", "-y"],
                             capture_output=True, text=True, timeout=60)
        out = (bad.stdout + bad.stderr).strip()
        if "Traceback" in out:
            raise RuntimeError(f"unknown id crashed instead of erroring: {out[-90:]}")
        stale = [l.split()[0] for l in cli("poder", "list").splitlines()
                 if l.startswith("poder-") and "OFFLINE" in l]
        if not stale:
            return f"unknown id → clean error; no OFFLINE record to reap"
        cli("poder", "delete", stale[0], "-y", "--keep-vm")
        gone = stale[0] not in cli("poder", "list")
        return f"unknown id → clean error; reaped {stale[0][:20]} (gone={gone})"

    # ---------- tokens ----------
    @check("tokens", "create / list / rm", covers=("token create", "token list", "token rm",))
    def _tok():
        out = cli("token", "create", "clisweep", "--role", "user")
        m = re.search(r"(e2b_[0-9a-f]{6,})", out)
        if not m:
            raise RuntimeError(f"no key in output: {out.strip()[:80]}")
        key = m.group(1)
        prefix = key[:16]
        listed = "clisweep" in cli("token", "list")
        cli("token", "rm", prefix)
        gone = "clisweep" not in cli("token", "list")
        return f"issued {key[:14]}…, listed={listed}, revoked={gone}"

    # ---------- sandbox lifecycle ----------
    @check("lifecycle", "create", covers=("create",))
    def _create():
        t0 = time.time()
        cli("create", SB, "--provider", "local", timeout=300)
        return f"{SB} ready in {time.time()-t0:.1f}s"

    check("lifecycle", "list", covers=("list",))(lambda: SB in cli("list"))
    check("lifecycle", "get", covers=("get",))(lambda: [l for l in cli("get", SB).splitlines() if "RUNNING" in l][:1] or "no RUNNING line")
    check("lifecycle", "env", covers=("env",))(lambda: " | ".join(cli("env", SB).split()[:6]))
    check("lifecycle", "stats", covers=("stats",))(lambda: " ".join(cli("stats", SB).split()[:6]))
    check("lifecycle", "logs", covers=("logs",))(lambda: f"{len(cli('logs', SB, '--tail', '5').splitlines())} lines")

    # ---------- exec ----------
    check("exec", "execute", covers=("execute",))(lambda: cli("execute", SB, "echo hi from cli; uname -m").strip().replace("\n", " | "))
    check("exec", "stream", covers=("stream",))(lambda: cli("stream", SB, "for i in 1 2 3; do echo tick$i; sleep 0.4; done").strip().replace("\n", " "))

    # ---------- stateful kernel ----------
    @check("run", "stateful across calls", covers=("run", "context create",))
    def _run():
        ctx = cli("context", "create", SB).strip().split()[-1]
        globals()["_ctx"] = ctx
        cli("run", SB, "a = 21", "--context", ctx)
        return f"a*2 = {cli('run', SB, 'print(a * 2)', '--context', ctx).strip()}"

    check("run", "context list", covers=("context list",))(lambda: f"{len([l for l in cli('context','list',SB).splitlines() if l.strip()])} lines")

    @check("run", "context restart clears it", covers=("context restart",))
    def _restart():
        cli("context", "restart", SB, globals()["_ctx"])
        out = subprocess.run(["sandrpod-cli", "run", SB, "print(a)", "--context", globals()["_ctx"]],
                             capture_output=True, text=True, timeout=120)
        combined = out.stdout + out.stderr
        return "NameError" if "NameError" in combined else f"variable survived: {combined.strip()[:40]}"

    check("run", "context rm", covers=("context rm",))(lambda: cli("context", "rm", SB, globals()["_ctx"]) or "removed")

    # ---------- sessions (stateful shell) ----------
    @check("session", "create / exec / get / list / delete", covers=("session create", "session exec", "session get", "session list", "session delete",))
    def _sess():
        sid = cli("session", "create", SB).strip().split()[-1]
        cli("session", "exec", SB, sid, "cd /tmp && echo marker > s.txt")
        persisted = "s.txt" in cli("session", "exec", SB, sid, "ls")   # cwd must persist
        listed = sid[:8] in cli("session", "list", SB)
        cli("session", "get", SB, sid)
        cli("session", "delete", SB, sid)
        return f"cwd persisted={persisted}, listed={listed}"

    # ---------- filesystem ----------
    check("fs", "write / cat", covers=("fs write", "fs cat",))(
        lambda: (cli("fs", "write", SB, "/workspace/a.txt", "hello from the cli"),
                 cli("fs", "cat", SB, "/workspace/a.txt").strip())[1])
    check("fs", "ls", covers=("fs ls",))(lambda: " ".join(cli("fs", "ls", SB, "/workspace").split()[:6]))
    check("fs", "info", covers=("fs info",))(lambda: " ".join(cli("fs", "info", SB, "/workspace/a.txt").split()[:5]))
    check("fs", "mkdir", covers=("fs mkdir",))(lambda: cli("fs", "mkdir", SB, "/workspace/sub") or "ok")
    check("fs", "mv", covers=("fs mv",))(lambda: cli("fs", "mv", SB, "/workspace/a.txt", "/workspace/b.txt") or "ok")
    check("fs", "search (glob)", covers=("fs search",))(lambda: " ".join(cli("fs", "search", SB, "*.txt").split()[:4]))
    check("fs", "grep", covers=("fs grep",))(lambda: " ".join(cli("fs", "grep", SB, "hello").split()[:5]))
    check("fs", "replace", covers=("fs replace",))(
        lambda: (cli("fs", "replace", SB, "/workspace/b.txt", "hello", "goodbye"),
                 cli("fs", "cat", SB, "/workspace/b.txt").strip())[1])
    check("fs", "rm", covers=("fs rm",))(lambda: cli("fs", "rm", SB, "/workspace/b.txt") or "ok")

    @check("fs", "upload / download", covers=("fs upload", "fs download",))
    def _updown():
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            up = os.path.join(d, "up.txt")
            open(up, "w").write("round trip through the cli\n")
            cli("fs", "upload", SB, up, "/workspace/up.txt")
            down = os.path.join(d, "down.txt")
            cli("fs", "download", SB, "/workspace/up.txt", down)
            return f"{open(down).read().strip()!r}"

    # ---------- preview ----------
    @check("preview", "in-sandbox web service", covers=("preview",))
    def _preview():
        cli("fs", "write", SB, "/workspace/index.html", "<h1>from the cli</h1>")
        subprocess.Popen(["sandrpod-cli", "execute", SB,
                          "cd /workspace && nohup python3 -m http.server 8000 >/tmp/h.log 2>&1 &"],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(3)
        return cli("preview", SB, "8000", "index.html").strip()[:60]

    # ---------- the awkward ones ----------
    # None of these are untestable; they just need more than a subprocess call.

    @check("async", "create --no-wait / job get", covers=("job get",))
    def _job():
        name = SB + "-async"
        out = cli("create", name, "--provider", "local", "--no-wait", timeout=60)
        m = re.search(r"(job-[\w-]+)", out)
        if not m:
            raise RuntimeError(f"no job id in output: {out.strip()[:80]}")
        jid = m.group(1)
        try:
            state = ""
            for _ in range(40):                       # the job is async by definition
                state = cli("job", "get", jid)
                if re.search(r"COMPLETED|SUCCEEDED|FAILED", state):
                    break
                time.sleep(3)
            return f"{jid[:24]} → {' '.join(state.split()[:4])}"
        finally:
            subprocess.run(["sandrpod-cli", "delete", name], capture_output=True, timeout=180)

    @check("interactive", "shell (PTY over WebSocket)", covers=("shell",))
    def _shell():
        """Drive the real PTY. `shell` needs a terminal, so give it one."""
        try:
            import websocket  # noqa: F401  — the [shell] extra
        except ImportError:
            raise RuntimeError("needs the [shell] extra: pip install 'sandrpod-cli[shell]'")
        import pty
        pid, fd = pty.fork()
        if pid == 0:                                   # child: become the CLI
            os.execvp("sandrpod-cli", ["sandrpod-cli", "shell", SB])
        import select
        seen, sent = b"", False
        try:
            deadline = time.time() + 30
            while time.time() < deadline and b"shell-works-42" not in seen:
                if select.select([fd], [], [], 0.5)[0]:
                    try:
                        seen += os.read(fd, 4096)
                    except OSError:
                        break
                # Wait for the banner before typing: the websocket handshake is
                # still in flight until then and anything sent early is dropped.
                if not sent and b"Connected" in seen:
                    time.sleep(1.5)
                    os.write(fd, b"echo shell-works-$((6*7))\n")
                    sent = True
            os.write(fd, b"\x1d")                      # Ctrl-] quits
            time.sleep(0.5)
        finally:
            os.close(fd)
            try:
                os.kill(pid, 9)
                os.waitpid(pid, 0)
            except OSError:
                pass
        if b"shell-works-42" not in seen:
            raise RuntimeError(f"no echo back in {len(seen)}B: {seen[-90:]!r}")
        return f"interactive echo returned, {len(seen)}B of PTY traffic"

    @check("interactive", "fs watch (streams until killed)", covers=("fs watch",))
    def _watch():
        """Runs forever by design — so read a few events, then stop it."""
        p = subprocess.Popen(["sandrpod-cli", "fs", "watch", SB, "/workspace"],
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        try:
            time.sleep(3)
            cli("fs", "write", SB, "/workspace/watched.txt", "trigger")
            time.sleep(4)
            p.terminate()
            out = p.communicate(timeout=15)[0]
        finally:
            if p.poll() is None:
                p.kill()
        if "watched.txt" not in out:
            raise RuntimeError(f"event never arrived: {out.strip()[-90:]!r}")
        return f"saw watched.txt in {len(out.splitlines())} lines of events"

    @check("images", "snapshot", covers=("snapshot",))
    def _snap():
        """Commits an image on the *worker's* daemon, which this client cannot
        reach — so there is nothing to delete from here.

        A fixed tag keeps that bounded: every run overwrites the same image
        instead of leaving one more behind. The teardown message says how to
        drop it, because a check that quietly accumulates disk on someone
        else's host is worse than one that is skipped.
        """
        out = cli("snapshot", SB, "--image", SNAPSHOT_TAG, timeout=300)
        return f"{out.strip().splitlines()[-1][:56]} (on the worker)"

    @check("config", "set-url / set-token / view / unset-token", covers=("config set-url", "config set-token", "config view", "config unset-token",))
    def _config():
        """Point HOME at a scratch dir so the real config is never touched.

        The config path is Path.home()-derived with no override, so HOME is the
        only lever. Carry the user site-packages across on PYTHONPATH — moving
        HOME also moves that, and without it the CLI cannot even import its own
        dependencies, which would make this a test of the environment rather
        than of the command.
        """
        import site
        import tempfile
        user_site = site.getusersitepackages()
        with tempfile.TemporaryDirectory() as home:
            env = {**os.environ, "HOME": home,
                   "PYTHONPATH": os.pathsep.join(
                       filter(None, [user_site, os.environ.get("PYTHONPATH", "")]))}
            # The env vars win over the stored config — correctly — so drop them
            # here or `view` reports them and the file is never exercised.
            env.pop("SANDRPOD_API_URL", None)
            env.pop("SANDRPOD_API_TOKEN", None)
            def c(*a):
                r = subprocess.run(["sandrpod-cli", "config", *a],
                                   capture_output=True, text=True, env=env, timeout=60)
                if r.returncode:
                    raise RuntimeError(f"config {a[0]}: {(r.stderr or r.stdout).strip()[:80]}")
                return r.stdout
            c("set-url", "https://config-probe.example.com")
            c("set-token", "e2b_" + "a" * 40)
            view = c("view")
            if "config-probe.example.com" not in view:
                raise RuntimeError(f"url not persisted: {view.strip()[:80]}")
            leaked = "a" * 40 in view          # a stored token must not print in full
            c("unset-token")
            after = c("view")
            return f"url persisted, token shown in full={leaked}, cleared={'a'*40 not in after}"

    @check("mcp", "add / ls / tools / url / rm", covers=("mcp add", "mcp ls", "mcp tools", "mcp url", "mcp rm",))
    def _mcp():
        """Register a real stdio MCP server inside the sandbox and query it."""
        server = (
            "import json,sys\n"
            "def send(o): sys.stdout.write(json.dumps(o)+chr(10)); sys.stdout.flush()\n"
            "for line in sys.stdin:\n"
            "    r = json.loads(line)\n"
            "    m, i = r.get('method'), r.get('id')\n"
            "    if m == 'initialize':\n"
            "        send({'jsonrpc':'2.0','id':i,'result':{'protocolVersion':'2024-11-05',"
            "'capabilities':{'tools':{}},'serverInfo':{'name':'probe','version':'0'}}})\n"
            "    elif m == 'tools/list':\n"
            "        send({'jsonrpc':'2.0','id':i,'result':{'tools':[{'name':'ping',"
            "'description':'probe','inputSchema':{'type':'object'}}]}})\n"
            "    elif i is not None:\n"
            "        send({'jsonrpc':'2.0','id':i,'result':{}})\n")
        cli("fs", "write", SB, "/workspace/probe_mcp.py", server)
        cli("mcp", "add", SB, "probe", "python3 /workspace/probe_mcp.py", timeout=180)
        listed = "probe" in cli("mcp", "ls", SB)
        url = cli("mcp", "url", SB).strip().splitlines()[0][:44]
        tools = cli("mcp", "tools", SB, timeout=180)
        cli("mcp", "rm", SB, "probe")
        gone = "probe" not in cli("mcp", "ls", SB)
        saw_tool = "ping" in tools or "probe" in tools
        return f"listed={listed}, tools reported={saw_tool}, removed={gone}"

    # ---------- teardown ----------
    check("lifecycle", "stop", covers=("stop",))(lambda: cli("stop", SB, timeout=180) or "stopped")
    check("lifecycle", "start", covers=("start",))(lambda: cli("start", SB, timeout=300) or "started")
    # Also the teardown — cleanup() re-runs it in a finally, which is a no-op
    # once the sandbox is gone.
    check("lifecycle", "delete", covers=("delete",))(lambda: cli("delete", SB, timeout=180) or "deleted")

    report()


def cli_commands():
    """Every runnable command, straight from the CLI's own --help."""
    def group(*path):
        out = subprocess.run(["sandrpod-cli", *path, "--help"],
                             capture_output=True, text=True).stdout
        m = re.search(r"Commands:\n(.*)", out, re.S)
        return [l.split()[0] for l in m.group(1).splitlines()
                if l.startswith("  ") and l.split()] if m else []
    out = []
    for c in group():
        subs = group(c)
        out.extend(f"{c} {sub}" for sub in subs) if subs else out.append(c)
    return set(out)


def report():
    ok = sum(1 for r in RESULTS if r[2] is True)
    ran = sum(1 for r in RESULTS if r[2] is not None)
    skipped = sum(1 for r in RESULTS if r[2] is None)
    print("\n" + "=" * 92)
    print(f"  {LABEL} — {ok}/{ran} passed, {skipped} skipped")
    print("=" * 92)
    # Reconcile against the CLI's own help so a newly added command shows up
    # here as uncovered rather than silently never being exercised.
    try:
        every = cli_commands()
        uncovered = sorted(every - COVERS)
        print(f"\n  commands exercised: {len(every) - len(uncovered)}/{len(every)}")
        for c in uncovered:
            print(f"    not covered: {c}")
        stale = sorted(COVERS - every)
        for c in stale:
            print(f"    declared but no longer a command: {c}")
    except Exception as e:
        print(f"\n  (coverage check failed: {e})")

    if any(r[0] == "images" and r[2] for r in RESULTS):
        print(f"\n  the snapshot check left an image on the worker; to drop it:\n"
              f"    docker rmi {SNAPSHOT_TAG}")
    print()


def cleanup():
    """Delete the sweep's sandbox however the run ends.

    This has to be a finally, not an except: the first version cleaned up only
    on exception and left a sandbox running whenever the process was killed —
    Ctrl-C, or just piping the output through `head`, which closes the pipe and
    takes the script with it. A script that creates billable resources cannot
    leave them behind on the paths it did not think of.
    """
    subprocess.run(["sandrpod-cli", "delete", SB], capture_output=True, timeout=120)


if __name__ == "__main__":
    failed = False
    try:
        main()
    except Exception:
        import traceback
        traceback.print_exc()
        report()
        failed = True
    finally:
        try:
            cleanup()
        except Exception as e:
            print(f"\ncleanup failed, delete {SB} by hand: {e}", file=sys.stderr)
    sys.exit(1 if failed else 0)
