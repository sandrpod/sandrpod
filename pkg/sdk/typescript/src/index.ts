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
  output: string;
  exit_code: number;
  truncated?: boolean;
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
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
