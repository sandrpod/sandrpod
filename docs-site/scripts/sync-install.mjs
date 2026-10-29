// Publish deploy/node-install.sh at /install.sh.
//
// The console hands out `curl -fsSL https://docs.sandrpod.com/install.sh | bash -s --
// …`, so the script has to be reachable there. Copying it at build time keeps one
// source of truth: the file in the repository that CI and humans already run.
import { copyFileSync, existsSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
// Both installers: the worker one (a machine that hosts sandboxes) and the agent
// one (a machine that is a sandbox).
const scripts = [
  ['node-install.sh', 'install.sh'],
  ['agent-install.sh', 'agent.sh'],
]
const deployDir = join(here, '..', '..', 'deploy')
const src = join(deployDir, 'node-install.sh')

mkdirSync(join(here, '..', 'public'), { recursive: true })
if (!existsSync(src)) {
  // A docs-only checkout (the site is sometimes built without the rest of the
  // repository): publish nothing rather than a stale copy.
  console.warn(`[install.sh] ${src} not found — skipping`)
  process.exit(0)
}
for (const [from, to] of scripts) {
  const dst = join(here, '..', 'public', to)
  copyFileSync(join(deployDir, from), dst)
  console.log(`[${to}] published from deploy/${from}`)
}
