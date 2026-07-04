# Copyright 2024 SandrPod
# CLI Main

import click
import sys
import os
import yaml
from pathlib import Path
from .client import CLIClient

# 配置文件路径
CONFIG_DIR = Path.home() / ".sandrpod-cli"
CONFIG_FILE = CONFIG_DIR / "config.yaml"

# 默认配置
DEFAULT_API_URL = "http://localhost:8080"


def load_config():
    """加载配置文件"""
    if CONFIG_FILE.exists():
        try:
            return yaml.safe_load(CONFIG_FILE.read_text()) or {}
        except Exception as e:
            click.echo(f"Warning: Failed to load config: {e}", err=True)
            return {}
    return {}


def save_config(config):
    """保存配置文件"""
    CONFIG_DIR.mkdir(parents=True, exist_ok=True)
    CONFIG_FILE.write_text(yaml.dump(config, default_flow_style=False))
    click.echo(f"Config saved to {CONFIG_FILE}")


def get_configured_api_url():
    """获取配置的 API URL，优先级：CLI参数 > 环境变量 > 配置文件"""
    # 1. 检查环境变量
    env_url = os.environ.get("SANDRPOD_API_URL")
    if env_url:
        return env_url

    # 2. 检查配置文件
    config = load_config()
    if config.get("api_url"):
        return config["api_url"]

    # 3. 返回默认值
    return DEFAULT_API_URL


def get_configured_token():
    """获取 API token，优先级：CLI参数 > 环境变量 > 配置文件"""
    env_token = os.environ.get("SANDRPOD_API_TOKEN")
    if env_token:
        return env_token
    config = load_config()
    return config.get("api_token")


def save_config_api_url(url):
    """保存 API URL 到配置文件"""
    config = load_config()
    config["api_url"] = url
    save_config(config)


def save_config_token(token):
    """保存 token 到配置文件"""
    config = load_config()
    config["api_token"] = token
    save_config(config)


@click.group()
@click.option("--api-url", default=None, help="API URL (overrides config)")
@click.option("--timeout", default=30, help="Request timeout")
@click.pass_context
def cli(ctx, api_url, timeout):
    """SandrPod CLI - Sandbox Management"""
    ctx.ensure_object(dict)
    # 如果没有传入 --api-url，使用配置的 URL
    if api_url is None:
        api_url = get_configured_api_url()
    token = get_configured_token()
    ctx.obj["client"] = CLIClient(api_url=api_url, timeout=timeout, token=token)


# ========== Config Commands ==========

@cli.group()
def config():
    """Manage CLI configuration (~/.sandrpod-cli/config.yaml)"""
    pass


@config.command(name="view")
def config_view():
    """Show current configuration"""
    url = get_configured_api_url()
    token = get_configured_token()
    click.echo(f"API URL : {url}")
    if token:
        masked = token[:4] + "****" + token[-4:] if len(token) > 8 else "****"
        click.echo(f"Token   : {masked} (set)")
    else:
        click.echo("Token   : (not set)")
    click.echo(f"Config  : {CONFIG_FILE}")


@config.command(name="set-url")
@click.argument("url")
def config_set_url(url):
    """Set the API server URL"""
    save_config_api_url(url)
    click.echo(f"API URL set to: {url}")


@config.command(name="set-token")
@click.argument("token")
def config_set_token(token):
    """Set the API token"""
    save_config_token(token)
    masked = token[:4] + "****" + token[-4:] if len(token) > 8 else "****"
    click.echo(f"Token set: {masked}")


@config.command(name="unset-token")
def config_unset_token():
    """Remove the saved API token"""
    cfg = load_config()
    cfg.pop("api_token", None)
    save_config(cfg)
    click.echo("Token removed")


# ========== Sandbox Commands ==========

@cli.command()
@click.pass_context
def list(ctx):
    """List all sandboxes"""
    client = ctx.obj["client"]
    try:
        sandboxes = client.list_sandboxes()
        if not sandboxes:
            click.echo("No sandboxes found")
            return
        click.echo(f"{'Name':<20} {'State':<12} {'Provider':<10} {'Region':<12} {'Arch':<8} {'OS Version'}")
        click.echo("-" * 85)
        for sb in sandboxes:
            arch = sb.get("arch", "-")
            os_ver = sb.get("os_version", "-")
            click.echo(
                f"{sb.get('name', 'N/A'):<20} {sb.get('state', 'N/A'):<12} "
                f"{sb.get('provider_type', 'N/A'):<10} {sb.get('region', 'N/A'):<12} "
                f"{arch:<8} {os_ver}"
            )
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@cli.command()
@click.argument("name")
@click.option("--region", default="local", help="Region")
@click.option("--provider", "--provider-type", "provider_type", default="local",
              help="Provider type: local, aws, aliyun, azure, gcp")
