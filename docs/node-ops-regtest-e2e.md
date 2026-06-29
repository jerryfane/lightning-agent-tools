# Node-Ops Regtest End-to-End Quickstart

This runbook proves the bounded node-ops path on a disposable regtest stack:
setup -> bake the scoped `node-ops` macaroon -> run `node-ops-daemon` ->
propose -> submit gated fee-set and rebalance requests -> approve from the
operator boundary -> query the audit ledger.

> Regtest only. The daemon rejects non-regtest write execution, but this flow is
> intentionally destructive to a throwaway node. Do not reuse these credentials
> or channels on mainnet.

## Prerequisites

- Go 1.24+.
- Docker with Compose.
- `jq`.
- Bash in a source checkout of this repository.

This is a source-checkout runbook. The published npm package includes this
document for reference, but it does not ship the `node-ops-daemon` source tree;
clone the repository before running the daemon build and config commands below.

Use one terminal for the regtest stack, one for `node-ops-daemon`, and one for
the operator/client commands below.

## 1. Build the local binaries

```bash
skills/lightning-mcp-server/scripts/install.sh

cd node-ops-daemon
make build
cd ..
```

The daemon binary is `node-ops-daemon/node-ops-daemon`.

## 2. Start a two-node regtest stack

Start the primary node plus Bitcoin Core, then enable the second litd node used
for channel and rebalance testing:

```bash
skills/lnd/scripts/docker-start.sh --regtest
docker compose -f skills/lnd/templates/docker-compose-regtest.yml \
  --profile two-node up -d litd-bob
```

Create both wallets:

```bash
skills/lnd/scripts/create-wallet.sh --container litd --network regtest --mode standalone
skills/lnd/scripts/create-wallet.sh --container litd-bob --network regtest --mode standalone
```

Fund the primary node and mature the coinbase outputs:

```bash
ALICE_ADDR="$(docker exec litd lncli --network=regtest newaddress p2tr | jq -r '.address')"
docker exec litd-bitcoind bitcoin-cli -regtest -rpcuser=devuser -rpcpassword=devpass \
  generatetoaddress 101 "$ALICE_ADDR"
```

Open three public channels from `litd` to `litd-bob`. The first channel is used
only for the fee-set proof. The second and third channels give the rebalance
executor separate outgoing and incoming local channels, so the fee-set
cooldown does not block the later rebalance proof.

```bash
BOB_PUBKEY="$(docker exec litd-bob lncli --network=regtest getinfo | jq -r '.identity_pubkey')"
docker exec litd lncli --network=regtest connect "$BOB_PUBKEY@litd-bob:9735"

docker exec litd lncli --network=regtest openchannel \
  --node_key="$BOB_PUBKEY" \
  --local_amt=1000000 \
  --push_amt=500000

MINING_ADDR="$(docker exec litd lncli --network=regtest newaddress p2tr | jq -r '.address')"
docker exec litd-bitcoind bitcoin-cli -regtest -rpcuser=devuser -rpcpassword=devpass \
  generatetoaddress 6 "$MINING_ADDR"

docker exec litd lncli --network=regtest openchannel \
  --node_key="$BOB_PUBKEY" \
  --local_amt=1000000 \
  --push_amt=200000

MINING_ADDR="$(docker exec litd lncli --network=regtest newaddress p2tr | jq -r '.address')"
docker exec litd-bitcoind bitcoin-cli -regtest -rpcuser=devuser -rpcpassword=devpass \
  generatetoaddress 6 "$MINING_ADDR"

docker exec litd lncli --network=regtest openchannel \
  --node_key="$BOB_PUBKEY" \
  --local_amt=1000000 \
  --push_amt=800000

MINING_ADDR="$(docker exec litd lncli --network=regtest newaddress p2tr | jq -r '.address')"
docker exec litd-bitcoind bitcoin-cli -regtest -rpcuser=devuser -rpcpassword=devpass \
  generatetoaddress 6 "$MINING_ADDR"
```

Confirm both nodes see the channels:

```bash
docker exec litd lncli --network=regtest listchannels | jq '.channels[] | {
  chan_id,
  remote_pubkey,
  capacity,
  local_balance,
  remote_balance
}'
docker exec litd-bob lncli --network=regtest listchannels | jq '.channels[] | {
  chan_id,
  remote_pubkey,
  capacity,
  local_balance,
  remote_balance
}'
```

Export the channel ids used by later steps:

```bash
mapfile -t NODE_OPS_CHANNELS < <(
  docker exec litd lncli --network=regtest listchannels | jq -r '.channels[].chan_id'
)
if [ "${#NODE_OPS_CHANNELS[@]}" -lt 3 ]; then
  echo "expected at least three node-ops channels" >&2
  exit 1
fi
FEE_CHAN_ID="${NODE_OPS_CHANNELS[0]}"
OUTGOING_CHAN_ID="${NODE_OPS_CHANNELS[1]}"
INCOMING_CHAN_ID="${NODE_OPS_CHANNELS[2]}"
```

