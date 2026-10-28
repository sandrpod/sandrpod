import DefaultTheme from 'vitepress/theme'
import type { Theme } from 'vitepress'
import { h } from 'vue'
import CopyForLLM from './CopyForLLM.vue'
import LangSwitch from './LangSwitch.vue'
import './custom.css'

// The layout wrapper is what puts the two agent-facing buttons on every page
// without every page having to mention them.
const Layout = () =>
  h(DefaultTheme.Layout, null, {
    'doc-before': () => h(CopyForLLM),
    // Language switching belongs next to search, not in a dropdown that forgets
    // which heading the reader was on.
    'nav-bar-content-after': () => h(LangSwitch),
  })

export default {
  extends: DefaultTheme,
  Layout,
} satisfies Theme
