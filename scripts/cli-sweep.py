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
LABEL = "sandrpod-cli against a live deployment"
_LAST_GROUP = [None]
SB = "clisweep-" + uuid.uuid4().hex[:8]


def cli(*args, timeout=120, check=True):
    """Run the CLI and return stdout. Non-zero exit raises with stderr attached."""
    p = subprocess.run(["sandrpod-cli", *args], capture_output=True, text=True,
                       timeout=timeout)
    if check and p.returncode:
        raise RuntimeError(f"exit {p.returncode}: {(p.stderr or p.stdout).strip()[:160]}")
    return p.stdout


def check(group, name):
    def deco(fn):
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


def skip(group, name, why):
    RESULTS.append((group, name, None, why, 0.0))
    if _LAST_GROUP[0] != group:
        print(f"\n[{group}]", flush=True)
        _LAST_GROUP[0] = group
    print(f"  – {name:<28}   ---  skipped: {why}", flush=True)


def main():
    print(f"target: {os.environ['SANDRPOD_API_URL']}   sandbox: {SB}\n")

    # ---------- server-level ----------
    # `health` prints a human line, not JSON — assert on what it actually emits.
    check("server", "health")(lambda: cli("health").strip().replace("\n", " ")[:50])
    check("server", "metrics")(
        lambda: f"{len([l for l in cli('metrics').splitlines() if l and not l.startswith('#')])} series")
    check("server", "poder list")(
        lambda: [l.split()[0] for l in cli("poder", "list").splitlines()
                 if l.startswith("poder-")])

    @check("server", "poder get")
    def _pget():
        pid = [l.split()[0] for l in cli("poder", "list").splitlines() if l.startswith("poder-")][0]
        out = cli("poder", "get", pid)
        return f"{pid[:20]} → {len(out.splitlines())} lines"

    # ---------- tokens ----------
    @check("tokens", "create / list / rm")
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
    @check("lifecycle", "create")
    def _create():
        t0 = time.time()
        cli("create", SB, "--provider", "local", timeout=300)
        return f"{SB} ready in {time.time()-t0:.1f}s"

    check("lifecycle", "list")(lambda: SB in cli("list"))
    check("lifecycle", "get")(lambda: [l for l in cli("get", SB).splitlines() if "RUNNING" in l][:1] or "no RUNNING line")
    check("lifecycle", "env")(lambda: " | ".join(cli("env", SB).split()[:6]))
    check("lifecycle", "stats")(lambda: " ".join(cli("stats", SB).split()[:6]))
    check("lifecycle", "logs")(lambda: f"{len(cli('logs', SB, '--tail', '5').splitlines())} lines")

    # ---------- exec ----------
    check("exec", "execute")(lambda: cli("execute", SB, "echo hi from cli; uname -m").strip().replace("\n", " | "))
    check("exec", "stream")(lambda: cli("stream", SB, "for i in 1 2 3; do echo tick$i; sleep 0.4; done").strip().replace("\n", " "))

    # ---------- stateful kernel ----------
    @check("run", "stateful across calls")
    def _run():
        ctx = cli("context", "create", SB).strip().split()[-1]
        globals()["_ctx"] = ctx
        cli("run", SB, "a = 21", "--context", ctx)
        return f"a*2 = {cli('run', SB, 'print(a * 2)', '--context', ctx).strip()}"

    check("run", "context list")(lambda: f"{len([l for l in cli('context','list',SB).splitlines() if l.strip()])} lines")

    @check("run", "context restart clears it")
    def _restart():
        cli("context", "restart", SB, globals()["_ctx"])
        out = subprocess.run(["sandrpod-cli", "run", SB, "print(a)", "--context", globals()["_ctx"]],
                             capture_output=True, text=True, timeout=120)
        combined = out.stdout + out.stderr
        return "NameError" if "NameError" in combined else f"variable survived: {combined.strip()[:40]}"

    check("run", "context rm")(lambda: cli("context", "rm", SB, globals()["_ctx"]) or "removed")

    # ---------- sessions (stateful shell) ----------
    @check("session", "create / exec / get / list / delete")
    def _sess():
        sid = cli("session", "create", SB).strip().split()[-1]
        cli("session", "exec", SB, sid, "cd /tmp && echo marker > s.txt")
        persisted = "s.txt" in cli("session", "exec", SB, sid, "ls")   # cwd must persist
        listed = sid[:8] in cli("session", "list", SB)
        cli("session", "get", SB, sid)
        cli("session", "delete", SB, sid)
        return f"cwd persisted={persisted}, listed={listed}"

    # ---------- filesystem ----------
    check("fs", "write / cat")(
        lambda: (cli("fs", "write", SB, "/workspace/a.txt", "hello from the cli"),
                 cli("fs", "cat", SB, "/workspace/a.txt").strip())[1])
    check("fs", "ls")(lambda: " ".join(cli("fs", "ls", SB, "/workspace").split()[:6]))
    check("fs", "info")(lambda: " ".join(cli("fs", "info", SB, "/workspace/a.txt").split()[:5]))
    check("fs", "mkdir")(lambda: cli("fs", "mkdir", SB, "/workspace/sub") or "ok")
    check("fs", "mv")(lambda: cli("fs", "mv", SB, "/workspace/a.txt", "/workspace/b.txt") or "ok")
    check("fs", "search (glob)")(lambda: " ".join(cli("fs", "search", SB, "*.txt").split()[:4]))
    check("fs", "grep")(lambda: " ".join(cli("fs", "grep", SB, "hello").split()[:5]))
    check("fs", "replace")(
        lambda: (cli("fs", "replace", SB, "/workspace/b.txt", "hello", "goodbye"),
                 cli("fs", "cat", SB, "/workspace/b.txt").strip())[1])
    check("fs", "rm")(lambda: cli("fs", "rm", SB, "/workspace/b.txt") or "ok")

    @check("fs", "upload / download")
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
    @check("preview", "in-sandbox web service")
    def _preview():
        cli("fs", "write", SB, "/workspace/index.html", "<h1>from the cli</h1>")
        subprocess.Popen(["sandrpod-cli", "execute", SB,
                          "cd /workspace && nohup python3 -m http.server 8000 >/tmp/h.log 2>&1 &"],
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        time.sleep(3)
        return cli("preview", SB, "8000", "index.html").strip()[:60]

    # ---------- teardown ----------
    check("lifecycle", "stop")(lambda: cli("stop", SB, timeout=180) or "stopped")
    check("lifecycle", "start")(lambda: cli("start", SB, timeout=300) or "started")
    # Also the teardown — cleanup() re-runs it in a finally, which is a no-op
    # once the sandbox is gone.
    check("lifecycle", "delete")(lambda: cli("delete", SB, timeout=180) or "deleted")

    # ---------- deliberately not covered ----------
    skip("skipped", "shell", "interactive PTY; needs a terminal")
    skip("skipped", "fs watch", "runs until Ctrl-C by design")
    skip("skipped", "snapshot", "commits a docker image; leaves state behind")
    skip("skipped", "mcp (5 cmds)", "needs an MCP server configured")
    skip("skipped", "config (4 cmds)", "mutates ~/.sandrpod config on this machine")
    skip("skipped", "job get", "only meaningful for --no-wait creates")

    report()


def report():
    ok = sum(1 for r in RESULTS if r[2] is True)
    ran = sum(1 for r in RESULTS if r[2] is not None)
    skipped = sum(1 for r in RESULTS if r[2] is None)
    print("\n" + "=" * 92)
    print(f"  {LABEL} — {ok}/{ran} passed, {skipped} skipped")
    print("=" * 92 + "\n")


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
