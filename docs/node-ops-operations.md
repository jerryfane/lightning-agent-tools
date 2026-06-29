# Node-Ops Operations

`node-ops-daemon` is the enforcement boundary for supported Lightning write
operations. It owns the scoped `node-ops` macaroon, enforces limits and
cooldowns, queues requests for operator review, records the append-only audit
ledger, and pushes monitor alerts. Agents and MCP tools submit requests to the
daemon; they do not use LND write credentials directly.

## Files and sockets

The default operator home is `~/.node-ops`:

| Path | Purpose |
|------|---------|
| `~/.node-ops/config.toml` | Daemon config copied from `node-ops-daemon/config.example.toml`. |
| `~/.node-ops/node-ops.macaroon` | Scoped daemon-owned LND credential. Do not use `admin.macaroon`. |
| `~/.node-ops/tls.cert` | LND TLS certificate copied from the regtest container. |
| `~/.node-ops/daemon.sock` | Local client socket for status, pending, audit, and submit requests. |
| `~/.node-ops/operator.sock` | Separate operator-only socket for approve and deny actions. |
| `~/.node-ops/operator.token` | Operator token read by approve and deny commands. Keep mode `0600`. |
| `~/.node-ops/ledger.db` | Append-only SQLite audit ledger. |
| `~/.node-ops/limits-state.json` | Persisted daily budgets and cooldown state. |
| `~/.node-ops/STOP` | Kill-switch file. If present, execution requests fail closed. |
| `~/.node-ops/alerts.jsonl` | JSONL alert stream when file alerts are enabled. |

Override client paths with:

```bash
export NODE_OPS_DAEMON_SOCKET="$HOME/.node-ops/daemon.sock"
export NODE_OPS_OPERATOR_SOCKET="$HOME/.node-ops/operator.sock"
export NODE_OPS_OPERATOR_TOKEN_FILE="$HOME/.node-ops/operator.token"
export NODE_OPS_ALERT_FILE="$HOME/.node-ops/alerts.jsonl"
```

## Initialize and start

From a source checkout, build the daemon and initialize the operator home:

```bash
cd node-ops-daemon
make build
cd ..

export PATH="$PWD/bin:$PATH"
node-ops init --container litd --network regtest --force
node-ops daemon
```

If the regtest LND gRPC port was overridden, pass it during initialization:

```bash
node-ops init \
  --container litd \
  --network regtest \
  --lnd-rpc "127.0.0.1:${LND_GRPC_PORT:-10009}" \
  --force
```

The daemon accepts only `required_network = "regtest"` for gated writes. This is
intentional: the current write path is a disposable-regtest proof, not a mainnet
automation surface.

## Observe the daemon

Use read-only commands before and after every request:

```bash
cd /path/to/lightning-agent-tools
export PATH="$PWD/bin:$PATH"

node-ops status
node-ops pending
node-ops audit --limit 20
node-ops audit --status pending --limit 20
node-ops audit --action execute_fee_set --oldest-first
node-ops alerts --lines 20
node-ops watch --interval 2 status
```

`status` should report the daemon state, kill-switch state, monitor state, and
any monitor error. `pending` lists requests waiting for operator review. `audit`
queries durable request history; use `--request-id`, `--action`, `--status`,
`--limit`, `--offset`, and `--oldest-first` to narrow results.

## Proposal flow

Use MCP read-only proposal tools first when an agent is connected through LNC:

```text
Call lnc_propose_fees with {"days":7,"min_fee_ppm":1,"max_fee_ppm":500}.
Call lnc_propose_rebalance with {"max_candidates":5,"max_fee_ppm":500}.
```

Render the exact local payload before submitting:

```bash
node-ops propose fee-set \
  --chan-id "$FEE_CHAN_ID" \
  --base-msat 1000 \
  --fee-ppm 100

node-ops propose rebalance \
  --outgoing-chan-id "$OUTGOING_CHAN_ID" \
  --incoming-chan-id "$INCOMING_CHAN_ID" \
  --amount-sat 20000 \
  --max-fee-ppm 500
```

