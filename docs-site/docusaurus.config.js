const repoEditBase =
  'https://github.com/jerryfane/lightning-agent-tools/edit/main';

function editUrl({docPath}) {
  if (docPath === 'generated/skills/index.md') {
    return undefined;
  }

  const generatedSkill = docPath.match(/^generated\/skills\/([^/]+)\.md$/);
  if (generatedSkill) {
    return `${repoEditBase}/skills/${generatedSkill[1]}/SKILL.md`;
  }

  if (docPath.startsWith('generated/')) {
    return `${repoEditBase}/docs/${docPath.slice('generated/'.length)}`;
  }

  return `${repoEditBase}/docs-site/docs/${docPath}`;
}

const config = {
  title: 'Lightning Agent Tools',
  tagline: 'Agent-ready Lightning Network operations, MCP tools, and skills.',

  url: 'https://jerryfane.github.io',
  baseUrl: '/lightning-agent-tools/',
  organizationName: 'jerryfane',
  projectName: 'lightning-agent-tools',
  trailingSlash: false,

  onBrokenLinks: 'throw',
  markdown: {
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          routeBasePath: '/',
          sidebarPath: './sidebars.js',
          editUrl,
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      },
    ],
  ],

  plugins: [
    [
      'docusaurus-plugin-llms',
      {
        title: 'Lightning Agent Tools Documentation',
        description:
          'Documentation for the Lightning MCP server, node operations daemon, agent skills, and regtest workflows.',
        docsDir: 'docs',
        generateLLMsTxt: true,
        generateLLMsFullTxt: true,
        generateMarkdownFiles: true,
        includeBlog: false,
        includeOrder: ['intro.md', 'generated/quickref.md', 'generated/**/*.md'],
        rootContent:
          'Use these links for an AI-friendly index of the Lightning Agent Tools documentation.',
        fullRootContent:
          'This is a single-file, AI-friendly copy of the Lightning Agent Tools documentation generated from the Docusaurus source.',
      },
    ],
  ],

  themeConfig: {
    navbar: {
      title: 'Lightning Agent Tools',
      items: [
        {
          to: '/',
          label: 'Docs',
          position: 'left',
        },
        {
          href: 'https://github.com/jerryfane/lightning-agent-tools',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Docs',
          items: [
            {
              label: 'Overview',
              to: '/',
            },
            {
              label: 'Generated Docs',
              to: '/generated/quickref',
            },
          ],
        },
        {
          title: 'Project',
          items: [
            {
              label: 'GitHub',
              href: 'https://github.com/jerryfane/lightning-agent-tools',
            },
          ],
        },
      ],
      copyright: `Copyright © ${new Date().getFullYear()} Lightning Agent Tools contributors.`,
    },
    prism: {
      additionalLanguages: ['bash', 'toml'],
    },
  },
};

module.exports = config;
