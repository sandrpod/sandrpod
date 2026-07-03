// Copyright 2024 SandrPod
// Minimal, dependency-free TypeScript client for the SandrPod API. Uses the
// global fetch (Node >= 18, browsers, Deno, Bun).

export type SandboxState =
  | "PENDING"
  | "STARTING"
  | "RUNNING"
  | "STOPPING"
  | "STOPPED"
  | "ERROR"
  | "TERMINATED";

export interface Sandbox {
  id: string;
  name: string;
  region: string;
  provider_type?: string;
  instance_type?: string;
  state: SandboxState;
  ip?: string;
  owner?: string;
  arch?: string;
  os?: string;
  os_version?: string;
  created_at: string;
  last_activity?: string;
}

export interface CreateSandboxOptions {
  region?: string;
  providerType?: string;
  instanceType?: string;
  image?: string;
  ttlSeconds?: number;
  cpuCores?: number;
  memoryMB?: number;
  /** Return a job immediately instead of blocking until RUNNING. */
  async?: boolean;
}

export interface Job {
  id: string;
  type: string;
  status: "PENDING" | "IN_PROGRESS" | "COMPLETED" | "FAILED";
  sandbox_name: string;
  error_message?: string;
  owner?: string;
}

export interface ExecuteResult {
  exit_code: number;
  stdout: string;
  stderr: string;
  started_at?: string;
  ended_at?: string;
}

/** Per-sandbox live resource usage (bytes for memory/disk). */
export interface SandboxMetrics {
  cpu_count: number;
  cpu_used_pct: number;
  mem_total: number;
  mem_used: number;
  disk_total: number;
  disk_used: number;
}

/** A stateful code-interpreter context (isolated namespace). */
export interface CodeContext {
  id: string;
  language: string;
  cwd: string;
}

/** Result of a stateful runCode call. */
export interface CodeResult {
  stdout: string;
  stderr: string;
  /** Value of the final expression, if any. */
  text: string;
  /** Traceback if the cell raised. */
  error: string;
}

/** One filesystem change from a directory watcher. */
export interface WatchEvent {
  name: string;
  /** create | write | remove | rename | chmod */
  type: string;
}

export interface ClientOptions {
  apiUrl?: string;
  token?: string;
  /** Per-request timeout in milliseconds (default 30_000). */
  timeoutMs?: number;
}

export class SandrPodError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly body: string,
  ) {
    super(message);
    this.name = "SandrPodError";
  }
}

export class SandrPodClient {
  private readonly apiUrl: string;
  private readonly token?: string;
  private readonly timeoutMs: number;

  constructor(opts: ClientOptions = {}) {
    this.apiUrl = (opts.apiUrl ?? "http://localhost:8080").replace(/\/+$/, "");
    this.token = opts.token;
    this.timeoutMs = opts.timeoutMs ?? 30_000;
  }

