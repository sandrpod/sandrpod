// Publish deploy/node-install.sh at /install.sh.
//
// The console hands out `curl -fsSL https://docs.sandrpod.com/install.sh | bash -s --
// …`, so the script has to be reachable there. Copying it at build time keeps one
// source of truth: the file in the repository that CI and humans already run.
import { copyFileSync, existsSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const src = join(here, '..', '..', 'deploy', 'node-install.sh')
const dst = join(here, '..', 'public', 'install.sh')

mkdirSync(dirname(dst), { recursive: true })
if (!existsSync(src)) {
  // A docs-only checkout (the site is sometimes built without the rest of the
  // repository): publish nothing rather than a stale copy.
  console.warn(`[install.sh] ${src} not found — skipping`)
  process.exit(0)
}
copyFileSync(src, dst)
console.log('[install.sh] published from deploy/node-install.sh')
