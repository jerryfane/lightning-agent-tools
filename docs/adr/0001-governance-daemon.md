# ADR-0001: Governance Daemon Architecture

**Status:** Accepted  
**Date:** 2025-06-25

## Context

AI agents operating on Lightning nodes need to adjust routing fees and rebalance
channels autonomously. Doing this directly through the full-admin `lnd` macaroon
is dangerous: a compromised or misbehaving agent could drain funds, destroy
channels, or set fees that violate operator policy.

We need a long-lived governing process that sits between the agent and the node,
enforces limits, queues borderline actions for human review, and produces a
tamper-evident audit trail.

## Decision

We introduce **node-ops-daemon**, a standalone Go binary that:

1. **Listens on a Unix-domain socket** at `~/.node-ops/daemon.sock` (mode 0600,
   loopback-only access). Wire format: 4-byte big-endian message length followed
   by a JSON body. Requests carry `{action, params}`; responses carry
   `{status, request_id, result?, reason?}`.

2. **Reads a TOML config** from `~/.node-ops/config.toml` with four sections:
   - `[limits]` — `daily_rebalance_budget_sat`, `max_fee_ppm_delta`,
     `per_channel_cooldown`, `rebalance_max_fee_ppm`
   - `[approval]` — `auto_execute_below_ppm_delta`, `require_approval`
   - `[storage]` — ledger SQLite path, killswitch file path

3. **Enforces limits** before any execution:
   - Rejects requests where `|delta_ppm| > max_fee_ppm_delta`
   - Rejects rebalances that exceed `daily_rebalance_budget_sat` or
     `rebalance_max_fee_ppm`
   - Rejects rebalances on a channel still in `per_channel_cooldown`
   - All rejections are recorded in the ledger before returning an error

4. **Routes to approval or auto-execution**:
   - If `require_approval = true` OR `|delta| > auto_execute_below_ppm_delta`,
     the action is queued in an in-memory pending queue and a `"pending"` response
     is returned
   - Otherwise the action is auto-executed via the `NodeExecutor` interface

5. **Maintains an append-only SQLite audit ledger** (`modernc.org/sqlite`, no
   CGO). The `actions` table accepts only `INSERT`s. Every request — whether
   executed, rejected, pending, approved, or failed — generates at least one row.

6. **Implements a file-presence kill-switch**: if the configured `killswitch_file`
   exists when a request arrives, the daemon rejects it immediately and logs the
   rejection, without invoking any executor.

## Out of Scope (Issue #9)

The `NodeExecutor` interface is defined here but its LND/macaroon implementation
is deferred to the next issue. `StubExecutor` is a no-op placeholder that lets
the daemon compile and run end-to-end in tests without a live node.

## Consequences

- The per-session MCP server (`lightning-mcp-server`) proposes actions; the
  daemon decides whether to execute them. This decoupling means compromised MCP
  sessions cannot directly write to the node.
- The approval queue is in-memory only in this skeleton; a future PR can persist
  it to SQLite for crash recovery.
- The 4-byte length prefix caps individual messages at 4 GiB; the daemon enforces
  a 1 MiB soft limit to prevent memory exhaustion.
- Using `modernc.org/sqlite` (pure Go) eliminates CGO, simplifying cross-compilation
  and container builds.

## Alternatives Considered

- **HTTP/JSON-RPC**: more tooling support but requires a port and TLS; Unix socket
  is simpler and inherently loopback-scoped.
- **gRPC**: strong typing but adds proto generation friction; plain JSON is
  sufficient for the narrow action set and is easier to script with standard tools.
- **mattn/go-sqlite3 (CGO)**: well-known but requires a C toolchain; the pure-Go
  alternative is equivalent for our write-once workload.