  private async request<T>(
    method: string,
    path: string,
    body?: unknown,
    timeoutMs?: number,
  ): Promise<T> {
    const headers: Record<string, string> = {};
    if (this.token) headers["Authorization"] = `Bearer ${this.token}`;
    if (body !== undefined) headers["Content-Type"] = "application/json";

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs ?? this.timeoutMs);
    try {
      const resp = await fetch(`${this.apiUrl}${path}`, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: controller.signal,
      });
      const text = await resp.text();
      if (!resp.ok) {
        throw new SandrPodError(
          `${method} ${path} failed: HTTP ${resp.status}`,
          resp.status,
          text,
        );
      }
      return (text ? JSON.parse(text) : undefined) as T;
    } finally {
      clearTimeout(timer);
    }
  }

  /** List sandboxes visible to the token. */
  async listSandboxes(): Promise<Sandbox[]> {
    const r = await this.request<{ sandboxes: Sandbox[] | null }>(
      "GET",
      "/api/v1/sandboxes",
    );
    return r.sandboxes ?? [];
  }

  async getSandbox(name: string): Promise<Sandbox> {
    return this.request<Sandbox>("GET", `/api/v1/sandboxes/${encodeURIComponent(name)}`);
  }

  async getJob(jobId: string): Promise<Job> {
    return this.request<Job>("GET", `/api/v1/jobs/${encodeURIComponent(jobId)}`);
  }

  /**
   * Create a sandbox. Returns the create response ({ job_id, status, sandbox }).
   * With `waitUntilRunning`, polls until the sandbox reaches RUNNING or throws.
   */
  async createSandbox(
    name: string,
    opts: CreateSandboxOptions = {},
    waitUntilRunning = false,
    pollTimeoutMs = 900_000,
  ): Promise<{ job_id?: string; status?: string; sandbox: Sandbox }> {
    const body: Record<string, unknown> = {
      name,
      region: opts.region ?? "local",
      provider_type: opts.providerType ?? "local",
    };
    if (opts.instanceType) body["instance_type"] = opts.instanceType;
    if (opts.image) body["image_id"] = opts.image;
    if (opts.ttlSeconds) body["ttl_seconds"] = opts.ttlSeconds;
    if (opts.cpuCores) body["cpu_cores"] = opts.cpuCores;
    if (opts.memoryMB) body["memory_mb"] = opts.memoryMB;
    if (opts.async ?? waitUntilRunning) body["async"] = true;

    const resp = await this.request<{ job_id?: string; status?: string; sandbox: Sandbox }>(
      "POST",
      "/api/v1/sandboxes",
      body,
    );
    if (!waitUntilRunning) return resp;

    const deadline = Date.now() + pollTimeoutMs;
    while (Date.now() < deadline) {
      await sleep(5000);
      let sb: Sandbox | undefined;
      try {
        sb = await this.getSandbox(name);
      } catch {
        continue;
      }
      if (sb.state === "RUNNING") return { ...resp, sandbox: sb };
      if (sb.state === "ERROR") {
        let msg = "";
        if (resp.job_id) {
          try {
            msg = (await this.getJob(resp.job_id)).error_message ?? "";
          } catch {
            /* ignore */
          }
        }
        throw new Error(`sandbox ${name} entered ERROR state${msg ? ": " + msg : ""}`);
      }
    }
    throw new Error(`timed out waiting for sandbox ${name} to reach RUNNING`);
  }

  async deleteSandbox(name: string): Promise<void> {
    await this.request<unknown>("DELETE", `/api/v1/sandboxes/${encodeURIComponent(name)}`);
  }

  /** Run a shell command in the sandbox and return its output. */
  async execute(name: string, command: string, timeoutSec = 30): Promise<ExecuteResult> {
    return this.request<ExecuteResult>(
      "POST",
      `/api/v1/sandboxes/execute?sandbox=${encodeURIComponent(name)}`,
      { language: "bash", code: command, timeout: timeoutSec },
      timeoutSec * 1000 + 5_000,
    );
  }

  // ---- per-sandbox resource stats (toolbox /metrics) ----

  /** Live CPU / memory / disk usage for one sandbox. */
  async stats(name: string): Promise<SandboxMetrics> {
    return this.request<SandboxMetrics>("GET", this.toolboxPath(name, "metrics"));
  }

  // ---- stateful code interpreter (toolbox /code-interpreter/*) ----

  /**
   * Run code in a stateful kernel. Variables persist across calls that share
   * the same `contextId` (Jupyter-style), unlike {@link execute}.
   */
  async runCode(name: string, code: string, contextId?: string): Promise<CodeResult> {
    const body: Record<string, unknown> = { code };
    if (contextId) body.context_id = contextId;
    return this.request<CodeResult>(
      "POST",
      this.toolboxPath(name, "code-interpreter/execute"),
      body,
    );
  }

  /** Create a new stateful context (isolated namespace). */
  async createCodeContext(name: string, language = "python", cwd = ""): Promise<CodeContext> {
    return this.request<CodeContext>(
      "POST",
      this.toolboxPath(name, "code-interpreter/contexts"),
      { language, cwd },
    );
  }

  /** List stateful contexts. */
  async listCodeContexts(name: string): Promise<CodeContext[]> {
    const r = await this.request<CodeContext[] | null>(
      "GET",
      this.toolboxPath(name, "code-interpreter/contexts"),
    );
    return r ?? [];
  }

  /** Restart a context's kernel (clears its variables, keeps the id). */
  async restartCodeContext(name: string, contextId: string): Promise<void> {
    await this.request<unknown>(
      "POST",
      this.toolboxPath(name, `code-interpreter/contexts/${encodeURIComponent(contextId)}/restart`),
    );
  }

  /** Remove a context and its kernel. */
  async removeCodeContext(name: string, contextId: string): Promise<void> {
    await this.request<unknown>(
      "DELETE",
      this.toolboxPath(name, `code-interpreter/contexts/${encodeURIComponent(contextId)}`),
    );
  }

  // ---- directory watch (toolbox /watch/*) ----

  /** Start watching a directory; returns a handle to poll events and stop. */
  async watchDir(name: string, path: string, recursive = false): Promise<WatchHandle> {
    const r = await this.request<{ watcher_id: string }>(
      "POST",
      this.toolboxPath(name, "watch/create"),
      { path, recursive },
    );
    return new WatchHandle(this, name, r.watcher_id);
  }

  /** @internal Used by {@link WatchHandle}. */
  async _watchEvents(name: string, watcherId: string): Promise<WatchEvent[]> {
    const r = await this.request<{ events: WatchEvent[] | null }>(
      "GET",
      this.toolboxPath(name, `watch/events?id=${encodeURIComponent(watcherId)}`),
    );
    return r.events ?? [];
  }

  /** @internal Used by {@link WatchHandle}. */
  async _watchRemove(name: string, watcherId: string): Promise<void> {
    await this.request<unknown>(
      "POST",
      this.toolboxPath(name, "watch/remove"),
      { watcher_id: watcherId },
    );
  }

  private toolboxPath(name: string, sub: string): string {
    return `/api/v1/sandboxes/${encodeURIComponent(name)}/toolbox/${sub}`;
  }
}

/**
 * Poll handle for a directory watcher. Call {@link getNewEvents} to fetch the
 * events accrued since the last call, and {@link stop} when done.
 */
export class WatchHandle {
  private closed = false;

  constructor(
    private readonly client: SandrPodClient,
    private readonly sandbox: string,
    readonly watcherId: string,
  ) {}

  /** Events since the last call (`[{ name, type }]`). */
  async getNewEvents(): Promise<WatchEvent[]> {
    if (this.closed) return [];
    return this.client._watchEvents(this.sandbox, this.watcherId);
  }

  /** Stop the watcher (idempotent). */
  async stop(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    try {
      await this.client._watchRemove(this.sandbox, this.watcherId);
    } catch {
      /* best-effort */
    }
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
