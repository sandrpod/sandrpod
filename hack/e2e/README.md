# Live end-to-end suites

These scripts drive a running SandrPod deployment from a developer machine with
the real E2B Python SDK and the real `sandrpod-cli`. They are the regression
baseline for the surfaces users actually touch; they are not unit tests.

They create sandboxes, so point them at a disposable deployment, a test
workspace, or an operator account you are willing to clean up after.

## E2B SDK

```bash
E2B_API_KEY=e2b_... \
E2B_DOMAIN=sandbox.example.com \
python3 hack/e2e/e2b_suite.py
```

Optional:

| Variable | Meaning |
|---|---|
| `E2B_API_URL` | defaults to `https://api.<E2B_DOMAIN>` |
| `E2E_TAG` | metadata tag for the run; leftovers are found by it |
| `E2E_MCP=1` | run the in-sandbox MCP gateway check (starts a minimal stdio MCP server) |

Run it once per SDK version you support, e.g. in separate virtualenvs for
`e2b==2.30.0` and `e2b==2.64.0` plus a matching `e2b-code-interpreter`.

The suite creates one live sandbox at a time so it also works against a trial
plan with a one-sandbox cap. It covers control plane, commands, files, PTY,
signed URLs, ports, metrics, pause/resume, network policy, autoPause,
`cpuCount`, async SDK, and the MCP gateway. Unsupported E2B control-plane
features are reported as `GAP`, not failures:

- `update_network` (405)
- `create_snapshot` / `list_snapshots` (use `sandrpod-cli snapshot`)
- `fork`

## CLI

```bash
SANDRPOD_API_URL=https://cloud.example.com \
SANDRPOD_API_TOKEN=e2b_... \
CLI_BIN=/path/to/sandrpod-cli \
python3 hack/e2e/cli_suite.py
```

Optional:

| Variable | Meaning |
|---|---|
| `SANDRPOD_ADMIN_TOKEN` | enables `metrics`, `poder list`, and token-list checks |
| `E2E_PODER_ID` | also exercises `create --poder` on that worker |
| `E2E_HOME` | keep the temporary CLI config directory for inspection |

The CLI suite uses a fresh `HOME`, checks the config file is mode `0600`, and
covers create/job/list/env, execute/stream, sessions, files (including binary and
watch), contexts, preview/stats/logs, MCP config CRUD, egress, stop/start,
snapshot with `--timeout`, and the pinned-create path when configured.

`shell` is an interactive TTY flow and is deliberately reported as a `GAP`;
exercise it manually.

## Exit codes

Both scripts exit non-zero only on failed checks. `GAP` lines document known
unimplemented or environment-dependent surfaces and do not fail the run.
