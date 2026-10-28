<script setup lang="ts">
import { computed, ref } from 'vue'
import { useData, withBase } from 'vitepress'

// Two buttons, for the reader who is not a person reading.
//
// "Copy for LLM" puts the page on the clipboard wrapped in the one sentence an
// assistant needs to stop summarising you back at yourself. "View as Markdown"
// opens the .md twin, so a tool can fetch the page instead of scraping HTML.
//
// Daytona ships the same pair, and the reasoning is theirs: agents are a first
// class reader of documentation now, and an HTML page with a nav bar is a worse
// input than the Markdown it was generated from.
const { page, title, frontmatter } = useData()

const mdPath = computed(() => {
  const rel = page.value.relativePath.replace(/\.md$/, '')
  return withBase('/' + (rel.endsWith('index') ? rel.slice(0, -'index'.length) : rel) + '.md')
})

const copied = ref(false)
const failed = ref(false)

async function copy() {
  try {
    const res = await fetch(mdPath.value)
    if (!res.ok) throw new Error(String(res.status))
    const md = await res.text()
    const header = [
      `# ${title.value}`,
      '',
      '> Source: https://docs.sandrpod.com' + page.value.relativePath.replace(/\.md$/, ''),
      '> The following is SandrPod documentation in Markdown, for reference.',
      '',
    ].join('\n')
    await navigator.clipboard.writeText(header + md)
    copied.value = true
    setTimeout(() => (copied.value = false), 2000)
  } catch {
    failed.value = true
    setTimeout(() => (failed.value = false), 2500)
  }
}
</script>

<template>
  <div class="llm-actions">
    <button type="button" :class="{ ok: copied }" @click="copy">
      <svg viewBox="0 0 24 24" aria-hidden="true">
        <rect x="9" y="9" width="11" height="11" rx="2" />
        <path d="M5 15V5a2 2 0 0 1 2-2h10" />
      </svg>
      {{ failed ? 'Copy failed' : copied ? 'Copied' : 'Copy for LLM' }}
    </button>
    <a :href="mdPath" target="_blank" rel="noreferrer">
      <svg viewBox="0 0 24 24" aria-hidden="true">
        <rect x="3" y="5" width="18" height="14" rx="2" />
        <path d="M7 10l2.5 2.5L7 15M12.5 15H17" />
      </svg>
      View as Markdown
    </a>
  </div>
</template>

<style scoped>
.llm-actions {
  display: flex;
  gap: 1.25rem;
  align-items: center;
  margin: 0 0 1.5rem;
  padding-bottom: 1rem;
  border-bottom: 1px solid var(--vp-c-divider);
  font-size: 0.85rem;
}
.llm-actions button,
.llm-actions a {
  display: inline-flex;
  align-items: center;
  gap: 0.4rem;
  color: var(--vp-c-text-2);
  background: none;
  border: 0;
  padding: 0;
  cursor: pointer;
  font: inherit;
  text-decoration: none;
}
.llm-actions button:hover,
.llm-actions a:hover {
  color: var(--vp-c-brand-1);
}
.llm-actions button.ok {
  color: var(--vp-c-brand-1);
}
.llm-actions svg {
  width: 1rem;
  height: 1rem;
  fill: none;
  stroke: currentColor;
  stroke-width: 1.6;
  stroke-linecap: round;
  stroke-linejoin: round;
}
</style>
