# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please **do not** open a public GitHub issue.

Instead, report it via GitHub's private security advisory:
**[Report a vulnerability](https://github.com/sandrpod/sandrpod/security/advisories/new)**

Please include:
- A description of the vulnerability and its impact
- Steps to reproduce
- Any suggested mitigations

We aim to acknowledge reports within **48 hours** and provide a fix or mitigation plan within **14 days** for critical issues.

---

## Supported Versions

| Version | Supported |
|---------|-----------|
| latest (`main`) | ✅ |
| older releases | ❌ |

---

## Known Limitations

The following are known security trade-offs in the current architecture.
They are documented here for transparency, not because they are ignored.

### Toolbox container runs as root

**Affected component:** `docker/Dockerfile.toolbox` / `cmd/toolbox`

**Description:**
The Toolbox container — which directly executes user-submitted code (bash, Python, Node.js) — runs as the Linux `root` user inside the container. There is no `USER` directive in the Dockerfile.

**Impact:**
- If a user's code exploits a container escape vulnerability (e.g. a Linux kernel CVE), the attacker would gain root access on the host machine.
- Files written to `/workspace` by user code are owned by root.
- User code can modify any file within the container's filesystem.

**Mitigations already in place:**
- Each sandbox runs in an **isolated container** — there is no multi-tenant sharing within a single Toolbox instance.
- Toolbox containers are **not run with `--privileged`**.
- The Docker socket is **not mounted** into Toolbox containers.

**Planned improvement:**
Add a non-root `sandbox` user in the Toolbox image and run the process under that user. This is deferred because it changes the runtime environment for user code (e.g. `pip install`, `npm install` behavior without root) and requires user-facing documentation updates.

For production deployments handling untrusted code, consider adding:
- A seccomp or AppArmor profile to restrict syscalls
- A stronger sandbox layer such as [gVisor](https://gvisor.dev) or [Firecracker](https://firecracker-microvm.github.io)

---

### Poder container requires Docker socket access

**Affected component:** `docker/Dockerfile.poder` / `cmd/poder`

**Description:**
The Poder worker node mounts `/var/run/docker.sock` from the host and runs as root in order to manage sandbox container lifecycles.

**Impact:**
Access to the Docker socket is equivalent to root on the host. A compromised Poder process could spawn arbitrary containers on the host machine.

**Mitigations:**
- Poder is a trusted internal component — it communicates only with the SandrPod API server via an authenticated WebSocket tunnel.
- When a token is configured (`SANDRPOD_TOKEN` — strongly recommended; without one, auth is disabled and every request runs as an anonymous admin), the API server requires it for all Poder registration and heartbeat calls.
- This is a standard trade-off shared by tools such as Portainer, Watchtower, and other Docker management services.

---

### WebSocket endpoint accepts all origins

**Affected component:** `cmd/server/main.go`

**Description:**
The WebSocket upgrader is configured with `CheckOrigin: func(r *http.Request) bool { return true }`, which disables origin checking on the `/ws/poder/connect` and `/ws/sandbox/connect` endpoints.

**Impact:**
In a browser context, a malicious page could initiate a WebSocket connection to the API server on behalf of a user. However, since these endpoints require a valid Bearer token, practical exploitability is low.

**Planned improvement:**
Make the allowed origin list configurable via a `-allowed-origins` flag so operators can restrict connections to known hosts in production.
