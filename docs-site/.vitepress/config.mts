import { defineConfig } from "vitepress";
import { copyMarkdownAndWriteLLMFiles } from "./llm";

const zhSidebar = [
  {
    text: "开始",
    items: [
      { text: "SandrPod 是什么", link: "/zh/" },
      { text: "快速开始", link: "/zh/quickstart" },
      { text: "第一个沙箱", link: "/zh/first-sandbox" },
    ],
  },
  {
    text: "接入任何 SDK",
    items: [
      { text: "E2B 直接替换", link: "/zh/e2b" },
      { text: "Python SDK", link: "/zh/python" },
      { text: "LangChain 与 deepagents", link: "/zh/langchain" },
      { text: "命令行 CLI", link: "/zh/cli" },
      { text: "REST API", link: "/zh/api" },
    ],
  },
  {
    text: "平台",
    items: [
      { text: "架构", link: "/zh/architecture" },
      { text: "Worker 与隧道", link: "/zh/workers" },
      { text: "隔离与放置", link: "/zh/isolation" },
      { text: "出网策略", link: "/zh/egress" },
    ],
  },
  {
    text: "部署运行",
    items: [
      { text: "生产部署", link: "/zh/deploy" },
      { text: "多实例", link: "/zh/multi-instance" },
      { text: "升级", link: "/zh/upgrading" },
    ],
  },
  {
    text: "帮助",
    items: [
      { text: "故障排查", link: "/zh/troubleshooting" },
      { text: "发布说明", link: "/zh/release-notes" },
    ],
  },
];

export default defineConfig({
  title: "SandrPod",
  description:
    "Self-hosted execution infrastructure for AI agents. Run agent code on any cloud, any machine — or none at all.",
  cleanUrls: true,
  // The site's own README is for the repository, not a page.
  srcExclude: ["README.md"],
  lastUpdated: true,
  head: [
    ["link", { rel: "icon", href: "/favicon.svg" }],
    ["meta", { name: "theme-color", content: "#0b1020" }],
  ],
  sitemap: { hostname: "https://docs.sandrpod.com" },

  locales: {
    root: { label: "English", lang: "en" },
    zh: {
      label: "中文",
      lang: "zh-CN",
      link: "/zh/",
      // VitePress merges `locales[lang].themeConfig` over the root themeConfig
      // (see resolveSiteDataByRoute); the newer `themeConfig.locales` shape is
      // not read by this version, which is why the Chinese pages first rendered
      // with English chrome.
      themeConfig: {
        nav: [
          { text: "开始", link: "/zh/quickstart" },
          { text: "平台", link: "/zh/architecture" },
          { text: "参考", link: "/zh/api" },
          { text: "部署", link: "/zh/deploy" },
          {
            text: "链接",
            items: [
              { text: "官网", link: "https://sandrpod.com" },
              { text: "控制台", link: "https://cloud.sandrpod.com" },
              { text: "GitHub", link: "https://github.com/sandrpod/sandrpod" },
              { text: "发布说明", link: "/zh/release-notes" },
            ],
          },
        ],
        sidebar: zhSidebar,
        outline: { level: [2, 3], label: "本页目录" },
        docFooter: { prev: "上一篇", next: "下一篇" },
        editLink: {
          pattern:
            "https://github.com/sandrpod/sandrpod/edit/main/docs-site/zh/:path",
          text: "编辑此页",
        },
        footer: {
          message: "Apache 2.0 —— 你的云、你的机器、你说了算。",
          copyright: "Copyright © 2026 SandrPod",
        },
        lastUpdated: { text: "最后更新" },
        search: {
          provider: "local",
          options: {
            locales: {
              zh: {
                translations: {
                  button: { buttonText: "搜索", buttonAriaLabel: "搜索" },
                  modal: {
                    displayDetails: "显示详情",
                    resetButtonTitle: "重置搜索",
                    backButtonTitle: "返回",
                    noResultsText: "没有结果",
                    footer: {
                      selectText: "选择",
                      navigateText: "切换",
                      closeText: "关闭",
                    },
                  },
                },
              },
            },
          },
        },
      },
    },
  },

  themeConfig: {
    logo: "/logo.svg",
    siteTitle: "SandrPod Docs",
    nav: [
      { text: "Get started", link: "/quickstart" },
      { text: "Platform", link: "/architecture" },
      { text: "Reference", link: "/api" },
      { text: "Guides", link: "/deploy" },
      {
        text: "Links",
        items: [
          { text: "Website", link: "https://sandrpod.com" },
          { text: "Console", link: "https://cloud.sandrpod.com" },
          { text: "GitHub", link: "https://github.com/sandrpod/sandrpod" },
          { text: "Release notes", link: "/release-notes" },
        ],
      },
    ],

    sidebar: [
      {
        text: "Introduction",
        items: [
          { text: "What SandrPod is", link: "/" },
          { text: "Quickstart", link: "/quickstart" },
          { text: "Your first sandbox", link: "/first-sandbox" },
        ],
      },
      {
        text: "Speak any SDK",
        items: [
          { text: "E2B (drop-in)", link: "/e2b" },
          { text: "Python SDK", link: "/python" },
          { text: "LangChain & deepagents", link: "/langchain" },
          { text: "CLI", link: "/cli" },
          { text: "REST API", link: "/api" },
        ],
      },
      {
        text: "Platform",
        items: [
          { text: "Architecture", link: "/architecture" },
          { text: "Workers and tunnels", link: "/workers" },
          { text: "Isolation and placement", link: "/isolation" },
          { text: "Egress policy", link: "/egress" },
        ],
      },
      {
        text: "Run it",
        items: [
          { text: "Self-host in production", link: "/deploy" },
          { text: "More than one instance", link: "/multi-instance" },
          { text: "Upgrading", link: "/upgrading" },
        ],
      },
      {
        text: "Help",
        items: [
          { text: "Troubleshooting", link: "/troubleshooting" },
          { text: "Release notes", link: "/release-notes" },
        ],
      },
    ],

    socialLinks: [
      { icon: "github", link: "https://github.com/sandrpod/sandrpod" },
    ],

    search: { provider: "local" },

    editLink: {
      pattern: "https://github.com/sandrpod/sandrpod/edit/main/docs-site/:path",
      text: "Edit this page",
    },

    footer: {
      message: "Apache 2.0 — your clouds, your machines, your rules.",
      copyright: "Copyright © 2026 SandrPod",
    },

    outline: { level: [2, 3] },

    docFooter: { prev: "Previous", next: "Next" },
  },

  // Two audiences read these pages: people and agents. Every page also exists as
  // .md at the same URL, plus llms.txt / llms-full.txt at the root — the same
  // affordance Daytona ships, because a docs site that an agent cannot fetch is a
  // docs site half its readers cannot use.
  buildEnd: copyMarkdownAndWriteLLMFiles,
});
