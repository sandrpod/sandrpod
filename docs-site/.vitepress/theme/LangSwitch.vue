<script setup lang="ts">
import { computed } from 'vue'
import { useData, withBase } from 'vitepress'

// VitePress's built-in language menu maps /zh/quickstart ↔ /quickstart and drops
// the #anchor on the way. Reading a long page, switching language and landing at
// the top of the translated page is the difference between a switcher and a
// reload button.
//
// So the menu is ours: same computation, plus `location.hash` at click time.
const { page } = useData()

const isZh = computed(() => page.value.relativePath.startsWith('zh/'))

const target = computed(() => {
  const rel = page.value.relativePath.replace(/\.md$/, '')
  const without = isZh.value ? rel.replace(/^zh\//, '') : `zh/${rel}`
  const path = without.replace(/(^|\/)index$/, '$1').replace(/\/$/, '') || '/'
  return withBase(path === '/' ? (isZh.value ? '/' : '/zh/') : path)
})

function go(e: MouseEvent) {
  // Keep the reader where they were: same page, same heading.
  const hash = window.location.hash
  if (!hash) return
  e.preventDefault()
  window.location.href = target.value + hash
}
</script>

<template>
  <div class="lang-switch">
    <a v-if="isZh" :href="target" @click="go">English</a>
    <template v-else>
      <span class="on">English</span>
      <span class="sep">/</span>
      <a :href="target" @click="go">中文</a>
    </template>
  </div>
</template>

<style scoped>
.lang-switch {
  display: flex;
  align-items: center;
  gap: 0.35rem;
  padding: 0 0.5rem;
  font-size: 0.85rem;
  line-height: 1;
  white-space: nowrap;
}
.lang-switch a {
  color: var(--vp-c-text-2);
  text-decoration: none;
}
.lang-switch a:hover {
  color: var(--vp-c-brand-1);
}
.lang-switch .on {
  color: var(--vp-c-text-1);
  font-weight: 600;
}
.lang-switch .sep {
  color: var(--vp-c-divider);
}
</style>