@click.option("--instance-type", default="", help="Instance type (optional)")
@click.option("--image", default="", help="Container image ID (optional, uses Poder default if omitted)")
@click.option("--poder", default=None, help="Target a specific Poder ID (bypasses the scheduler)")
@click.option("--ttl", default=0, help="Idle TTL in seconds; sandbox is auto-reaped after this much inactivity (0 = server default)")
@click.option("--cpu", default=0.0, help="CPU cores limit for the sandbox container (local/docker poders; 0 = unlimited)")
@click.option("--memory", default=0, help="Memory limit in MiB for the sandbox container (local/docker poders; 0 = unlimited)")
@click.option("--no-wait", is_flag=True, help="Return immediately with the job id instead of waiting for RUNNING")
@click.option("--wait-timeout", default=900, help="Seconds to wait for the sandbox to reach RUNNING (default 900)")
@click.pass_context
def create(ctx, name, region, provider_type, instance_type, image, poder, ttl, cpu, memory, no_wait, wait_timeout):
    """Create a sandbox (waits for it to reach RUNNING; cloud provisioning can take minutes)"""
    import time as _time
    client = ctx.obj["client"]
    try:
        # --poder: create directly on a specific Poder (fast, returns a sandbox
        # record, not a scheduler job).
        if poder:
            sb = client.create_sandbox_on_poder(
                poder, name, region, provider_type, instance_type, image
            )
            click.echo(f"Sandbox:  {sb.get('name', name)}")
            click.echo(f"State:    {sb.get('state', 'N/A')}")
            click.echo(f"Poder ID: {sb.get('poder_id', poder)}")
            click.echo(f"IP:       {sb.get('ip', 'N/A')}")
            return
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)

    # Scheduler path: ask for async creation (new servers return a job
    # immediately; old servers ignore the flag and block synchronously — if the
    # connection drops mid-way we just fall through to polling).
    job_id = None
    try:
        resp = client.create_sandbox(name, region, provider_type, instance_type, image, async_=True, ttl_seconds=ttl, cpu_cores=cpu, memory_mb=memory)
        job_id = resp.get("job_id")
        click.echo(f"Sandbox:  {resp.get('sandbox', {}).get('name', name)}")
        click.echo(f"Job ID:   {job_id or 'N/A'}")
        click.echo(f"Status:   {resp.get('status', 'N/A')}")
    except Exception as e:
        click.echo(f"(create request did not return cleanly: {e})", err=True)
        click.echo("Provisioning may still be running server-side; polling status...")

    if no_wait:
        click.echo(f"Tip: run `sandrpod-cli get {name}` to check state")
        return

    # Poll until RUNNING / ERROR / timeout. Tolerate early 404s (record may
    # appear a moment later on older servers).
    deadline = _time.time() + wait_timeout
    last_state = None
    while _time.time() < deadline:
        _time.sleep(5)
        try:
            state = client.get_sandbox(name).get("state")
        except Exception:
            state = None
        if state != last_state and state:
            click.echo(f"State:    {state}")
            last_state = state
        if state == "RUNNING":
            click.echo(f"Sandbox '{name}' is ready")
            return
        if state == "ERROR":
            msg = ""
            if job_id:
                try:
                    msg = client.get_job(job_id).get("error_message", "")
                except Exception:
                    pass
            click.echo(f"Error: sandbox entered ERROR state{': ' + msg if msg else ''}", err=True)
            sys.exit(1)
    click.echo(f"Error: timed out after {wait_timeout}s waiting for '{name}' (check `sandrpod-cli get {name}`)", err=True)
    sys.exit(1)


