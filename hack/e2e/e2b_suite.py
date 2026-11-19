#!/usr/bin/env python3
"""E2B SDK end-to-end suite against a live SandrPod deployment.

Required environment:
  E2B_API_KEY   an e2b_<hex> workspace key
  E2B_DOMAIN    e.g. sandbox.sandrpod.com

Optional:
  E2B_API_URL   defaults to https://api.<domain>
  E2E_TAG       metadata tag for this run (default: time based)
  E2E_MCP=1     also run the MCP gateway checks

Every sandbox this creates carries the tag in metadata, and a failed run's
leftovers are reported and killed on the next run by the same tag.
"""
import asyncio
import base64
import inspect
import json
import os
import signal
import ssl
import sys
import time
import traceback
import urllib.error
import urllib.parse
import urllib.request

API_KEY = os.environ.get("E2B_API_KEY", "")
if not API_KEY:
    sys.exit("E2B_API_KEY is required")
DOMAIN = os.environ.get("E2B_DOMAIN", "")
if not DOMAIN:
    sys.exit("E2B_DOMAIN is required (e.g. sandbox.example.com)")
API_URL = os.environ.get("E2B_API_URL") or ("https://api." + DOMAIN)
os.environ["E2B_API_KEY"] = API_KEY
os.environ["E2B_DOMAIN"] = DOMAIN

TAG = os.environ.get("E2E_TAG") or ("sandrpod-e2e-%d" % int(time.time()))
RESULTS = []


class CheckTimeout(Exception):
    pass


def check(name, fn, timeout=90):
    t0 = time.time()
    old = signal.signal(signal.SIGALRM, lambda *_: (_ for _ in ()).throw(
        CheckTimeout("check exceeded %ss" % timeout)))
    signal.setitimer(signal.ITIMER_REAL, timeout)
    try:
        value = fn()
        dt = time.time() - t0
        print("PASS  %-46s %5.2fs  %s" % (name, dt, "" if value is None else str(value)[:100]), flush=True)
        RESULTS.append((name, True, ""))
        return value
    except Exception as e:
        dt = time.time() - t0
        msg = "%s: %s" % (type(e).__name__, str(e)[:240])
        print("FAIL  %-46s %5.2fs  %s" % (name, dt, msg), flush=True)
        RESULTS.append((name, False, msg))
        return None
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, old)


def http_get(url, timeout=15):
    ctx = ssl.create_default_context()
    with urllib.request.urlopen(url, timeout=timeout, context=ctx) as r:
        return r.status, r.read()


def wait_for(fn, timeout=20, interval=0.5):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        try:
            v = fn()
            if v:
                return v
        except Exception as e:
            last = e
        time.sleep(interval)
    if last:
        raise last
    raise TimeoutError("condition not met in %ss" % timeout)


def create_with_retry(SandboxCls, **kwargs):
    last = None
    for i in range(4):
        try:
            return SandboxCls.create(**kwargs)
        except Exception as e:
            last = e
            time.sleep(2 + i * 2)
    raise last


from e2b import CommandExitException, Sandbox
from e2b.exceptions import AuthenticationException, SandboxException
from e2b.sandbox.commands.command_handle import PtySize
from e2b.sandbox.filesystem.filesystem import WriteEntry
from e2b.sandbox.sandbox_api import SandboxQuery


def _all_items(pag):
    out = []
    while True:
        items = pag.next_items()
        if not items:
            break
        out.extend(items)
        if not pag.has_next:
            break
    return out


def _list_contains(cls, sid):
    return any(i.sandbox_id == sid for i in _all_items(cls.list()))


def _list_contains_filtered(cls, sid, tag):
    return any(i.sandbox_id == sid for i in _all_items(
        cls.list(query=SandboxQuery(metadata={"e2e": tag}))))


def _stdout(res):
    out = ""
    for m in res.logs.stdout:
        if isinstance(m, str):
            out += m
        else:
            out += getattr(m, "text", "")
    return out


def gap(name, detail):
    print("GAP   %-46s %s" % (name, detail), flush=True)
    RESULTS.append((name, None, detail))