The proposal output includes daemon status, existing pending requests, the MCP
tool name, the daemon action, request parameters, and a review checklist. Stop
instead of submitting if the kill-switch is active, the daemon reports an error,
the request would exceed a limit, or there is already conflicting pending work.

## Submit, approve, deny, and execute

Submit supported write requests only through MCP `lnc_execute_*` tools or the
local `node-ops submit` wrapper:

```bash
FEE_RESPONSE="$(node-ops submit fee-set \
  --chan-id "$FEE_CHAN_ID" \
  --base-msat 1000 \
  --fee-ppm 100)"
FEE_REQUEST_ID="$(echo "$FEE_RESPONSE" | jq -r '.request_id')"

REBALANCE_RESPONSE="$(node-ops submit rebalance \
  --outgoing-chan-id "$OUTGOING_CHAN_ID" \
  --incoming-chan-id "$INCOMING_CHAN_ID" \
  --amount-sat 20000 \
  --max-fee-ppm 500)"
REBALANCE_REQUEST_ID="$(echo "$REBALANCE_RESPONSE" | jq -r '.request_id')"
```

Expected submit status is `pending`. Approve or deny from the operator boundary:

```bash
node-ops approve fee-set --request-id "$FEE_REQUEST_ID"
node-ops deny fee-set --request-id "$FEE_REQUEST_ID" --reason "outside policy"

node-ops approve rebalance --request-id "$REBALANCE_REQUEST_ID"
node-ops deny rebalance --request-id "$REBALANCE_REQUEST_ID" --reason "route too expensive"
```

After approval, inspect the ledger:

```bash
node-ops audit --request-id "$FEE_REQUEST_ID" --oldest-first
node-ops audit --request-id "$REBALANCE_REQUEST_ID" --oldest-first
```

Successful requests should move from `pending` to `executed`. Denied requests
should remain non-executed and record the operator reason. Rejected or failed
requests should preserve the daemon reason so the operator can fix the input and
submit a lower-risk retry.

## Monitoring and alerts

The monitor polls read-only node health and can push JSONL alerts to
`~/.node-ops/alerts.jsonl`:

```bash
node-ops status | jq '.result.monitor'
node-ops alerts --lines 20
node-ops alerts --follow
```

The regtest configuration produced by `node-ops init` enables file alerts,
shortens the poll interval, and writes the alert path into `config.toml`.
Production-like operators should lengthen `poll_interval` and `alert_cooldown`
before running persistent test infrastructure.

## Troubleshooting

| Symptom | What to check |
|---------|---------------|
| `node-ops daemon: missing ... node-ops-daemon` | Run `cd node-ops-daemon && make build && cd ..`. |
| `node-ops daemon: missing ~/.node-ops/config.toml` | Run `node-ops init --container litd --network regtest`. |
| Socket connection fails | Confirm `node-ops daemon` is still running and the client uses `~/.node-ops/daemon.sock`. |
| Approve or deny fails | Confirm `~/.node-ops/operator.sock` exists and `~/.node-ops/operator.token` has mode `0600`. |
| Writes are rejected immediately | Check `node-ops status`, remove `~/.node-ops/STOP` if the kill-switch was intentionally cleared, and confirm `required_network = "regtest"`. |
| Request stays pending | Use `node-ops pending`, approve or deny the request, then query `node-ops audit --request-id <id> --oldest-first`. |
| Fee-set rejected | Check `max_fee_ppm_delta`, `daily_fee_ppm_budget`, per-channel cooldown, and current channel policy. |
| Rebalance rejected | Check `daily_rebalance_budget_sat`, `rebalance_max_fee_ppm`, channel cooldowns, and whether a bounded route exists. |
| Monitor error appears in status | Verify the daemon can read LND through the configured `lnd_rpc`, macaroon, and TLS cert. |
| No alerts file exists | Wait for a trigger, or confirm `[monitor] enabled = true` and `alert_channel = "file"` in `config.toml`. |

For a full proof that opens regtest channels, submits fee-set and rebalance
requests, approves them, queries the audit ledger, and verifies alerts, use
[Node-Ops Regtest End-to-End Quickstart](node-ops-regtest-e2e.md).
