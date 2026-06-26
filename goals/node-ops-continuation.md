# Node-Ops Continuation With Codex Shipper

Continue the Lightning node-ops roadmap in `jerryfane/lightning-agent-tools`
using the Codex-backed Gitmoot agent `shipper-codex`. First stabilize the
existing PR stack for issues #1-#8, then continue issues #9-#15 with conservative
parallelism. The intended outcome is a safe, gated node-ops system: read-only
planning tools, a governance daemon, first write actions routed through approval
and hard limits, an operator skill, and an end-to-end regtest proof.

## Core Rules

- Target repo: `jerryfane/lightning-agent-tools`.
- Target base branch: `main`.
- Runtime agent for all new work: `shipper-codex`.
- Do not route new continuation work to the legacy Claude `shipper` agent.
- Do not open or update upstream `lightninglabs/lightning-agent-tools` PRs unless
  explicitly instructed by the human operator.
- Preserve the existing untracked file `lightning-mcp-server/build.err`; do not
  add, edit, delete, or commit it unless a later task explicitly requires it.
- Use one branch and one PR per task or issue.
- Every implementation PR must include tests or an explicit reason why only
  static/docs validation applies.
- Run `codex exec review --uncommitted` on every repo with uncommitted changes
  before commit, and include the exact final raw review output in the PR body.
- Use squash merge if the repo has no discoverable preferred merge method.
- After each merge, update local `main`, verify the worktree is clean except for
  intentionally ignored or pre-existing untracked files, and re-check remaining
  PR mergeability.
- If a task becomes safety-critical, order-dependent, or conflicted, stop
  treating it as parallel and serialize it behind its dependency.

## Before Starting

Run and record these checks:

```sh
git status --short --branch
git remote -v
gh auth status
gitmoot agent doctor shipper-codex
gitmoot daemon status
gitmoot status --repo jerryfane/lightning-agent-tools
```

If the repo is not on `main`, the remote no longer resolves to
`jerryfane/lightning-agent-tools`, GitHub auth fails, or there are unrelated
tracked changes, stop and report the blocker.

## Parallel Strategy

- Stabilization comes first. Do not start issues #9-#15 until PR #23 for issue
  #8 is merged into `jerryfane/lightning-agent-tools/main`.
- During stabilization, merge overlapping MCP tool PRs one at a time because
  #18, #19, #20, and #22 all touch
  `lightning-mcp-server/internal/services/manager.go`.
- After #23 lands, issue #11 and issue #12 may run in parallel if their file
  ownership remains cleanly separated.
- Issue #13 may run in parallel with daemon hardening if it only consumes
  read-only `node_health` output and does not change write-path daemon logic.
- Issue #9 and issue #10 are safety-critical write paths. Do not implement both
  in the same branch. Prefer #9 first, then #10, unless #11 must land first to
  make the execution boundary safe.
- Issue #14 starts only after #9 lands.
- Issue #15 runs last after #10, #12, and #14 are merged.

## Implementation Tasks

### Task 1: Stabilize And Merge Existing PR Stack (#16-#23)

Review and merge the already-open fork PRs in this repo:

1. #16 `docs: dev environment + regtest harness (#1)`
2. #17 `docs: ADR-0001 governance daemon architecture (#2)`
3. #18 `propose_fees` for #3
4. #19 `propose_rebalance` for #4
5. #20 `node_health` for #6
6. #22 `propose_channel_actions` for #5
7. #21 `node-ops scoped macaroon preset` for #7
8. #23 `governance daemon skeleton` for #8

Acceptance criteria:

- Each PR is reviewed with Codex before merge.
- Relevant tests pass for each PR. For Go MCP changes, run tests in
  `lightning-mcp-server`. For `node-ops-daemon`, run `go test ./...` and
  `go build ./...` in that module.
- If a PR is stale but otherwise correct, rebase/update it against current
  `main`, rerun checks, and merge.
- If a PR has a real content conflict or failing test, create a focused fix on
  that PR branch, rerun checks, and continue.
- Close or supersede duplicate/stale blocked Gitmoot task records only if
  Gitmoot requires it to proceed; otherwise leave historical records untouched.

Suggested PR/merge notes:

- Mention the original issue number closed by each PR.
- Record PR URL and merge commit hash in the final Gitmoot result.

### Task 2: Harden Limits Engine Persistence (#11)

Implement issue #11 after #23 is merged.

Acceptance criteria:

- Daily budgets, per-channel cooldowns, and kill-switch state are enforced at
  the daemon execution boundary, not in prompts.
- Required state survives daemon restart.
- Tests cover budget exhaustion, cooldown enforcement, and restart persistence.
- The implementation does not execute real mainnet writes.

Suggested commit message:

```text
feat(daemon): persist node-ops limits state
```

