# @sandrpod/sdk

Minimal, dependency-free TypeScript client for the SandrPod sandbox control
plane. Uses the global `fetch` (Node ≥ 18, browsers, Deno, Bun).

## Install

```bash
npm install @sandrpod/sdk
# or build from source:
cd pkg/sdk/typescript && npm install && npm run build
```

## Usage

```ts
import { SandrPodClient } from "@sandrpod/sdk";

const client = new SandrPodClient({
  apiUrl: process.env.SANDRPOD_API_URL ?? "http://localhost:8080",
  token: process.env.SANDRPOD_API_TOKEN,
});

// Create and wait until RUNNING (async provisioning + polling under the hood).
const { sandbox } = await client.createSandbox(
  "demo",
  { providerType: "gcp", region: "asia-east1-a", instanceType: "e2-medium" },
  /* waitUntilRunning */ true,
);

const res = await client.execute(sandbox.name, "echo hello");
console.log(res.output);

await client.deleteSandbox(sandbox.name);
```

For a fire-and-forget create, pass `waitUntilRunning = false` (the default) and
poll `getJob(job_id)` / `getSandbox(name)` yourself.
