# Agent Instructions

Lightning Agent Tools packages an MCP server, a guarded node-ops daemon, and
agent skills for Lightning Network development and regtest workflows. Treat this
repository as operational tooling: preserve safety boundaries, keep docs and
generated outputs in sync with source, and avoid broad rewrites unless a task
explicitly asks for them.

## Important Directories

- `lightning-mcp-server/`: Go MCP server for Lightning Node Connect read tools
  plus thin clients for daemon-gated node-ops requests.
- `node-ops-daemon/`: Go daemon that owns write credentials, policy limits,
  approval queues, kill-switch checks, and audit logging.
- `skills/`: agent skill instructions, helper scripts, and templates. Do not
  edit generated or vendored skill outputs unless the task owns them.
- `docs/`: canonical repository documentation that is mirrored into the
  Docusaurus site.
- `docs-site/`: Docusaurus source, generated docs bridge, and LLM-friendly
  output configuration.
- `tests/`: repository-level test assets and fixtures.

## Build And Test Commands

Run the narrowest relevant gate first, then broaden before committing.

```sh
cd lightning-mcp-server && make unit
cd lightning-mcp-server && make build
cd node-ops-daemon && make unit
cd node-ops-daemon && make build
cd docs-site && npm ci && npm run build
git diff --check
```

For MCP server code that changes formatting, imports, modules, or lint-sensitive
paths, prefer `cd lightning-mcp-server && make check` when the local toolchain
has `golangci-lint`.

## Gitmoot Workflow

- Work on the task branch Gitmoot assigned; never push directly to `main`.
- Respect Gitmoot branch and checkout locks. If another task owns a file, stop
  and coordinate instead of editing through the conflict.
- Keep changes scoped to the issue or goal file. Do not opportunistically edit
  generated scripts, workflows, or broad docs.
- Use Conventional Commits and reference the issue or task when the job asks for
  it.
- Before opening a PR, run the task's listed checks and `codex exec review
  --uncommitted`; address review findings until clean or document a genuine
  false positive.

## Current Shipper-Codex Guidance

`shipper-codex` agents are expected to carry assigned Gitmoot implementation
tasks through implementation, local verification, review, commit, push, PR
creation, and final status reporting. Prefer repo-local patterns over new
abstractions, and report blockers with the exact command or external state that
blocked progress.

## Node-Ops Safety

- Direct MCP Lightning node access is read-only. Write operations such as fee
  changes and rebalances must go through `node-ops-daemon`.
- The daemon enforces configured caps, cooldowns, daily budgets, operator
  approval, kill-switch state, and append-only audit records before executing
  LND writes.
- Keep the MCP server as a thin client for node-ops requests; it must not gain
  LND write credentials.
- Do not bypass the approval socket, operator token, limit store, kill-switch,
  or audit ledger in tests or production paths.

## Credentials And Macaroons

- Never commit Lightning credentials, pairing phrases, TLS certs, macaroon
  files, node databases, wallet data, or exported signer bundles.
- Do not give agents `admin.macaroon` in production. Use scoped macaroons such
  as `pay-only`, `invoice-only`, `read-only`, or `node-ops` for the exact task.
- Daemon-owned node-ops macaroons belong under local operator control, commonly
  `~/.node-ops/`, with restrictive file permissions.
- Treat LNC pairing secrets as sensitive and single-use; do not paste them into
  commits, issues, PR descriptions, or generated docs.

## Do Not Commit

- `docs-site/build/`, `docs-site/.docusaurus/`, `node_modules/`, coverage
  reports, compiled binaries, caches, logs, and temporary test output.
- Local runtime state such as `~/.lnd/`, `~/.lit/`, `~/.lnget/`,
  `~/.node-ops/`, macaroons, TLS certs, wallets, and signer credential bundles.
- Generated LLM output files from the Docusaurus build. Change the source docs
  and rebuild instead.