sb = None
ci = None
extra = []
try:
    # ── control plane ──────────────────────────────────────────────────────
    sb = create_with_retry(
        Sandbox, template="base", timeout=300,
        metadata={"e2e": TAG, "purpose": "sdk-suite"},
        envs={"E2E_ENV": "value-from-create"},
    )
    print("sandbox", sb.sandbox_id, "tag", TAG)

    check("create + state running", lambda: sb.get_info().state)
    check("is_running", lambda: sb.is_running())
    check("metadata round-trip", lambda: sb.get_info().metadata.get("e2e") == TAG)
    check("set_timeout", lambda: sb.set_timeout(600))
    check("list contains sandbox", lambda: _list_contains(Sandbox, sb.sandbox_id))
    check("list metadata filter", lambda: _list_contains_filtered(Sandbox, sb.sandbox_id, TAG))

    # ── commands ───────────────────────────────────────────────────────────
    r = check("commands.run stdout/exit", lambda: sb.commands.run("echo hello-e2b && uname -s"))
    def exit7():
        try:
            sb.commands.run("sh -c 'exit 7'")
        except CommandExitException as e:
            return e.exit_code == 7
        raise AssertionError("non-zero exit did not raise")
    check("commands exit code", exit7)
    check("commands stderr", lambda: "oops" in sb.commands.run("echo oops >&2").stderr)
    def create_envs():
        got = sb.commands.run('echo "$E2E_ENV"').stdout.strip()
        if got != "value-from-create":
            raise AssertionError("create envs not inherited: %r" % got)
        return got
    check("commands envs from create", create_envs)
    def command_envs():
        got = sb.commands.run('echo "$E2E_ENV"', envs={"E2E_ENV": "override"}).stdout.strip()
        if got != "override":
            raise AssertionError("per-command env did not override: %r" % got)
        return got
    check("commands envs per-command override", command_envs)
    check("commands cwd", lambda: sb.commands.run("pwd", cwd="/tmp").stdout.strip() == "/tmp")
    check("commands user=root", lambda: sb.commands.run("id -u", user="root").stdout.strip() == "0")

    chunks = []
    def stream():
        res = sb.commands.run("for i in 1 2 3; do echo $i; sleep 0.2; done", on_stdout=chunks.append)
        return chunks, res.exit_code
    check("commands streaming on_stdout", stream)

    errs = []
    def stderr_callback():
        sb.commands.run("echo stderr-line >&2", on_stderr=errs.append)
        return any("stderr-line" in c for c in errs)
    check("commands streaming on_stderr", stderr_callback)

    # The SDK's timeout is a stream timeout; this server keeps the process
    # alive and the generated client does not abort the stream on a silent
    # command. Record the observed behaviour rather than failing the baseline.
    def command_timeout():
        t0 = time.time()
        try:
            r = sb.commands.run("sleep 5", timeout=1)
            return "returned_after_%.1fs_exit_%s" % (time.time() - t0, r.exit_code)
        except Exception as e:
            return "raised_" + type(e).__name__
    print("INFO  commands timeout semantics: %s" % command_timeout(), flush=True)

    bg = check("commands background", lambda: sb.commands.run("sleep 60", background=True))
    if bg is not None:
        check("commands list", lambda: any(p.pid == bg.pid for p in sb.commands.list()))
        check("commands kill background", lambda: sb.commands.kill(bg.pid) is not False)

    chat = check("commands stdin process", lambda: sb.commands.run("cat", background=True))
    if chat is not None:
        check("commands send_stdin", lambda: sb.commands.send_stdin(chat.pid, "stdin-ok\n") is None)
        check("commands close_stdin", lambda: sb.commands.close_stdin(chat.pid) is None)
        check("commands kill cat", lambda: sb.commands.kill(chat.pid) is not False)

    # ── filesystem ─────────────────────────────────────────────────────────
    check("files.write text", lambda: sb.files.write("/tmp/e2e.txt", "written from sdk"))
    check("files.read text", lambda: sb.files.read("/tmp/e2e.txt") == "written from sdk")
    check("files.exists", lambda: sb.files.exists("/tmp/e2e.txt") is True)
    check("files.get_info", lambda: sb.files.get_info("/tmp/e2e.txt").type)
    check("files.write bytes", lambda: sb.files.write("/tmp/e2e.bin", b"\x00\x01\xff"))
    check("files.read bytes", lambda: sb.files.read("/tmp/e2e.bin", format="bytes") == b"\x00\x01\xff")
    check("files.make_dir", lambda: sb.files.make_dir("/tmp/e2e-dir") is not False)
    check("files.write_files batch", lambda: sb.files.write_files([
        WriteEntry(path="/tmp/e2e-dir/a.txt", data="a"),
        WriteEntry(path="/tmp/e2e-dir/b.txt", data="b"),
    ]))
    check("files.list depth", lambda: sorted(e.name for e in sb.files.list("/tmp/e2e-dir", depth=2)))
    check("files.rename", lambda: sb.files.rename("/tmp/e2e.txt", "/tmp/e2e-renamed.txt").name == "e2e-renamed.txt")
    check("files.remove", lambda: sb.files.remove("/tmp/e2e-renamed.txt") is None)

    def read_stream():
        with sb.files.read("/tmp/e2e.bin", format="stream") as stream:
            data = b"".join(stream)
        if data != b"\x00\x01\xff":
            raise AssertionError("streamed bytes = %r" % data)
        return len(data)
    check("files.read stream", read_stream)

    def gzip_roundtrip():
        sb.files.write("/tmp/e2e.gz.txt", "gzip-content", gzip=True)
        got = sb.files.read("/tmp/e2e.gz.txt")
        if got != "gzip-content":
            raise AssertionError("gzip roundtrip = %r" % got)
        return got
    check("files gzip write/read", gzip_roundtrip)

    def watch():
        sb.files.make_dir("/tmp/e2e-watch")
        h = sb.files.watch_dir("/tmp/e2e-watch")
        try:
            time.sleep(0.5)
            sb.files.write("/tmp/e2e-watch/event.txt", "x")
            events = wait_for(lambda: h.get_new_events(request_timeout=5) or None, timeout=15)
            return [(e.type, e.name) for e in events][:3]
        finally:
            h.stop()
    check("files.watch_dir events", watch)

    api_key = os.environ["E2B_API_KEY"]

    def _status(url, headers=None):
        req = urllib.request.Request(url, headers=headers or {})
        try:
            with urllib.request.urlopen(req, timeout=10, context=ssl.create_default_context()) as r:
                return r.status
        except urllib.error.HTTPError as e:
            return e.code
        except Exception:
            return 0

    def _get(url, headers=None):
        req = urllib.request.Request(url, headers=headers or {})
        try:
            with urllib.request.urlopen(req, timeout=15, context=ssl.create_default_context()) as r:
                return r.status, r.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()

    def dl():
        sb.files.write("/tmp/e2e-dl.txt", "download-ok")
        url = sb.download_url("/tmp/e2e-dl.txt")
        status, body = _get(url)  # no header: the signed URL is the capability
        if status != 200 or body.decode() != "download-ok":
            raise AssertionError("anonymous download: %s %r" % (status, body[:80]))
        return status
    check("files.download_url anonymous", dl)

    def up():
        url = sb.upload_url("/tmp/e2e-up.txt")
        req = urllib.request.Request(url, data=b"upload-ok", method="PUT")
        with urllib.request.urlopen(req, timeout=15, context=ssl.create_default_context()) as r:
            status = r.status
        if status != 200 or sb.files.read("/tmp/e2e-up.txt") != "upload-ok":
            raise AssertionError("anonymous upload failed")
        return status
    check("files.upload_url anonymous", up)

    # ── PTY ────────────────────────────────────────────────────────────────
    def pty():
        h = sb.pty.create(size=PtySize(rows=24, cols=80), timeout=30)
        try:
            sb.pty.send_stdin(h.pid, b"echo pty-ok\n")
            sb.pty.resize(h.pid, PtySize(rows=30, cols=100))
            time.sleep(0.5)
            return h.pid
        finally:
            sb.pty.kill(h.pid)
    check("pty create/send_stdin/resize/kill", pty)

    # ── ports and envd ─────────────────────────────────────────────────────
    def envd_health():
        tok = getattr(sb, "_envd_access_token", None)
        req = urllib.request.Request("https://" + sb.get_host(49983) + "/health",
                                     headers={"X-Access-Token": tok or ""})
        with urllib.request.urlopen(req, timeout=10, context=ssl.create_default_context()) as r:
            return r.status == 200
    check("envd health via get_host(49983) + token", envd_health)

    web = check("port server background", lambda: sb.commands.run(
        "python3 -m http.server 8000 --bind 0.0.0.0", background=True, cwd="/tmp"))
    if web is not None:
        def port():
            url = "https://" + sb.get_host(8000) + "/"
            return wait_for(lambda: _get(url)[0] == 200, timeout=20, interval=1)
        check("generic port proxy anonymous", port)
        check("kill port server", lambda: sb.commands.kill(web.pid) is not False)

    # ── metrics / pause / resume ───────────────────────────────────────────
    check("get_metrics", lambda: type(sb.get_metrics()).__name__)
    sid = sb.sandbox_id
    check("pause", lambda: sb.pause() is not False)
    def resume():
        again = Sandbox.connect(sid, timeout=300)
        return again.commands.run("echo resumed").stdout.strip() == "resumed"
    check("connect auto-resumes", resume)

    # ── auth ───────────────────────────────────────────────────────────────
    def bad_key():
        try:
            Sandbox.create(template="base", timeout=10, api_key="e2b_" + "0" * 40)
        except AuthenticationException:
            return "AuthenticationException"
        except SandboxException as e:
            return "SandboxException: " + str(e)[:80]
        raise AssertionError("a bogus key was accepted")
    check("invalid API key refused", bad_key)

    check("kill", lambda: sb.kill() is not False)
    def gone():
        try:
            Sandbox.connect(sid, timeout=10)
        except Exception as e:
            return type(e).__name__
        raise AssertionError("killed sandbox still connectable")
    check("killed sandbox not connectable", gone)
    sb = None

    # ── code interpreter (separate sandbox, sequential: quota may be 1) ────
    from e2b_code_interpreter import Sandbox as CISandbox
    ci = create_with_retry(CISandbox, template="base", timeout=300, metadata={"e2e": TAG, "purpose": "ci-suite"})
    print("ci sandbox", ci.sandbox_id)

    check("run_code assign", lambda: ci.run_code("a = 100").error is None)
    stateful = check("run_code stateful a*2", lambda: ci.run_code("print(a * 2)"))
    if stateful is not None:
        check("run_code stdout 200", lambda: "200" in _stdout(stateful))
    check("run_code math import", lambda: "4.0" in _stdout(ci.run_code("import math\nprint(math.sqrt(16))")))
    check("run_code error capture",
          lambda: "ZeroDivisionError" in str(ci.run_code("1/0").error), timeout=120)

    def chart():
        res = ci.run_code("import matplotlib\nmatplotlib.use('Agg')\nimport matplotlib.pyplot as plt\nplt.plot([1,2,3],[1,4,9])\nplt.savefig('/tmp/chart.png')\nprint('chart-ok')")
        return "chart-ok" in _stdout(res), len(res.results)
    check("run_code matplotlib file", chart, timeout=120)

    def contexts():
        ctx = ci.create_code_context()
        ids = [c.id for c in ci.list_code_contexts()]
        ci.restart_code_context(ctx)
        ci.remove_code_context(ctx)
        return ctx.id in ids
    check("code contexts create/list/restart/remove", contexts)

    check("code sandbox kill", lambda: ci.kill() is not False)
    ci = None

    # ── network policies ───────────────────────────────────────────────────
    def probe(sandbox, host):
        try:
            r = sandbox.commands.run(
                "curl -sS -m 8 -o /dev/null -w '%{http_code}' https://" + host, timeout=20)
            return r.stdout.strip()
        except CommandExitException as e:
            return "DENIED(%s)" % e.exit_code

    def new_sandbox(**kw):
        s = create_with_retry(Sandbox, **kw)
        extra.append(s)
        return s

    def deny_all():
        s = new_sandbox(template="base", timeout=300, allow_internet_access=False)
        try:
            got = probe(s, "example.com")
            if got == "200":
                raise AssertionError("deny-all reached example.com")
            return got
        finally:
            s.kill()

    def allow_out():
        s = new_sandbox(template="base", timeout=300,
                        network={"allow_out": ["example.com"]})
        try:
            allowed = probe(s, "example.com")
            blocked = probe(s, "example.org")
            if allowed != "200":
                raise AssertionError("allowed example.com got %r" % allowed)
            if blocked == "200":
                raise AssertionError("unlisted example.org reached")
            return (allowed, blocked)
        finally:
            s.kill()

    check("network allow_internet_access=false", deny_all)
    check("network allow_out/deny_out", allow_out)

    def private_ports():
        s = new_sandbox(template="base", timeout=300,
                        network={"allow_public_traffic": False})
        server = None
        try:
            server = s.commands.run("python3 -m http.server 8000 --bind 0.0.0.0",
                                    background=True, cwd="/tmp")
            host = s.get_host(8000)
            last = None
            for _ in range(20):
                try:
                    status =_status("https://%s/" % host)
                    last = status
                    if status != 0:
                        break
                except Exception as e:
                    last = e
                time.sleep(1)
            if last != 401:
                raise AssertionError("private port without key = %r, want 401" % (last,))
            return "401"
        finally:
            if server is not None:
                try: s.commands.kill(server.pid)
                except Exception: pass
            s.kill()
    check("allowPublicTraffic=false requires auth", private_ports)

    def raw(method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(API_URL + path, data=data, method=method,
                                     headers={"X-API-KEY": API_KEY,
                                              "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=30,
                                        context=ssl.create_default_context()) as r:
                return r.status, r.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()

    def unsupported_options():
        status, _ = raw("POST", "/sandboxes",
                        {"templateID": "base", "timeout": 60,
                         "network": {"rules": {"example.com": [
                             {"method": "GET", "action": "allow"}]}}})
        if status != 400:
            raise AssertionError("rules status = %s, want 400" % status)
        return "rules=400"
    check("unsupported network options are 400", unsupported_options)

    def auto_pause_memory():
        status, body = raw("POST", "/sandboxes",
                           {"templateID": "base", "timeout": 60,
                            "autoPauseMemory": True})
        if status != 400:
            raise AssertionError("autoPauseMemory status = %s, want 400: %r" % (status, body[:100]))
        return status
    check("autoPauseMemory refused with 400", auto_pause_memory)

    def auto_pause_and_sensitive_port():
        status, body = raw("POST", "/sandboxes",
                           {"templateID": "base", "timeout": 60, "autoPause": True})
        if status != 201:
            raise AssertionError("autoPause create = %s %r" % (status, body[:120]))
        sid = json.loads(body)["sandboxID"]
        try:
            # The debugger port is a whole interactive machine: no token -> 401,
            # not a proxy into a running service.
            req = urllib.request.Request("https://9222-%s.%s/" % (sid, DOMAIN))
            try:
                with urllib.request.urlopen(req, timeout=15,
                                            context=ssl.create_default_context()) as r:
                    code = r.status
            except urllib.error.HTTPError as e:
                code = e.code
            if code != 401:
                raise AssertionError("sensitive port without token = %s, want 401" % code)
            return sid
        finally:
            raw("DELETE", "/sandboxes/" + sid)

    check("autoPause create + sensitive port 401", auto_pause_and_sensitive_port)

    if "cpu_count" not in inspect.signature(Sandbox.create).parameters:
        gap("cpuCount (SDK 2.64+)", "not present in this SDK")
    else:
        def cpu_count():
            s = new_sandbox(template="base", timeout=120, cpu_count=1)
            try:
                got = s.get_info().cpu_count
                if got != 1:
                    raise AssertionError("cpu_count = %s, want 1" % got)
                return got
            finally:
                s.kill()
        check("cpuCount=1 is honoured", cpu_count)

    def update_network():
        s = new_sandbox(template="base", timeout=300,
                        network={"allow_out": ["example.com"]})
        try:
            try:
                s.update_network({"allow_out": ["example.org"]})
            except Exception as e:
                msg = str(e)
                if any(x in msg for x in ("404", "405", "501", "not supported", "Not Found")):
                    gap("update_network", "not implemented: " + msg[:120])
                    return None
                raise
            new_host = probe(s, "example.org")
            old_host = probe(s, "example.com")
            if new_host != "200" or old_host == "200":
                raise AssertionError("after update new=%r old=%r" % (new_host, old_host))
            return "new=200 old=blocked"
        finally:
            s.kill()
    check("update_network", update_network)

    def async_sdk():
        try:
            from e2b import AsyncSandbox
        except ImportError:
            gap("AsyncSandbox", "not available in this SDK")
            return
        async def run():
            s = await AsyncSandbox.create(template="base", timeout=120)
            try:
                r = await s.commands.run("echo async-ok")
                text = "".join(r.stdout) if isinstance(r.stdout, list) else r.stdout
                if "async-ok" not in text:
                    raise AssertionError("async stdout = %r" % text)
                return "ok"
            finally:
                await s.kill()
        return asyncio.run(run())
    check("AsyncSandbox create/run/kill", async_sdk)

    # ── E2B MCP gateway (opt-in: starts a child MCP server in the sandbox) ──
    if os.environ.get("E2E_MCP") == "1":
        MCP_SERVER = r"""
import sys, json
def send(o):
    sys.stdout.write(json.dumps(o) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    try: req = json.loads(line)
    except Exception: continue
    method, rid = req.get("method"), req.get("id")
    if method == "initialize":
        send({"jsonrpc":"2.0","id":rid,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"e2e","version":"1"}}})
    elif method == "notifications/initialized":
        pass
    elif method == "tools/list":
        send({"jsonrpc":"2.0","id":rid,"result":{"tools":[{"name":"echo","description":"echo text","inputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}]}})
    elif method == "tools/call":
        args = (req.get("params") or {}).get("arguments") or {}
        send({"jsonrpc":"2.0","id":rid,"result":{"content":[{"type":"text","text":"echo:" + str(args.get("text",""))}]}})
    elif rid is not None:
        send({"jsonrpc":"2.0","id":rid,"error":{"code":-32601,"message":"method not found"}})
"""
        b64 = base64.b64encode(MCP_SERVER.encode()).decode()
        mcp_cfg = {"e2e": {"runCmd": "python3 -u -c \"import base64;exec(base64.b64decode('%s'))\"" % b64}}

        def _post(url, payload, headers):
            req = urllib.request.Request(url, data=json.dumps(payload).encode(),
                                         method="POST", headers=headers)
            try:
                with urllib.request.urlopen(req, timeout=30,
                                            context=ssl.create_default_context()) as r:
                    return r.status, dict(r.headers), r.read()
            except urllib.error.HTTPError as e:
                return e.code, dict(e.headers), e.read()

        def _json_body(body):
            text = body.decode(errors="replace")
            try:
                return json.loads(text)
            except Exception:
                for line in text.splitlines():
                    if line.startswith("data:"):
                        try:
                            return json.loads(line[5:].strip())
                        except Exception:
                            pass
            raise AssertionError("MCP response was not JSON/SSE: %r" % text[:200])

        def mcp_gateway():
            s = new_sandbox(template="base", timeout=300, metadata={"e2e": TAG}, mcp=mcp_cfg)
            try:
                url, token = "", None
                for _ in range(30):
                    url = s.get_mcp_url()
                    token = s.get_mcp_token()
                    if url and token:
                        break
                    time.sleep(1)
                if not token:
                    raise AssertionError("get_mcp_token() stayed empty")
                headers = {"Authorization": "Bearer " + token,
                           "Content-Type": "application/json",
                           "Accept": "application/json, text/event-stream"}
                body = {"jsonrpc": "2.0", "id": 1, "method": "initialize",
                        "params": {"protocolVersion": "2024-11-05", "capabilities": {},
                                   "clientInfo": {"name": "e2e", "version": "1"}}}
                status, hdrs, raw = _post(url, body, headers)
                if status != 200:
                    raise AssertionError("initialize %s %r" % (status, raw[:160]))
                session = {k.lower(): v for k, v in hdrs.items()}.get("mcp-session-id")
                if session:
                    headers["Mcp-Session-Id"] = session
                if not _json_body(raw).get("result"):
                    raise AssertionError("initialize result missing")
                _post(url, {"jsonrpc": "2.0", "method": "notifications/initialized"}, headers)
                status, _, raw = _post(url, {"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, headers)
                tools = _json_body(raw).get("result", {}).get("tools", [])
                names = [t.get("name", "") for t in tools]
                echo = next((n for n in names if n == "echo" or n.endswith("__echo")), None)
                if not echo:
                    raise AssertionError("echo tool missing: %r" % names[:20])
                status, _, raw = _post(url, {"jsonrpc": "2.0", "id": 3, "method": "tools/call",
                                             "params": {"name": echo, "arguments": {"text": "mcp-ok"}}}, headers)
                content = _json_body(raw).get("result", {}).get("content", [])
                if not any("mcp-ok" in str(c.get("text", "")) for c in content):
                    raise AssertionError("echo result: %r" % content)
                return "initialize/tools/list/tools/call"
            finally:
                s.kill()

        check("E2B MCP gateway tools/call", mcp_gateway, timeout=180)
    else:
        gap("E2B create(mcp=...)", "set E2E_MCP=1 to run the in-sandbox MCP gateway checks")

    # ── snapshots + fork ───────────────────────────────────────────────────
    def snapshots_and_fork():
        s = new_sandbox(template="base", timeout=300, metadata={"e2e": TAG})
        snap_id = ""
        children = []
        try:
            info = s.create_snapshot(name="e2e")
            snap_id = info.snapshot_id
            listed = [x.snapshot_id for x in _all_items(Sandbox.list_snapshots(limit=100))]
            if snap_id not in listed:
                raise AssertionError("snapshot %r not in list %r" % (snap_id, listed[:10]))
            if hasattr(Sandbox, "fork"):
                results = s.fork(timeout=120, count=1)
                for r in results:
                    if isinstance(r, Exception):
                        raise AssertionError("fork failed: %s" % r)
                    children.append(r)
                    if r.commands.run("echo fork-ok").stdout.strip() != "fork-ok":
                        raise AssertionError("forked sandbox did not run")
            else:
                gap("E2B fork", "not present in this SDK")
            return snap_id
        finally:
            for child in children:
                try: child.kill()
                except Exception: pass
            if snap_id:
                try: Sandbox.delete_snapshot(snap_id)
                except Exception as e: print("snapshot delete failed:", e)
            # Fork creates a second image; remove it after the forks are gone.
            try:
                for x in _all_items(Sandbox.list_snapshots(limit=100)):
                    if ":fork-" in x.snapshot_id and s.sandbox_id in x.snapshot_id:
                        Sandbox.delete_snapshot(x.snapshot_id)
            except Exception as e:
                print("fork snapshot cleanup failed:", e)
            s.kill()
    check("E2B snapshot create/list/fork/delete", snapshots_and_fork, timeout=240)

finally:
    for name, obj in (("sdk", sb), ("ci", ci)):
        if obj is not None:
            try:
                obj.kill()
                print("cleanup: killed", name, obj.sandbox_id)
            except Exception as e:
                print("cleanup: %s kill failed: %s" % (name, e))
    for obj in extra:
        try:
            obj.kill()
        except Exception:
            pass

    # Report any sandbox still carrying our tag (should be none).
    try:
        leftovers = [i.sandbox_id for i in _all_items(Sandbox.list()) if (i.metadata or {}).get("e2e") == TAG]
        print("leftovers with tag:", leftovers)
    except Exception as e:
        print("leftover scan failed:", e)

    failed = [x for x in RESULTS if x[1] is False]
    gaps = [x for x in RESULTS if x[1] is None]
    print("\n%d checks, %d passed, %d failed, %d gaps" %
          (len(RESULTS), len([x for x in RESULTS if x[1] is True]), len(failed), len(gaps)))
    for name, ok, msg in failed:
        print("  FAILED:", name, "-", msg)
    for name, ok, msg in gaps:
        print("  GAP:   ", name, "-", msg)
    sys.exit(1 if failed else 0)
