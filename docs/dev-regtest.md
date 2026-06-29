# Dev environment & regtest harness

The development loop for working on `lightning-mcp-server` and the node-ops
write path: **build the server, stand up a throwaway regtest litd node, and run
the read tools or gated node-ops tools against it.**

> ⚠️ **Regtest only.** Never point experimental or write-path work at a mainnet
> node. Regtest coins are worthless and the chain is disposable — that is the
> entire point of this harness.

## Prerequisites

| Tool | Why | Notes |
|------|-----|-------|
| **Go 1.24+** | builds the MCP server | `go.mod` pins `go 1.24` (toolchain `go1.24.x`) |
| **Docker + Compose** | runs the regtest `litd` + `bitcoind` stack | required for steps 2–6 |
| `jq`, `gh` | helper scripting | |

## 1. Build the MCP server

```bash
# via the skill script (compiles + installs to $GOPATH/bin)
skills/lightning-mcp-server/scripts/install.sh

# …or directly from the module
cd lightning-mcp-server && go build -o ./lightning-mcp-server .
```

## 2. Bring up a regtest stack (litd + bitcoind)

```bash
skills/lnd/scripts/docker-start.sh --regtest
# equivalently:
#   docker compose -f skills/lnd/templates/docker-compose-regtest.yml up -d
```

This starts two containers:

| Container | Exposes |
|-----------|---------|
| `litd` | lnd gRPC `:10009`, REST `:8080`, LiT HTTPS `:8443`, P2P `:9735` |
| `litd-bitcoind` | Bitcoin Core regtest RPC `:18443`, ZMQ `:28332/:28333` |

## 3. Create a wallet and fund the node

```bash
skills/lnd/scripts/create-wallet.sh --container litd --mode standalone

# mine 101 blocks to a node address (101 = coinbase maturity + 1)
docker exec litd-bitcoind bitcoin-cli -regtest -rpcuser=devuser -rpcpassword=devpass \
  generatetoaddress 101 \
  "$(docker exec litd lncli --network=regtest newaddress p2tr | jq -r '.address')"
```

## 4. Generate an LNC pairing phrase (read-only session)

The MCP server connects over **Lightning Node Connect** using a one-time,
BIP39-style **pairing phrase** minted by `litd`. Use a **read-only** session so the
dev tunnel carries least privilege:

```bash
docker exec litd litcli --network=regtest sessions add \
  --label dev-mcp --type readonly
# copy the 10-word `pairing_secret_mnemonic` from the output
```

LNC routes both ends outbound to a mailbox relay (no inbound ports, no TLS certs,
no macaroons on disk); the pairing phrase is single-use and the MCP server keeps
only an ephemeral in-memory keypair for the session.

## 5. Point the MCP server at regtest

```bash
# dev mode (insecure TLS for the local mailbox); writes lightning-mcp-server/.env
skills/lightning-mcp-server/scripts/configure.sh --dev

# then pass the pairing phrase and password to the `lnc_connect` tool.
# Do not write the one-time pairing phrase to `.env` or any other file.
```

## 6. Run the tools end-to-end

Register the built server with an MCP host (e.g. Claude Code) or run it directly,
then exercise the read tools and confirm live data comes back from the regtest
node:

- `lnc_get_info` → node pubkey, synced height, version
- `lnc_list_channels`, `lnc_pending_channels`
- `lnc_get_balance` → wallet and channel balances
- `lnc_describe_graph`, `lnc_list_peers`

The LNC-backed tools are read-only. The `lnc_execute_fee_set` tool is the first
daemon-gated write path: it submits a request to `node-ops-daemon`, which must
be configured for regtest, hold the scoped macaroon, enforce fee caps, daily
fee budget, and cooldowns, require operator-token approval, and write audit
entries.

## Teardown

```bash
docker compose -f skills/lnd/templates/docker-compose-regtest.yml down -v   # -v wipes volumes
```

## Two-node variant (for channel/payment testing)

A second `litd` node (`bob`) is available behind a compose profile when you need a
channel between two parties (e.g. routing / L402 tests):

```bash
docker compose -f skills/lnd/templates/docker-compose-regtest.yml --profile two-node up -d
```

---

### See also
- `skills/run-litd/SKILL.md` — full litd setup (Neutrino, bitcoind, remote signer)
- `skills/lightning-mcp-server/SKILL.md` — MCP server build & configuration
- `skills/lnc-app/SKILL.md` — Lightning Node Connect pairing details
- `docs/mcp-server.md` — MCP transport / tool reference
