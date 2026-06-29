---
name: node-ops
description: Operate bounded Lightning node fee-set and circular-rebalance workflows through the node-ops daemon and MCP tools. Use when Codex needs to observe node health or audit state, propose fee/rebalance actions, submit gated execute_fee_set or execute_rebalance requests, or help a human operator approve or deny pending node-ops actions without exposing write credentials.
---

# Node Ops

Use this skill for the operator loop around `node-ops-daemon`: observe, propose,
submit a gated request, then wait for a human/operator approval before any write
can execute. The daemon owns the scoped node-ops macaroon, persistent limits,
operator socket, kill-switch, and audit ledger. The MCP server and scripts are
thin clients only.

## Safety Invariants

- Never call `lncli`, `bos`, direct gRPC write APIs, or high-level payment RPCs
  for node-ops writes.
- Use read-only MCP tools for observation and proposals:
  `lnc_node_health`, `lnc_propose_fees`, `lnc_propose_rebalance`,
  `lnc_propose_channel_actions`, and `lnc_query_node_ops_audit`.
- Use only `lnc_execute_fee_set`, `lnc_execute_rebalance`, or the bundled
  `scripts/execute.py` wrapper to submit write requests. These queue work in the
  daemon; they do not grant the model write credentials.
- Treat the operator socket and token as human/operator-only. Do not approve,
  deny, read, copy, or request the operator token unless the human explicitly
  asks for an operator action in that environment.
- Stop on kill-switch, budget, cooldown, missing daemon, missing audit, stale
  pending request, route-not-found, or non-regtest execution errors. Report the
  daemon reason verbatim.

## Operator Playbook

1. Observe current state.
   - Query node health and balances through MCP read-only tools.
   - Query daemon status, pending requests, and recent audit entries:
     `skills/node-ops/scripts/observe.py status`,
     `skills/node-ops/scripts/observe.py pending`, and
     `skills/node-ops/scripts/observe.py audit --limit 20`.
2. Propose before executing.
   - Fee-set: use `lnc_propose_fees`; prefer the smallest fee change that fixes
     the routing signal. Check current ppm, proposed ppm, absolute ppm delta,
     daily fee-ppm budget, and per-channel cooldown.
   - Rebalance: use `lnc_propose_rebalance`; outgoing channel should have excess
     local balance, incoming channel should need local balance. Keep amount small
     enough to preserve reserves and set `max_fee_ppm` below the daemon cap.
   - Render the exact request payload with `scripts/propose.py fee-set ...` or
     `scripts/propose.py rebalance ...` for review.
3. Submit the gated request.
   - Prefer MCP tools when working inside an MCP session:
     `lnc_execute_fee_set` or `lnc_execute_rebalance`.
   - Use `scripts/execute.py fee-set ...` or `scripts/execute.py rebalance ...`
     only as a local daemon client. Expected status is `pending`.
4. Human operator reviews and approves or denies out of band.
   - Approval runs through the separate operator socket with the private token.
   - If the human explicitly asks you to operate that boundary, use
     `scripts/execute.py approve-fee-set`, `deny-fee-set`,
     `approve-rebalance`, or `deny-rebalance`.
5. Verify after decision.
   - Re-query audit entries by `request_id`.
   - Confirm status transitions: `pending -> approved -> accepted -> executed`
     for success, or `pending -> denied` / `rejected` / `failed` otherwise.
   - Report executed fields: channel IDs, fee ppm/base msat, amount sat,
     max fee ppm, actual fee, payment hash, and daemon reason or warning.

## Scripts

All scripts speak the daemon's length-prefixed JSON Unix-socket protocol and
never call `lncli` or direct LND APIs.

```bash
# Read-only daemon observation.
skills/node-ops/scripts/observe.py status
skills/node-ops/scripts/observe.py pending
skills/node-ops/scripts/observe.py audit --action execute_rebalance --limit 20

# Render reviewable payloads without submitting writes.
skills/node-ops/scripts/propose.py fee-set --chan-id 123 --base-msat 1000 --fee-ppm 250
skills/node-ops/scripts/propose.py rebalance --outgoing-chan-id 11 --incoming-chan-id 22 --amount-sat 50000 --max-fee-ppm 500

# Submit gated requests. These should return status=pending unless rejected.
skills/node-ops/scripts/execute.py fee-set --chan-id 123 --base-msat 1000 --fee-ppm 250
skills/node-ops/scripts/execute.py rebalance --outgoing-chan-id 11 --incoming-chan-id 22 --amount-sat 50000 --max-fee-ppm 500

# Operator-only approval or denial when explicitly instructed by the human.
skills/node-ops/scripts/execute.py approve-fee-set --request-id <id>
skills/node-ops/scripts/execute.py deny-rebalance --request-id <id> --reason "too expensive"
```

Environment overrides:

- `NODE_OPS_DAEMON_SOCKET`: execution socket, default `~/.node-ops/daemon.sock`.
- `NODE_OPS_OPERATOR_SOCKET`: operator socket, default `~/.node-ops/operator.sock`.
- `NODE_OPS_OPERATOR_TOKEN_FILE`: operator token file, default
  `~/.node-ops/operator.token`.

## Edge Cases

- `pending` is not success. It means the daemon accepted the request for human
  review.
- Cooldowns apply to both channels in a rebalance. A recent fee-set on either
  channel can block a rebalance.
- Rebalance fee caps are enforced in millisatoshis. Do not round user-visible
  fee caps up to whole sats when deciding whether an action is safe.
- A failed execution should roll back reserved budgets and cooldowns; verify by
  querying the audit ledger and, when needed, submitting a lower-risk retry.
- The daemon is regtest-gated for write execution. Treat any non-regtest or
  concrete-executor startup error as a hard stop, not as a prompt-only warning.