## 3. Connect the MCP read-only LNC session

The proposal tools (`lnc_propose_fees`, `lnc_propose_rebalance`, and related
read-only MCP tools) require an active Lightning Node Connect session. Configure
the MCP server for regtest and register it with your MCP host:

```bash
skills/lightning-mcp-server/scripts/configure.sh --dev
skills/lightning-mcp-server/scripts/setup-claude-config.sh --scope project
```

Restart the MCP host after registration. Then mint a one-time read-only LNC
pairing phrase from the primary node:

```bash
docker exec litd litcli --network=regtest sessions add \
  --label node-ops-e2e --type readonly
```

Copy the printed `pairing_secret_mnemonic` and password from the command output,
then call the `lnc_connect` MCP tool in your agent session with those values.
Do not write the one-time pairing phrase to `.env` or another file.

The shell-only daemon client commands below still work without LNC, but the MCP
proposal calls in the fee-set and rebalance sections will fail until
`lnc_connect` succeeds.

## 4. Bake the scoped node-ops macaroon

The daemon receives the node write credential. The MCP server and skill scripts
do not.

```bash
mkdir -p "$HOME/.node-ops"
chmod 700 "$HOME/.node-ops"

skills/macaroon-bakery/scripts/bake.sh \
  --container litd \
  --network regtest \
  --role node-ops \
  --save-to "$HOME/.node-ops/node-ops.macaroon"

skills/macaroon-bakery/scripts/bake.sh \
  --container litd \
  --network regtest \
  --inspect "$HOME/.node-ops/node-ops.macaroon"
```

The printed permissions should include `UpdateChannelPolicy` and
`SendToRouteV2`, plus read RPCs needed to bound the action. It should not include
open-channel, close-channel, or high-level payment RPCs.

Copy the regtest TLS certificate from the container:

```bash
docker cp litd:/root/.lnd/tls.cert "$HOME/.node-ops/tls.cert"
chmod 600 "$HOME/.node-ops/node-ops.macaroon" "$HOME/.node-ops/tls.cert"
```

## 5. Configure the daemon and operator token

```bash
cp node-ops-daemon/config.example.toml "$HOME/.node-ops/config.toml"
python3 - <<'PY'
from pathlib import Path

path = Path.home() / ".node-ops" / "config.toml"
body = path.read_text()
body = body.replace('macaroon = "~/.node-ops/node-ops.macaroon"',
                    f'macaroon = "{Path.home()}/.node-ops/node-ops.macaroon"')
body = body.replace('tls_cert = "~/.lnd/tls.cert"',
                    f'tls_cert = "{Path.home()}/.node-ops/tls.cert"')
body = body.replace('approval_token_file = "~/.node-ops/operator.token"',
                    f'approval_token_file = "{Path.home()}/.node-ops/operator.token"')
body = body.replace('required_network = "regtest"', 'required_network = "regtest"')
path.write_text(body)
PY

python3 - <<'PY'
import secrets
from pathlib import Path

path = Path.home() / ".node-ops" / "operator.token"
path.write_text(secrets.token_urlsafe(32) + "\n")
path.chmod(0o600)
PY
```

Keep `~/.node-ops/operator.token` outside the MCP host process. It is the
operator-only approval credential.

## 6. Run the daemon

In a separate terminal:

```bash
node-ops-daemon/node-ops-daemon "$HOME/.node-ops/config.toml"
```

In the client terminal:

```bash
skills/node-ops/scripts/observe.py status
skills/node-ops/scripts/observe.py pending
skills/node-ops/scripts/observe.py audit --limit 10
```

Expected result: status is `ok`, `pending` is empty, and the audit query returns
ledger entries for the read-only checks.

## 7. Propose and execute a gated fee-set

First use read-only proposal data from MCP in your agent session:

```text
Call lnc_propose_fees with {"days":7,"min_fee_ppm":1,"max_fee_ppm":500}.
Choose a channel whose proposed fee delta is within the daemon limits.
```

Render the exact local payload before submitting:

```bash
skills/node-ops/scripts/propose.py fee-set \
  --chan-id "$FEE_CHAN_ID" \
  --base-msat 1000 \
  --fee-ppm 100
```

Submit the request through the gated daemon path. In an MCP session, call
`lnc_execute_fee_set` with the same `chan_id`, `base_msat`, and `fee_ppm`
fields. From the shell, use the bundled daemon client:

```bash
FEE_RESPONSE="$(skills/node-ops/scripts/execute.py fee-set \
  --chan-id "$FEE_CHAN_ID" \
  --base-msat 1000 \
  --fee-ppm 100)"
echo "$FEE_RESPONSE" | jq .
FEE_REQUEST_ID="$(echo "$FEE_RESPONSE" | jq -r '.request_id')"
```

Expected result: the daemon returns `status: "pending"`. Pending is not
execution; it only means the request is queued for operator review.

