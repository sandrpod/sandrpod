# sandrpod-cli

Command-line client for [SandrPod](https://github.com/sandrpod/sandrpod) — open-source, **self-hosted execution infrastructure (sandboxes) for AI agents**. Run sandboxes on your own substrate (8 clouds incl. Aliyun/Tencent, plain Docker, or a bare machine), drive them over the native REST API, and stay E2B-compatible.

## Install

```bash
pip install sandrpod-cli
pip install "sandrpod-cli[shell]"   # + interactive PTY shell (websocket-client)
```

## Configure

```bash
export SANDRPOD_API_URL=http://your-server:8080
export SANDRPOD_API_TOKEN=your-token          # only if the server runs with auth
# or pass per-command:  sandrpod-cli --api-url http://your-server:8080 <cmd>
```

## Quickstart

```bash
sandrpod-cli create mybox --provider local           # create a sandbox
sandrpod-cli execute mybox "echo hi; python3 -V"     # one-shot exec
sandrpod-cli run mybox "import numpy; print(1)"      # stateful code interpreter (Jupyter-style)
sandrpod-cli fs write mybox /workspace/a.txt hello   # filesystem ops
sandrpod-cli fs ls mybox
sandrpod-cli stream mybox "for i in 1 2 3; do echo \$i; sleep 1; done"   # real-time output
sandrpod-cli shell mybox                             # interactive PTY (needs the [shell] extra)
sandrpod-cli preview mybox 8000                      # proxy a web port inside the sandbox
sandrpod-cli stats mybox                             # CPU / memory / disk
sandrpod-cli delete mybox
```

**Charts**: `sandrpod-cli run` captures matplotlib figures, saving the rendered PNG plus structured chart data (E2B-style).

Full command list (cloud providers, snapshots, tokens, contexts, directory watch, …) is in the [SandrPod repository](https://github.com/sandrpod/sandrpod).

## License

MIT