### Task 3: Add Audit Ledger Query Surface (#12)

Implement issue #12 after #23 is merged. This may run in parallel with Task 2 if
it does not modify the same daemon files.

Acceptance criteria:

- Every proposal, approval or denial, execution result, and failure is recorded
  in the append-only ledger.
- Add a read-only query surface for the ledger, preferably through the MCP server
  if it matches repo patterns.
- Tests prove entries are append-only and queryable.

Suggested commit message:

```text
feat(daemon): expose node-ops audit ledger query
```

### Task 4: Implement Gated Fee-Set Execution (#9)

Implement issue #9 after #23 is merged. If Task 2 changes the execution-boundary
limits contract, wait for Task 2 before implementing this task.

Acceptance criteria:

- Add MCP `execute_fee_set` support that routes through the daemon.
- Enforce ppm delta cap, cooldown, budget, approval, kill-switch, and audit log
  before any write execution.
- Execute `UpdateChannelPolicy` only through the scoped node-ops macaroon.
- Tests and/or regtest smoke prove: within-cap approved fee set succeeds,
  over-cap fee set is rejected, denied approval does not execute, and every path
  creates the expected audit entry.
- Mainnet execution must remain impossible in automated tests.

Suggested commit message:

```text
feat(mcp): add gated execute_fee_set
```

### Task 5: Implement Gated Rebalance Execution (#10)

Implement issue #10 after #23 is merged and after any required limits-boundary
work from Task 2.

Acceptance criteria:

- Add gated circular rebalance execution via `bos rebalance` or `SendToRouteV2`.
- Enforce daily sat budget, max ppm, approval, kill-switch, and audit logging.
- Tests and/or regtest smoke prove approved in-cap rebalance succeeds and
  over-cap rebalance is rejected.
- Keep this branch separate from Task 4.

Suggested commit message:

```text
feat(daemon): add gated rebalance execution
```

### Task 6: Add Background Monitoring And Push Alerts (#13)

Implement issue #13 after #20/#6 is merged. This may run in parallel with Tasks
2-5 only if it remains read-only and does not alter write-path daemon behavior.

Acceptance criteria:

- Background monitor polls or subscribes to `node_health`.
- Simulated force-close, peer, or health degradation on regtest creates a push
  alert through a configurable channel or conversational surface.
- Tests cover alert triggering and de-duplication or cooldown behavior.

Suggested commit message:

```text
feat(mcp): add node health alert monitor
```

### Task 7: Add Node-Ops Agent Skill (#14)

Implement issue #14 after Task 4 is merged.

Acceptance criteria:

- Add `skills/node-ops/SKILL.md` with frontmatter and a propose -> approve ->
  execute operator playbook.
- Add skill scripts for observe, propose, and execute paths that call the daemon
  or MCP surfaces; they must not call `lncli` directly for writes.
- Run `skills-ref validate ./skills/node-ops` if available. If unavailable,
  document the missing validator and run a structural/manual validation pass.

Suggested commit message:

```text
feat(skill): add node-ops operator skill
```

### Task 8: End-To-End Regtest Integration And Docs (#15)

Implement issue #15 last, after Tasks 5, 3, and 7 are merged.

Acceptance criteria:

- Fresh regtest flow documents and proves: setup -> bake node-ops macaroon ->
  run daemon -> propose -> approve -> execute fee-set -> execute rebalance ->
  query audit log.
- README or quickstart documents the full operator path.
- The final smoke test is reproducible from the documented commands.

Suggested commit message:

```text
docs(node-ops): add end-to-end regtest quickstart
```

## Gitmoot Execution Commands

After this goal is imported, execute with:

```sh
gitmoot orchestrate shipper-codex --repo jerryfane/lightning-agent-tools "Execute the node-ops continuation goal from goals/node-ops-continuation.md. Start with Task 1 stabilization. After PR #23 is merged, fan out only the tasks marked safe for parallel execution. Keep safety-critical write paths serialized when their limits or execution boundaries overlap. Report PR URLs, merge commits, checks run, and any blockers."
```

For direct task execution, use:

```sh
gitmoot task run <task-id> --repo jerryfane/lightning-agent-tools --owner shipper-codex --base main
```

## Final Report Requirements

Return a `gitmoot_result` JSON object with:

- `decision`: `implemented`, `blocked`, or `failed`.
- `summary`: concise outcome.
- `changes_made`: goal tasks completed and merged.
- `tests_run`: checks by task.
- `needs`: human actions, skipped checks, or remaining blockers.
- `delegations`: any parallel subtasks launched and their final status.

Also include each completed task's branch, PR URL, merge status, and merged
commit hash. Do not claim interactive `/review` is clean; say
`codex exec review is clean; ready for manual /review.`