@cli.command()
@click.argument("name")
@click.pass_context
def get(ctx, name):
    """Get sandbox info"""
    client = ctx.obj["client"]
    try:
        sandbox = client.get_sandbox(name)
        click.echo(f"Name:          {sandbox.get('name')}")
        click.echo(f"State:         {sandbox.get('state')}")
        click.echo(f"Region:        {sandbox.get('region')}")
        click.echo(f"Provider Type: {sandbox.get('provider_type')}")
        click.echo(f"Instance Type: {sandbox.get('instance_type')}")
        click.echo(f"Poder ID:      {sandbox.get('poder_id')}")
        click.echo(f"IP:            {sandbox.get('ip', 'N/A')}")
        if sandbox.get("arch"):
            click.echo(f"Arch:          {sandbox.get('arch')}")
        if sandbox.get("os"):
            click.echo(f"OS:            {sandbox.get('os')}")
        if sandbox.get("os_version"):
            click.echo(f"OS Version:    {sandbox.get('os_version')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@cli.command()
@click.argument("name")
@click.pass_context
def env(ctx, name):
    """Show sandbox runtime environment info (arch, OS, shell, etc.)"""
    client = ctx.obj["client"]
    try:
        info = client.get_sandbox_env(name)
        click.echo(f"Arch:           {info.get('arch', 'N/A')}")
        click.echo(f"OS:             {info.get('os', 'N/A')}")
        click.echo(f"OS Version:     {info.get('os_version', 'N/A')}")
        click.echo(f"Kernel Version: {info.get('kernel_version', 'N/A')}")
        click.echo(f"Shell:          {info.get('shell', 'N/A')}")
        click.echo(f"Work Dir:       {info.get('work_dir', 'N/A')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@cli.command()
@click.argument("names", nargs=-1, required=True)
@click.pass_context
def delete(ctx, names):
    """Delete one or more sandboxes (space-separated names)"""
    client = ctx.obj["client"]
    failed = []
    for name in names:
        try:
            client.delete_sandbox(name)
            click.echo(f"Sandbox '{name}' deleted")
        except Exception as e:
            click.echo(f"Error deleting '{name}': {e}", err=True)
            failed.append(name)
    if failed:
        sys.exit(1)


@cli.command()
@click.argument("name")
@click.pass_context
def start(ctx, name):
    """Start a sandbox"""
    client = ctx.obj["client"]
    try:
        client.start_sandbox(name)
        click.echo(f"Sandbox '{name}' started")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@cli.command()
@click.argument("name")
@click.pass_context
def stop(ctx, name):
    """Stop a sandbox"""
    client = ctx.obj["client"]
    try:
        client.stop_sandbox(name)
        click.echo(f"Sandbox '{name}' stopped")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@cli.command()
@click.argument("name")
@click.option("--tail", default="100", help="Tail lines")
@click.pass_context
def logs(ctx, name, tail):
    """Get sandbox logs"""
    client = ctx.obj["client"]
    try:
        logs = client.get_sandbox_logs(name, tail)
        click.echo(logs)
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@cli.command()
@click.argument("name")
@click.argument("code")
@click.option("--language", "-l", default="bash", help="Language (python, node, bash)")
@click.option("--timeout", default=30, help="Timeout in seconds")
@click.pass_context
def execute(ctx, name, code, language, timeout):
    """Execute code in a sandbox (default: bash)"""
    client = ctx.obj["client"]
    try:
        result = client.execute_code(name, language, code, timeout)
        if result.get("error"):
            click.echo(f"Error: {result.get('error')}", err=True)
            sys.exit(1)
        exit_code = result.get("exit_code", 0)
        if result.get("stdout"):
            click.echo(f"{result.get('stdout')}")
        if exit_code != 0 and result.get("stderr"):
            click.echo(f"Stderr: {result.get('stderr')}", err=True)
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@cli.command()
@click.argument("name")
@click.argument("code")
@click.option("--language", "-l", default="bash", help="Language (python, node, bash)")
@click.option("--timeout", default=30, help="Timeout in seconds")
@click.pass_context
def stream(ctx, name, code, language, timeout):
    """Execute code and stream output live as it is produced"""
    client = ctx.obj["client"]
    exit_code = 0
    try:
        for ev in client.stream_execute(name, language, code, timeout):
            kind, data = ev.get("event"), ev.get("data", "")
            if kind == "exit":
                try:
                    exit_code = int(data.strip())
                except ValueError:
                    pass
            elif kind == "error":
                click.echo(f"Error: {data}", err=True)
                exit_code = exit_code or 1
            else:  # stdout / stderr
                click.echo(data, err=(kind == "stderr"))
        if exit_code != 0:
            sys.exit(exit_code)
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@cli.command()
@click.argument("name")
@click.argument("port", type=int)
@click.argument("path", default="/")
@click.pass_context
def preview(ctx, name, port, path):
    """Fetch a web service running inside the sandbox (proxied via the tunnel)"""
    client = ctx.obj["client"]
    try:
        body = client.preview(name, port, path)
        click.echo(body.decode("utf-8", errors="replace"))
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@cli.command()
@click.argument("name")
@click.pass_context
def shell(ctx, name):
    """Open an interactive shell in the sandbox (PTY over WebSocket)"""
    try:
        import websocket  # websocket-client
    except ImportError:
        click.echo("Error: interactive shell needs 'websocket-client'.\n"
                   "  pip install websocket-client   (or: pip install 'sandrpod-cli[shell]')", err=True)
        sys.exit(1)
    import threading
    import termios
    import tty

    client = ctx.obj["client"]
    url = client.pty_url(name)
    header = []
    auth = client.auth_header()
    if auth:
        header.append(f"Authorization: {auth}")

    try:
        ws = websocket.create_connection(url, header=header, enable_multithread=True)
    except Exception as e:
        click.echo(f"Error: failed to open shell for '{name}': {e}", err=True)
        sys.exit(1)

    click.echo(f"Connected to '{name}'. Press Ctrl-] to exit.")
    old_attrs = None
    if sys.stdin.isatty():
        old_attrs = termios.tcgetattr(sys.stdin)
        tty.setraw(sys.stdin.fileno())

    stop = threading.Event()

    def pump_output():
        try:
            while not stop.is_set():
                data = ws.recv()
                if data is None or data == "":
                    break
                if isinstance(data, str):
                    data = data.encode("utf-8", errors="replace")
                sys.stdout.buffer.write(data)
                sys.stdout.buffer.flush()
        except Exception:
            pass
        finally:
            stop.set()

    reader = threading.Thread(target=pump_output, daemon=True)
    reader.start()
    try:
        while not stop.is_set():
            ch = sys.stdin.read(1)
            if not ch or ch == "\x1d":  # Ctrl-]
                break
            ws.send(ch)
    except (KeyboardInterrupt, Exception):
        pass
    finally:
        stop.set()
        try:
            ws.close()
        except Exception:
            pass
        if old_attrs is not None:
            termios.tcsetattr(sys.stdin, termios.TCSADRAIN, old_attrs)
        click.echo("\nShell closed.")


@cli.command()
@click.argument("name")
@click.option("--image", default="", help="Target image ref (repo:tag); default sandrpod-snapshot/<name>:latest")
@click.pass_context
def snapshot(ctx, name, image):
    """Commit the sandbox's current state to a new image (docker commit)"""
    client = ctx.obj["client"]
    try:
        res = client.snapshot(name, image)
        click.echo(f"Snapshot image: {res.get('image', 'N/A')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


# ========== Observability & Code Interpreter ==========

@cli.command()
@click.argument("name")
@click.pass_context
def stats(ctx, name):
    """Show a sandbox's live CPU / memory / disk usage (per-sandbox, not server metrics)."""
    client = ctx.obj["client"]
    try:
        m = client.get_sandbox_stats(name)

        def gib(b):
            return f"{(b or 0) / 1024 / 1024 / 1024:.2f} GiB"

        click.echo(f"CPU:    {m.get('cpu_used_pct', 0):.1f}% of {m.get('cpu_count', 0)} cores")
        click.echo(f"Memory: {gib(m.get('mem_used'))} / {gib(m.get('mem_total'))}")
        click.echo(f"Disk:   {gib(m.get('disk_used'))} / {gib(m.get('disk_total'))}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@cli.command()
@click.argument("name")
@click.argument("code")
@click.option("--context", "context_id", default="",
              help="Stateful context id; variables persist across runs within it")
@click.pass_context
def run(ctx, name, code, context_id):
    """Run code in a STATEFUL kernel (state persists across runs in the same --context)."""
    client = ctx.obj["client"]
    try:
        res = client.run_code(name, code, context_id)
        if res.get("stdout"):
            click.echo(res["stdout"], nl=False)
        if res.get("stderr"):
            click.echo(res["stderr"], nl=False, err=True)
        if res.get("error"):
            click.echo(res["error"], err=True)
            sys.exit(1)
        # Rich results, E2B-shaped: print the main result's text (DataFrame ASCII,
        # scalar values) and save each artifact a terminal can't render inline
        # (png/jpeg/svg/html + structured chart JSON) to a file.
        import base64
        import json as _json
        results = res.get("results")
        if results:
            for n, r in enumerate(results, start=1):
                if r.get("is_main_result") and r.get("text"):
                    click.echo(r["text"])
                for key, ext, is_binary in (("png", "png", True), ("jpeg", "jpg", True),
                                            ("svg", "svg", False), ("html", "html", False)):
                    if r.get(key):
                        data = base64.b64decode(r[key]) if is_binary else r[key].encode()
                        with open(f"result-{n}.{ext}", "wb") as fh:
                            fh.write(data)
                        click.echo(f"[saved {key} → result-{n}.{ext}]")
                if r.get("chart"):
                    with open(f"result-{n}.chart.json", "w") as fh:
                        _json.dump(r["chart"], fh, indent=2)
                    click.echo(f"[chart data → result-{n}.chart.json]")
        elif res.get("text"):
            click.echo(res["text"])
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@click.group(name="context")
@click.pass_context
def code_context_group(ctx):
    """Manage stateful code-interpreter contexts (isolated namespaces for `run`)."""
    pass


@code_context_group.command(name="create")
@click.argument("name")
@click.option("--language", default="python")
@click.option("--cwd", default="")
@click.pass_context
def context_create(ctx, name, language, cwd):
    """Create a new stateful context; prints its id."""
    client = ctx.parent.parent.obj["client"]
    try:
        click.echo(client.create_code_context(name, language, cwd).get("id", ""))
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@code_context_group.command(name="list")
@click.argument("name")
@click.pass_context
def context_list(ctx, name):
    """List stateful contexts."""
    client = ctx.parent.parent.obj["client"]
    try:
        ctxs = client.list_code_contexts(name)
        if not ctxs:
            click.echo("(none)")
            return
        for c in ctxs:
            click.echo(f"{c.get('id', ''):16}  {c.get('language', '')}  {c.get('cwd', '')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@code_context_group.command(name="restart")
@click.argument("name")
@click.argument("context_id")
@click.pass_context
def context_restart(ctx, name, context_id):
    """Restart a context's kernel (clears its variables, keeps the id)."""
    client = ctx.parent.parent.obj["client"]
    try:
        client.restart_code_context(name, context_id)
        click.echo("restarted")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@code_context_group.command(name="rm")
@click.argument("name")
@click.argument("context_id")
@click.pass_context
def context_rm(ctx, name, context_id):
    """Remove a context and its kernel."""
    client = ctx.parent.parent.obj["client"]
    try:
        client.remove_code_context(name, context_id)
        click.echo("removed")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


cli.add_command(code_context_group)


# ========== File Commands ==========

@click.group(name="fs")
@click.pass_context
def fs_group(ctx):
    """File operations on sandbox"""
    pass


@fs_group.command(name="ls")
@click.argument("name")
@click.option("--path", default="", help="Directory path")
@click.pass_context
def fs_ls(ctx, name, path):
    """List directory contents"""
    client = ctx.parent.parent.obj["client"]
    try:
        result = client.list_files(name, path if path else "")
        files = result.get("files", [])
        if not files:
            click.echo("(empty)")
            return
        for f in files:
            type_str = "d" if f.get("is_dir") else "f"
            size = f.get("size", 0)
            click.echo(f"{type_str} {size:<10} {f.get('name', 'N/A')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@fs_group.command(name="cat")
@click.argument("name")
@click.argument("path")
@click.pass_context
def fs_cat(ctx, name, path):
    """Read file contents"""
    client = ctx.parent.parent.obj["client"]
    try:
        content = client.read_file(name, path)
        click.echo(content.decode("utf-8", errors="replace"))
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@fs_group.command(name="write")
@click.argument("name")
@click.argument("path")
@click.argument("content")
@click.pass_context
def fs_write(ctx, name, path, content):
    """Write file contents"""
    client = ctx.parent.parent.obj["client"]
    try:
        result = client.write_file(name, path, content)
        click.echo(f"Written: {result.get('path')} ({result.get('size')} bytes)")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@fs_group.command(name="mkdir")
@click.argument("name")
@click.argument("path")
@click.pass_context
def fs_mkdir(ctx, name, path):
    """Create directory"""
    client = ctx.parent.parent.obj["client"]
    try:
        result = client.create_folder(name, path)
        click.echo(f"Created: {result.get('path')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@fs_group.command(name="rm")
@click.argument("name")
@click.argument("path")
@click.pass_context
def fs_rm(ctx, name, path):
    """Delete file or directory"""
    client = ctx.parent.parent.obj["client"]
    try:
        result = client.delete_file(name, path)
        click.echo(f"Deleted: {result.get('path')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@fs_group.command(name="mv")
@click.argument("name")
@click.argument("source")
@click.argument("destination")
@click.pass_context
def fs_mv(ctx, name, source, destination):
    """Move/rename file or directory"""
    client = ctx.parent.parent.obj["client"]
    try:
        result = client.move_file(name, source, destination)
        click.echo(f"Moved: {result.get('source')} -> {result.get('destination')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@fs_group.command(name="search")
@click.argument("name")
@click.option("--path", default="", help="Search path")
@click.argument("pattern")
@click.pass_context
def fs_search(ctx, name, path, pattern):
    """Search files by glob pattern"""
    client = ctx.parent.parent.obj["client"]
    try:
        result = client.search_files(name, path if path else "", pattern)
        files = result.get("files", [])
        if not files:
            click.echo("(no matches)")
            return
        for f in files:
            click.echo(f)
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@fs_group.command(name="grep")
@click.argument("name")
@click.argument("pattern")
@click.option("--path", default="", help="Search path")
@click.pass_context
def fs_grep(ctx, name, pattern, path):
    """Search for pattern in files"""
    client = ctx.parent.parent.obj["client"]
    try:
        result = client.find_in_files(name, path if path else "", pattern)
        if not result:
            click.echo("(no matches)")
            return
        for m in result:
            line = m.get("line", "?")
            content = m.get("content", "").strip()
            click.echo(f"{m.get('file')}:{line}: {content}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@fs_group.command(name="replace")
@click.argument("name")
@click.argument("file")
@click.argument("pattern")
@click.argument("new_value")
@click.pass_context
def fs_replace(ctx, name, file, pattern, new_value):
    """Replace text in a file. Usage: fs replace SANDBOX FILE PATTERN NEW_VALUE"""
    client = ctx.parent.parent.obj["client"]
    try:
        result = client.replace_in_files(name, [file], pattern, new_value)
        click.echo(f"Replaced in {file}: {result}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@fs_group.command(name="info")
@click.argument("name")
@click.argument("path")
@click.pass_context
def fs_info(ctx, name, path):
    """Get file/directory info"""
    client = ctx.parent.parent.obj["client"]
    try:
        info = client.get_file_info(name, path)
        click.echo(f"Name: {info.get('name')}")
        click.echo(f"Path: {info.get('path')}")
        click.echo(f"Type: {'directory' if info.get('is_dir') else 'file'}")
        click.echo(f"Size: {info.get('size', 0)} bytes")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@fs_group.command(name="upload")
@click.argument("name")
@click.argument("local_path", type=click.Path(exists=True))
@click.argument("remote_path")
@click.pass_context
def fs_upload(ctx, name, local_path, remote_path):
    """Upload a local file to sandbox. Usage: fs upload SANDBOX LOCAL_PATH REMOTE_PATH"""
    import os
    client = ctx.parent.parent.obj["client"]
    try:
        local_path = os.path.expanduser(local_path)
        filename = os.path.basename(local_path)
        with open(local_path, "rb") as f:
            content = f.read()
        size = len(content)
        # If remote_path ends with '/', treat as directory and append filename.
        # Otherwise use remote_path as the exact destination file path.
        if remote_path.endswith("/"):
            dest_dir = remote_path.rstrip("/")
            dest_file = f"{dest_dir}/{filename}"
            result = client.upload_files(name, [(filename, content)], dest_dir)
        else:
            # remote_path is the exact file path; use its directory as upload dest
            dest_dir = os.path.dirname(remote_path)
            dest_filename = os.path.basename(remote_path)
            result = client.upload_files(name, [(dest_filename, content)], dest_dir)
            dest_file = remote_path
        click.echo(f"Uploaded: {local_path} → {dest_file} ({size} bytes)")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@fs_group.command(name="download")
@click.argument("name")
@click.argument("remote_path")
@click.argument("local_path", required=False)
@click.pass_context
def fs_download(ctx, name, remote_path, local_path):
    """Download a file from sandbox. Usage: fs download SANDBOX REMOTE_PATH [LOCAL_PATH]"""
    import os
    client = ctx.parent.parent.obj["client"]
    try:
        content = client.read_file(name, remote_path)
        # Default local path: current dir + remote filename
        if not local_path:
            local_path = os.path.basename(remote_path)
        local_path = os.path.expanduser(local_path)
        with open(local_path, "wb") as f:
            f.write(content)
        click.echo(f"Downloaded: {remote_path} → {local_path} ({len(content)} bytes)")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@fs_group.command(name="watch")
@click.argument("name")
@click.argument("path")
@click.option("--recursive", is_flag=True, help="Watch subdirectories too")
@click.option("--interval", default=1.0, help="Poll interval in seconds")
@click.pass_context
def fs_watch(ctx, name, path, recursive, interval):
    """Watch a directory and print filesystem events until Ctrl-C."""
    import time

    client = ctx.parent.parent.obj["client"]
    watcher_id = ""
    try:
        watcher_id = client.watch_create(name, path, recursive)
        click.echo(f"Watching {path} (Ctrl-C to stop)...")
        while True:
            for ev in client.watch_events(name, watcher_id):
                click.echo(f"{ev.get('type', '?'):8} {ev.get('name', '')}")
            time.sleep(interval)
    except KeyboardInterrupt:
        click.echo("\nStopped.")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)
    finally:
        if watcher_id:
            try:
                client.watch_remove(name, watcher_id)
            except Exception:
                pass


# Register file group
cli.add_command(fs_group)


# ========== Session Commands ==========

@click.group(name="session")
@click.pass_context
def session_group(ctx):
    """Session operations (stateful shell)"""
    pass


@session_group.command(name="create")
@click.argument("name")
@click.option("--session-id", default=None, help="Session ID (auto-generated if not provided)")
@click.pass_context
def session_create(ctx, name, session_id):
    """Create a new session"""
    client = ctx.obj["client"]
    try:
        session = client.create_session(name, session_id)
        click.echo(f"Session created: {session.get('session_id')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@session_group.command(name="list")
@click.argument("name")
@click.pass_context
def session_list(ctx, name):
    """List all sessions"""
    client = ctx.obj["client"]
    try:
        sessions = client.list_sessions(name)
        if not sessions:
            click.echo("No sessions found")
            return
        click.echo(f"{'Session ID':<40} {'Created At':<30}")
        click.echo("-" * 75)
        for s in sessions:
            click.echo(f"{s.get('session_id', 'N/A'):<40} {s.get('created_at', 'N/A'):<30}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@session_group.command(name="get")
@click.argument("name")
@click.argument("session_id")
@click.pass_context
def session_get(ctx, name, session_id):
    """Get session info"""
    client = ctx.obj["client"]
    try:
        session = client.get_session(name, session_id)
        click.echo(f"Session ID: {session.get('session_id')}")
        click.echo(f"Created At: {session.get('created_at')}")
        commands = session.get("commands", [])
        if commands:
            click.echo(f"Commands: {len(commands)}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@session_group.command(name="exec")
@click.argument("name")
@click.argument("session_id")
@click.argument("command")
@click.pass_context
def session_exec(ctx, name, session_id, command):
    """Execute command in session (stateful - cd/env persist)"""
    client = ctx.obj["client"]
    try:
        result = client.execute_in_session(name, session_id, command)
        if result.get("error"):
            click.echo(f"Error: {result.get('error')}", err=True)
            sys.exit(1)
        if result.get("output"):
            click.echo(result.get("output"))
        exit_code = result.get("exit_code", 0)
        if exit_code != 0:
            click.echo(f"Exit code: {exit_code}", err=True)
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@session_group.command(name="delete")
@click.argument("name")
@click.argument("session_id")
@click.pass_context
def session_delete(ctx, name, session_id):
    """Delete a session"""
    client = ctx.obj["client"]
    try:
        client.delete_session(name, session_id)
        click.echo(f"Session '{session_id}' deleted")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


# Register session group
cli.add_command(session_group)


# ========== Job Commands ==========

@cli.group()
@click.pass_context
def job(ctx):
    """Inspect async provisioning jobs (from `create --no-wait`)"""
    pass


@job.command("get")
@click.argument("job_id")
@click.pass_context
def job_get(ctx, job_id):
    """Show a job's status/result/error"""
    client = ctx.obj["client"]
    try:
        j = client.get_job(job_id)
        click.echo(f"Job:      {j.get('id', job_id)}")
        click.echo(f"Type:     {j.get('type', 'N/A')}")
        click.echo(f"Status:   {j.get('status', 'N/A')}")
        click.echo(f"Sandbox:  {j.get('sandbox_name', 'N/A')}")
        if j.get("error_message"):
            click.echo(f"Error:    {j['error_message']}")
        result = j.get("result")
        if result:
            click.echo(f"Result:   ip={result.get('ip', '-')} proxy={result.get('proxy_url', '-')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


# ========== Metrics ==========

@cli.command()
@click.pass_context
def metrics(ctx):
    """Fetch server Prometheus /metrics (needs an admin token)"""
    client = ctx.obj["client"]
    try:
        click.echo(client.metrics(), nl=False)
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


# ========== Token Commands ==========

@cli.group()
@click.pass_context
def token(ctx):
    """Manage API tokens (admin). Issued keys are e2b_<hex> — drop-in E2B_API_KEY."""
    pass


@token.command("create")
@click.argument("name")
@click.option("--role", type=click.Choice(["user", "admin"]), default="user",
              help="Token role (default: user)")
@click.pass_context
def token_create(ctx, name, role):
    """Issue a new API token. The raw key is shown ONCE."""
    client = ctx.obj["client"]
    try:
        t = client.create_token(name, role)
        key = t.get("key", "")
        click.echo(f"✓ Issued token for {click.style(t.get('name', ''), bold=True)} (role={t.get('role')})")
        click.echo()
        click.echo(f"  {click.style(key, fg='green', bold=True)}")
        click.echo()
        click.echo("  ⚠  Save it now — only the hash is stored; the key cannot be shown again.")
        click.echo(f"  Use as:  E2B_DOMAIN=<your-domain> E2B_API_KEY={key}")
        click.echo(f"  Revoke:  sandrpod-cli token rm {t.get('prefix')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@token.command("list")
@click.pass_context
def token_list(ctx):
    """List issued tokens (prefix only; raw keys are never shown)."""
    client = ctx.obj["client"]
    try:
        toks = client.list_tokens()
        if not toks:
            click.echo("No tokens issued")
            return
        click.echo(f"{'PREFIX':<20} {'NAME':<24} {'ROLE':<8} CREATED")
        click.echo("-" * 72)
        for t in toks:
            click.echo(
                f"{t.get('prefix', '-'):<20} {t.get('name', '-'):<24} "
                f"{t.get('role', '-'):<8} {t.get('created_at', '-')}"
            )
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@token.command("rm")
@click.argument("prefix")
@click.pass_context
def token_rm(ctx, prefix):
    """Revoke a token by its display prefix (e.g. e2b_1a2b3c4d5e6f)."""
    client = ctx.obj["client"]
    try:
        client.delete_token(prefix)
        click.echo(f"✓ Revoked {prefix}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


# ========== Poder Commands ==========

@cli.group()
@click.pass_context
def poder(ctx):
    """Manage Poder worker nodes"""
    pass


@poder.command("list")
@click.pass_context
def poder_list(ctx):
    """List all poder worker nodes"""
    client = ctx.obj["client"]
    try:
        poders = client.list_poders()
        if not poders:
            click.echo("No poders found")
            return
        click.echo(f"{'ID':<20} {'State':<10} {'Provider':<10} {'Arch':<8} {'OS Version':<30} {'URL'}")
        click.echo("-" * 95)
        for p in poders:
            res = p.get("resources", {})
            arch = res.get("arch", "-")
            os_ver = res.get("os_version", "-")
            click.echo(
                f"{p.get('id', 'N/A'):<20} {p.get('state', 'N/A'):<10} "
                f"{p.get('provider_type', 'N/A'):<10} {arch:<8} {os_ver:<30} "
                f"{p.get('url', 'N/A')}"
            )
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@poder.command("get")
@click.argument("poder_id")
@click.pass_context
def poder_get(ctx, poder_id):
    """Get a single poder's info by ID"""
    client = ctx.obj["client"]
    try:
        p = client.get_poder(poder_id)
        res = p.get("resources", {})
        click.echo(f"ID:            {p.get('id')}")
        click.echo(f"Name:          {p.get('name')}")
        click.echo(f"State:         {p.get('state')}")
        click.echo(f"Provider Type: {p.get('provider_type')}")
        click.echo(f"Region:        {p.get('region')}")
        click.echo(f"VM ID:         {p.get('vm_id', 'N/A')}")
        click.echo(f"URL:           {p.get('url')}")
        click.echo(f"Arch:          {res.get('arch', 'N/A')}")
        click.echo(f"OS Version:    {res.get('os_version', 'N/A')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@poder.command("delete")
@click.argument("poder_id")
@click.option("--yes", "-y", is_flag=True, help="Skip confirmation prompt")
@click.option("--keep-vm", is_flag=True,
              help="Delete only the poder record; do not terminate its cloud VM")
@click.pass_context
def poder_delete(ctx, poder_id, yes, keep_vm):
    """Delete a poder worker node by ID"""
    client = ctx.obj["client"]
    if not yes:
        prompt = f"Delete poder '{poder_id}'?"
        if not keep_vm:
            prompt += " (its cloud VM, if any, will be terminated)"
        click.confirm(prompt, abort=True)
    try:
        client.delete_poder(poder_id, keep_vm=keep_vm)
        click.echo(f"Poder '{poder_id}' deleted." + (" (VM kept)" if keep_vm else ""))
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


# ========== Health Command ==========

@cli.command()
@click.pass_context
def health(ctx):
    """Check server health"""
    client = ctx.obj["client"]
    try:
        health = client.health()
        click.echo(f"Status: {health.get('status', 'unknown')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


if __name__ == "__main__":
    cli(obj={})