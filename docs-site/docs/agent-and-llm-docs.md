---
title: Agent and LLM Docs
description: How coding agents should use AGENTS.md, llms.txt, and llms-full.txt.
sidebar_position: 2
---

# Agent and LLM Docs

Lightning Agent Tools exposes three agent-facing documentation surfaces:
`AGENTS.md`, `/llms.txt`, and `/llms-full.txt`. On GitHub Pages they are
available at:

- [llms.txt](https://jerryfane.github.io/lightning-agent-tools/llms.txt)
- [llms-full.txt](https://jerryfane.github.io/lightning-agent-tools/llms-full.txt)

They serve different readers and should not be edited as substitutes for one
another.

## `AGENTS.md`

`AGENTS.md` is the repo-local instruction file for coding agents. It gives
operational guidance that an agent needs before changing files: important
directories, local checks, Gitmoot workflow expectations, node-ops safety
boundaries, credential handling, and outputs that must not be committed.

Use it when an agent is about to inspect, edit, test, review, or ship changes in
this repository. It is intentionally concise and task-oriented; it should not
duplicate the README or replace product documentation.

The format follows the convention described by [agents.md](https://agents.md/):
keep project-specific instructions in a predictable repo-root file that coding
agents can discover.

## `/llms.txt`

`/llms.txt` is the concise LLM index emitted by the Docusaurus build. It points
agents to the important documentation pages in Markdown-friendly form, so it is
the best starting point when an assistant needs a compact map of the docs.

Use it when an LLM needs quick context, link targets, or document selection
before deciding which page to read in full. It is not a sitemap for crawlers and
it is not a replacement for the human documentation navigation.

The file follows the approach described by [llmstxt.org](https://llmstxt.org/):
provide a Markdown file at a predictable path so language models can consume the
most relevant documentation without scraping the whole site.

## `/llms-full.txt`

`/llms-full.txt` is the full generated documentation bundle. It combines the
Docusaurus documentation content into one AI-friendly Markdown file.

Use it when an agent needs broad offline context or when a workflow benefits
from reading the whole docs set in one request. It is larger than `/llms.txt`,
so prefer the concise index first when the task only needs routing or a small
set of pages.

## How These Files Differ

- README: explains the project to people and gives installation, architecture,
  and usage entry points.
- Human docs: provide canonical task and concept documentation for readers using
  the Docusaurus site or the source `docs/` tree.
- Sitemap: helps search engines and site tooling discover URLs.
- `AGENTS.md`: gives coding agents repository-specific operating rules.
- `/llms.txt`: gives LLMs a concise generated index of documentation.
- `/llms-full.txt`: gives LLMs a generated single-file copy of the docs.

## Source Of Truth

The LLM files are generated from canonical Docusaurus docs during `npm run
build` in `docs-site/`. Do not manually edit generated `llms.txt`,
`llms-full.txt`, Markdown mirrors, or build output. Update the source docs, run
the docs build, and let the generator produce the LLM-facing files.
