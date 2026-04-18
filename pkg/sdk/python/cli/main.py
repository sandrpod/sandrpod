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
DEFAULT_API_URL = "http://localhost:18080"


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


def save_config_api_url(url):
    """保存 API URL 到配置文件"""
    config = load_config()
    config["api_url"] = url
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
    ctx.obj["client"] = CLIClient(api_url=api_url, timeout=timeout)


# ========== Config Commands ==========

@cli.command()
@click.argument("url")
def set_api_url(url):
    """Set the default API URL"""
    save_config_api_url(url)


@cli.command()
def get_api_url():
    """Show the current API URL"""
    current = get_configured_api_url()
    click.echo(f"Current API URL: {current}")


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
@click.option("--provider-type", default="local", help="Provider type: aws, aliyun, local, docker")
@click.option("--instance-type", default="medium", help="Instance type")
@click.pass_context
def create(ctx, name, region, provider_type, instance_type):
    """Create a sandbox"""
    client = ctx.obj["client"]
    try:
        sandbox = client.create_sandbox(name, region, provider_type, instance_type)
        click.echo(f"Sandbox created: {sandbox.get('name')}")
        click.echo(f"State: {sandbox.get('state')}")
        click.echo(f"Provider: {sandbox.get('provider_type')}")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
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
@click.argument("name")
@click.pass_context
def delete(ctx, name):
    """Delete a sandbox"""
    client = ctx.obj["client"]
    try:
        client.delete_sandbox(name)
        click.echo(f"Sandbox '{name}' deleted")
    except Exception as e:
        click.echo(f"Error: {e}", err=True)
        sys.exit(1)


@cli.command()
@click.argument("name")
@click.pass_context
def start(ctx, name):
    """Start a sandbox"""
    client = ctx.obj["client"]
    try:
        sandbox = client.start_sandbox(name)
        click.echo(f"Sandbox started: {sandbox.get('name')}")
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
        sandbox = client.stop_sandbox(name)
        click.echo(f"Sandbox stopped: {sandbox.get('name')}")
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


# ========== Poder Commands ==========

@cli.command()
@click.pass_context
def poders(ctx):
    """List all poders"""
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