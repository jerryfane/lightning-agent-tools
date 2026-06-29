# Getting Started

This guide gets a new operator from a fresh checkout to a guarded regtest
node-ops workflow. It keeps write authority in `node-ops-daemon`; the MCP server
and agent-facing tools submit bounded requests but do not hold LND write
credentials.

## Choose an installation path

Use the npm package when you only need the released MCP server binary:

```bash
npm install -g @lightninglabs/lightning-mcp-server
lightning-mcp-server --help
```

Use a source checkout when you need local development, regtest, or node-ops
operations:

```bash
git clone https://github.com/lightninglabs/lightning-agent-tools.git
cd lightning-agent-tools
export PATH="$PWD/bin:$PATH"
skills/lightning-mcp-server/scripts/install.sh
```

Node-ops operations are source-checkout workflows because they need
`node-ops-daemon`, the `node-ops` operator CLI wrapper, the scoped macaroon
bakery, and the local regtest harness.

## Local prerequisites

Install these before running the regtest or node-ops flows:

| Tool | Used for |
|------|----------|
| Go 1.24+ | Builds `lightning-mcp-server` and `node-ops-daemon`. |
| Node.js 24.14+ | Builds the Docusaurus documentation site. |
| Docker with Compose | Runs disposable `litd` and `bitcoind` regtest containers. |
| Python 3 | Runs the local `node-ops` helper clients. |
| `jq` | Extracts channel ids, request ids, and audit fields in examples. |

For day-to-day MCP-only use, configure a read-only Lightning Node Connect (LNC)
session and keep write credentials out of the MCP host. For write-path testing,
use the regtest-only node-ops flow below.

## First regtest run

Start a disposable regtest node:

```bash
skills/lnd/scripts/docker-start.sh --regtest
skills/lnd/scripts/create-wallet.sh --container litd --network regtest --mode standalone

ADDR="$(docker exec litd lncli --network=regtest newaddress p2tr | jq -r '.address')"
docker exec litd-bitcoind bitcoin-cli -regtest -rpcuser=devuser -rpcpassword=devpass \
  generatetoaddress 101 "$ADDR"
```

Configure the MCP server for local development and pair it through a read-only
LNC session:

```bash
skills/lightning-mcp-server/scripts/configure.sh --dev
skills/lightning-mcp-server/scripts/setup-claude-config.sh --scope project

docker exec litd litcli --network=regtest sessions add \
  --label dev-mcp --type readonly
```

Copy the one-time pairing phrase into the MCP host's `lnc_connect` tool call.
Do not save that phrase to `.env` or a shell history file.

## First node-ops run

Build the daemon, initialize the operator home, then start the daemon:

```bash
cd node-ops-daemon
make build
cd ..

export PATH="$PWD/bin:$PATH"
node-ops init --container litd --network regtest --force
node-ops daemon
```

`node-ops init` creates `~/.node-ops` with mode `0700`, bakes
`~/.node-ops/node-ops.macaroon`, copies `~/.node-ops/tls.cert`, writes
`~/.node-ops/config.toml`, and creates the operator-only
`~/.node-ops/operator.token` with mode `0600`. The daemon listens on
`~/.node-ops/daemon.sock`; approve and deny commands use the separate
`~/.node-ops/operator.sock`.

In another terminal, verify the daemon and audit state:

```bash
cd lightning-agent-tools
export PATH="$PWD/bin:$PATH"

node-ops status
node-ops pending
node-ops audit --limit 10
node-ops alerts --lines 20
```

The happy path is always:

1. Inspect status and pending work.
2. Use read-only proposal data from MCP, or render a local payload with
   `node-ops propose`.
3. Submit the bounded request with `node-ops submit`.
4. Approve or deny from the operator boundary.
5. Inspect the audit ledger and alerts.

Example fee-set request:

```bash
node-ops propose fee-set --chan-id "$FEE_CHAN_ID" --base-msat 1000 --fee-ppm 100

FEE_RESPONSE="$(node-ops submit fee-set \
  --chan-id "$FEE_CHAN_ID" \
  --base-msat 1000 \
  --fee-ppm 100)"
FEE_REQUEST_ID="$(echo "$FEE_RESPONSE" | jq -r '.request_id')"

node-ops approve fee-set --request-id "$FEE_REQUEST_ID"
node-ops audit --request-id "$FEE_REQUEST_ID" --oldest-first
```

Example rebalance request:

```bash
node-ops propose rebalance \
  --outgoing-chan-id "$OUTGOING_CHAN_ID" \
  --incoming-chan-id "$INCOMING_CHAN_ID" \
  --amount-sat 20000 \
  --max-fee-ppm 500

REBALANCE_RESPONSE="$(node-ops submit rebalance \
  --outgoing-chan-id "$OUTGOING_CHAN_ID" \
  --incoming-chan-id "$INCOMING_CHAN_ID" \
  --amount-sat 20000 \
  --max-fee-ppm 500)"
REBALANCE_REQUEST_ID="$(echo "$REBALANCE_RESPONSE" | jq -r '.request_id')"

node-ops approve rebalance --request-id "$REBALANCE_REQUEST_ID"
node-ops audit --request-id "$REBALANCE_REQUEST_ID" --oldest-first
```

`pending` is not execution. It means the daemon accepted a request for human
review and has not performed the LND write yet.

## Next steps

- Keep the concise command list open in [Quick Reference](quickref.md).
- Use [Dev Environment And Regtest Harness](dev-regtest.md) for the MCP server
  development loop.
- Use [Node-Ops Operations](node-ops-operations.md) for daemon lifecycle,
  monitoring, approvals, and troubleshooting.
- Use [Node-Ops Regtest End-to-End Quickstart](node-ops-regtest-e2e.md) for the
  full two-node fee-set, rebalance, audit, and alert proof.