Approve from the operator boundary:

```bash
skills/node-ops/scripts/execute.py approve-fee-set --request-id "$FEE_REQUEST_ID" | jq .
```

Verify the channel policy and audit trail:

```bash
docker exec litd lncli --network=regtest feereport | jq --arg chan "$FEE_CHAN_ID" \
  '.channel_fees[] | select((.chan_id | tostring) == $chan)'

skills/node-ops/scripts/observe.py audit --request-id "$FEE_REQUEST_ID" --oldest-first | jq .
```

Expected audit statuses for the original request: `pending` followed by
`executed`. The operator approval request is a separate audit entry that points
back to the original request id.

## 8. Propose and execute a gated rebalance

A circular rebalance needs at least two local channels on the executing node.
The two-channel setup above exports `OUTGOING_CHAN_ID` and `INCOMING_CHAN_ID`.
If the proposal tool reports no candidates, create a more imbalanced pair before
continuing.

Use read-only MCP proposal data first:

```text
Call lnc_propose_rebalance with {"max_candidates":5,"max_fee_ppm":500}.
Pick outgoing_chan_id, incoming_chan_id, amount_sat, and max_fee_ppm from a
candidate that stays inside the daemon limits.
```

Use the selected channels and amount:

```bash
AMOUNT_SAT=20000
MAX_FEE_PPM=500
```

Render and submit the request. In an MCP session, call
`lnc_execute_rebalance` with the same `outgoing_chan_id`, `incoming_chan_id`,
`amount_sat`, and `max_fee_ppm` fields. From the shell, use the bundled daemon
client:

```bash
skills/node-ops/scripts/propose.py rebalance \
  --outgoing-chan-id "$OUTGOING_CHAN_ID" \
  --incoming-chan-id "$INCOMING_CHAN_ID" \
  --amount-sat "$AMOUNT_SAT" \
  --max-fee-ppm "$MAX_FEE_PPM"

REBALANCE_RESPONSE="$(skills/node-ops/scripts/execute.py rebalance \
  --outgoing-chan-id "$OUTGOING_CHAN_ID" \
  --incoming-chan-id "$INCOMING_CHAN_ID" \
  --amount-sat "$AMOUNT_SAT" \
  --max-fee-ppm "$MAX_FEE_PPM")"
echo "$REBALANCE_RESPONSE" | jq .
REBALANCE_REQUEST_ID="$(echo "$REBALANCE_RESPONSE" | jq -r '.request_id')"
```

Approve from the operator boundary:

```bash
skills/node-ops/scripts/execute.py approve-rebalance \
  --request-id "$REBALANCE_REQUEST_ID" | jq .
```

Verify the audit trail:

```bash
skills/node-ops/scripts/observe.py audit \
  --request-id "$REBALANCE_REQUEST_ID" \
  --oldest-first | jq .
```

Expected audit statuses for the original request: `pending` followed by
`executed`. If lnd cannot find a bounded route, the request should become
`failed`, the daemon should report the LND reason, and reserved budget/cooldown
state should roll back.

## 9. Query the full ledger

The audit query can be run through MCP or the local read-only client:

```text
Call lnc_query_node_ops_audit with
{"action":"execute_fee_set","limit":20,"newest_first":false}.

Call lnc_query_node_ops_audit with
{"action":"execute_rebalance","limit":20,"newest_first":false}.
```

```bash
skills/node-ops/scripts/observe.py audit --action execute_fee_set --oldest-first | jq .
skills/node-ops/scripts/observe.py audit --action execute_rebalance --oldest-first | jq .
```

The proof is complete when the ledger shows:

- `execute_fee_set` submitted as `pending` and later `executed`.
- `approve_fee_set` recorded as `approved` on the operator socket.
- `execute_rebalance` submitted as `pending` and later `executed`, or a
  bounded `failed` result when no route is available.
- `approve_rebalance` recorded as `approved` on the operator socket.
- No MCP node-ops submit or approval path used `lncli`, `bos`, or an LND write
  API directly.

## Negative controls

Run at least one rejection before teardown:

```bash
skills/node-ops/scripts/execute.py fee-set \
  --chan-id "$FEE_CHAN_ID" \
  --base-msat 1000 \
  --fee-ppm 1000000 || true

touch "$HOME/.node-ops/STOP"
skills/node-ops/scripts/execute.py rebalance \
  --outgoing-chan-id "$OUTGOING_CHAN_ID" \
  --incoming-chan-id "$INCOMING_CHAN_ID" \
  --amount-sat "$AMOUNT_SAT" \
  --max-fee-ppm "$MAX_FEE_PPM" || true
rm -f "$HOME/.node-ops/STOP"
```

Each rejection must have an audit entry with the daemon reason.

## Teardown

Stop the daemon with `Ctrl-C`, then remove the disposable regtest volumes:

```bash
docker compose -f skills/lnd/templates/docker-compose-regtest.yml down -v
rm -rf "$HOME/.node-ops"
```
