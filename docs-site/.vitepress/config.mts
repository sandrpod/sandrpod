import { defineConfig } from 'vitepress'
import { copyMarkdownAndWriteLLMFiles } from './llm'

export default defineConfig({
  title: 'SandrPod',
  description: 'Self-hosted execution infrastructure for AI agents. Run agent code on any cloud, any machine — or none at all.',
  cleanUrls: true,
  lastUpdated: true,
  head: [
    ['link', { rel: 'icon', href: '/favicon.svg' }],
    ['meta', { name: 'theme-color', content: '#0b1020' }],
  ],
  sitemap: { hostname: 'https://docs.sandrpod.com' },

  themeConfig: {
    logo: '/logo.svg',
    siteTitle: 'SandrPod Docs',
    nav: [
      { text: 'Get started', link: '/quickstart' },
      { text: 'Platform', link: '/architecture' },
      { text: 'Reference', link: '/api' },
      { text: 'Guides', link: '/deploy' },
      {
        text: 'Links',
        items: [
          { text: 'Website', link: 'https://sandrpod.com' },
          { text: 'Console', link: 'https://cloud.sandrpod.com' },
          { text: 'GitHub', link: 'https://github.com/sandrpod/sandrpod' },
          { text: 'Release notes', link: '/release-notes' },
        ],
      },
    ],

    sidebar: [
      {
        text: 'Introduction',
        items: [
          { text: 'What SandrPod is', link: '/' },
          { text: 'Quickstart', link: '/quickstart' },
          { text: 'Your first sandbox', link: '/first-sandbox' },
        ],
      },
      {
        text: 'Speak any SDK',
        items: [
          { text: 'E2B (drop-in)', link: '/e2b' },
          { text: 'Python SDK', link: '/python' },
          { text: 'LangChain & deepagents', link: '/langchain' },
          { text: 'CLI', link: '/cli' },
          { text: 'REST API', link: '/api' },
        ],
      },
      {
        text: 'Platform',
        items: [
          { text: 'Architecture', link: '/architecture' },
          { text: 'Workers and tunnels', link: '/workers' },
          { text: 'Isolation and placement', link: '/isolation' },
          { text: 'Egress policy', link: '/egress' },
        ],
      },
      {
        text: 'Run it',
        items: [
          { text: 'Self-host in production', link: '/deploy' },
          { text: 'More than one instance', link: '/multi-instance' },
          { text: 'Upgrading', link: '/upgrading' },
        ],
      },
      {
        text: 'Help',
        items: [
          { text: 'Troubleshooting', link: '/troubleshooting' },
          { text: 'Release notes', link: '/release-notes' },
        ],
      },
    ],

    socialLinks: [{ icon: 'github', link: 'https://github.com/sandrpod/sandrpod' }],

    search: { provider: 'local' },

    editLink: {
      pattern: 'https://github.com/sandrpod/sandrpod/edit/main/docs-site/:path',
      text: 'Edit this page',
    },

    footer: {
      message: 'Apache 2.0 — your clouds, your machines, your rules.',
      copyright: 'Copyright © 2026 SandrPod',
    },

    outline: { level: [2, 3] },

    docFooter: { prev: 'Previous', next: 'Next' },
  },

  // Two audiences read these pages: people and agents. Every page also exists as
  // .md at the same URL, plus llms.txt / llms-full.txt at the root — the same
  // affordance Daytona ships, because a docs site that an agent cannot fetch is a
  // docs site half its readers cannot use.
  buildEnd: copyMarkdownAndWriteLLMFiles,
})
