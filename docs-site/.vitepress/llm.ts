import { readFileSync, writeFileSync, mkdirSync, readdirSync, statSync } from 'node:fs'
import { join, relative, dirname } from 'node:path'

function walk(dir: string, base = dir): string[] {
  const out: string[] = []
  for (const name of readdirSync(dir)) {
    if (name.startsWith('.') || name === 'node_modules' || name === 'public') continue
    const full = join(dir, name)
    const st = statSync(full)
    if (st.isDirectory()) out.push(...walk(full, base))
    else if (name.endsWith('.md')) out.push(relative(base, full))
  }
  return out
}

// frontmatter without a parser: these files use a flat `key: value` block.
function frontmatter(src: string): Record<string, string> {
  const m = src.match(/^---\n([\s\S]*?)\n---\n/)
  if (!m) return {}
  const out: Record<string, string> = {}
  for (const line of m[1].split('\n')) {
    const i = line.indexOf(':')
    if (i > 0) out[line.slice(0, i).trim()] = line.slice(i + 1).trim().replace(/^["']|["']$/g, '')
  }
  return out
}

function stripFrontmatter(src: string): string {
  return src.replace(/^---\n[\s\S]*?\n---\n/, '')
}

export function copyMarkdownAndWriteLLMFiles(siteConfig: any) {
  // VitePress passes its SiteConfig; the shape has moved between minor versions,
  // so accept both spellings rather than pin to one.
  const outDir: string = siteConfig?.site?.outDir ?? siteConfig?.outDir
  const srcDir: string = siteConfig?.srcDir ?? siteConfig?.root
  if (!outDir || !srcDir) {
    throw new Error(`llm export: cannot locate directories (outDir=${outDir} srcDir=${srcDir})`)
  }
  const pages = walk(srcDir).filter((p) => !p.startsWith('.vitepress'))

  const entries: { path: string; title: string; description: string; body: string }[] = []
  for (const rel of pages) {
    const src = readFileSync(join(srcDir, rel), 'utf8')
    const fm = frontmatter(src)
    const body = stripFrontmatter(src)
    const urlPath = '/' + rel.replace(/\.md$/, '').replace(/(^|\/)index$/, '')
    const clean = urlPath === '/' ? '/index' : urlPath

    // The .md twin, at the page's own URL. `/quickstart` → `/quickstart.md`.
    const dest = join(outDir, rel)
    mkdirSync(dirname(dest), { recursive: true })
    writeFileSync(dest, src)

    entries.push({
      path: clean,
      title: fm.title || rel.replace(/\.md$/, ''),
      description: fm.description || '',
      body,
    })
  }

  entries.sort((a, b) => (a.path === '/index' ? -1 : b.path === '/index' ? 1 : a.path.localeCompare(b.path)))

  const index = [
    '# SandrPod documentation',
    '',
    '> Self-hosted execution infrastructure for AI agents: run agent code on any',
    '> cloud, any machine, or none at all — and speak any SDK, including the',
    '> unmodified E2B SDK.',
    '',
    'Every page below also exists as Markdown at the same URL plus `.md`',
    '(`/e2b.md`), and the whole site is concatenated in `/llms-full.txt`.',
    '',
    ...entries.map((e) => `- [${e.title}](https://docs.sandrpod.com${e.path}.md)${e.description ? `: ${e.description}` : ''}`),
    '',
  ].join('\n')
  writeFileSync(join(outDir, 'llms.txt'), index)

  const full = entries
    .map((e) => `\n\n<!-- ${e.path} -->\n\n# ${e.title}\n\n${e.body.trim()}\n`)
    .join('')
  writeFileSync(join(outDir, 'llms-full.txt'), `# SandrPod documentation (full)\n${full}`)

  console.log(`[llm] ${entries.length} pages exported as .md, plus llms.txt and llms-full.txt`)
}
