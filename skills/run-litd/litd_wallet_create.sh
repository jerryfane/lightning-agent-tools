#!/bin/bash

# litd Wallet Creation Script
# Tested on Ubuntu 24.04
#
# Run this after neutrino_setup_binary.sh or litd_setup_binary.sh,
# and before neutrino_setup3.sh or litd_setup3.sh.
#
# Starts litd temporarily, creates the LND wallet via REST API,
# then stops litd. Equivalent to the automated wallet creation in
# the remote signer scripts — no manual lncli create step needed.

set -e

# Variables
USER_HOME=$(eval echo ~${SUDO_USER:-$USER})
LIT_DIR="$USER_HOME/.lit"
LIT_CONF_FILE="$LIT_DIR/lit.conf"
LND_DIR="$USER_HOME/.lnd"
WALLET_PASSWORD_FILE="$LND_DIR/wallet_password"
SEED_FILE="$LND_DIR/seed_phrase.txt"
TLS_CERT="$LIT_DIR/tls.cert"
TIMEOUT=180
LITD_PID=""

# Check root
if [[ $EUID -ne 0 ]]; then
    echo "[-] This script must be run as root. Use sudo."
    exit 1
fi

# Install dependencies
apt-get install -y curl jq > /dev/null 2>&1

# Check prerequisites
if [[ ! -f "$WALLET_PASSWORD_FILE" || ! -s "$WALLET_PASSWORD_FILE" ]]; then
    echo "[-] Wallet password file not found: $WALLET_PASSWORD_FILE"
    echo "    Run the setup script first."
    exit 1
fi

if [[ ! -f "$LIT_CONF_FILE" || ! -s "$LIT_CONF_FILE" ]]; then
    echo "[-] lit.conf not found: $LIT_CONF_FILE"
    echo "    Run the setup script first."
    exit 1
fi

# Check if litd is already running
if pgrep -x litd > /dev/null; then
    echo "[-] litd is already running. Stop it before running this script."
    exit 1
fi

# Detect network from lit.conf
if grep -q "lnd.bitcoin.signet=1" "$LIT_CONF_FILE" 2>/dev/null; then
    NETWORK="signet"
else
    NETWORK="mainnet"
fi

MACAROON_PATH="$LND_DIR/data/chain/bitcoin/$NETWORK/admin.macaroon"

# Check if wallet already exists
if [[ -f "$MACAROON_PATH" ]]; then
    echo "[!] Wallet already initialized. Skipping."
    exit 0
fi

# Stop litd on exit (success or failure)
cleanup() {
    if [[ -n "$LITD_PID" ]] && kill -0 "$LITD_PID" 2>/dev/null; then
        echo "[+] Stopping litd..."
        kill "$LITD_PID"
        wait "$LITD_PID" 2>/dev/null || true
        echo "[+] litd stopped."
    fi
}
trap cleanup EXIT

# Start litd in background as the regular user
echo "[+] Starting litd for wallet creation..."
LITD_PATH=$(which litd)
sudo -u ${SUDO_USER:-$USER} "$LITD_PATH" &
LITD_PID=$!

# Wait for TLS cert
echo "[+] Waiting for litd TLS certificate..."
ELAPSED=0
while [[ ! -f "$TLS_CERT" ]]; do
    sleep 2; ELAPSED=$((ELAPSED + 2))
    if [[ $ELAPSED -ge $TIMEOUT ]]; then echo "[-] Timed out waiting for TLS cert."; exit 1; fi
done
echo "[+] TLS certificate ready."

# Wait for REST API
echo "[+] Waiting for litd REST API..."
ELAPSED=0
while ! curl -sf --cacert "$TLS_CERT" https://localhost:8443/v1/state > /dev/null 2>&1; do
    sleep 2; ELAPSED=$((ELAPSED + 2))
    if [[ $ELAPSED -ge $TIMEOUT ]]; then echo "[-] Timed out waiting for REST API."; exit 1; fi
done
echo "[+] litd REST API ready."

# Generate seed
echo "[+] Generating wallet seed..."
SEED_JSON=$(curl -sf --cacert "$TLS_CERT" https://localhost:8443/v1/genseed)
if [[ -z "$SEED_JSON" ]]; then echo "[-] Failed to generate seed."; exit 1; fi

MNEMONIC=$(echo "$SEED_JSON" | jq -r '.cipher_seed_mnemonic | join(" ")')
MNEMONIC_ARRAY=$(echo "$SEED_JSON" | jq -c '.cipher_seed_mnemonic')

echo "$MNEMONIC" > "$SEED_FILE"
chown ${SUDO_USER:-$USER}:${SUDO_USER:-$USER} "$SEED_FILE"
chmod 600 "$SEED_FILE"

echo ""
echo "============================================================"
echo "  !! WALLET SEED PHRASE - BACK THIS UP IMMEDIATELY !!      "
echo "  This is your ONLY recovery option if this node is lost.  "
echo "============================================================"
echo "$MNEMONIC"
echo "============================================================"
echo "  Saved to: $SEED_FILE  (chmod 600)"
echo "  Once backed up, delete it:  shred -u $SEED_FILE"
echo "============================================================"
echo ""

# Initialize wallet
echo "[+] Initializing wallet..."
WALLET_PASSWORD_B64=$(tr -d '\n' < "$WALLET_PASSWORD_FILE" | base64 -w 0)
INIT_RESPONSE=$(curl -sf --cacert "$TLS_CERT" \
    -X POST https://localhost:8443/v1/initwallet \
    -H 'Content-Type: application/json' \
    -d "{\"wallet_password\": \"$WALLET_PASSWORD_B64\", \"cipher_seed_mnemonic\": $MNEMONIC_ARRAY}")

if [[ -z "$INIT_RESPONSE" ]]; then echo "[-] No response from initwallet."; exit 1; fi
if echo "$INIT_RESPONSE" | jq -e '.code' > /dev/null 2>&1; then
    echo "[-] Wallet init failed: $(echo "$INIT_RESPONSE" | jq -r '.message')"
    exit 1
fi
echo "[+] Wallet initialized, waiting for unlock..."

# Wait for macaroon
ELAPSED=0
while [[ ! -f "$MACAROON_PATH" ]]; do
    sleep 2; ELAPSED=$((ELAPSED + 2))
    if [[ $ELAPSED -ge $TIMEOUT ]]; then echo "[-] Timed out waiting for wallet unlock."; exit 1; fi
done
echo "[+] Wallet unlocked."

cat <<"EOF"

[+] Wallet creation complete!

IMPORTANT: Back up your seed phrase then delete it from disk:
  shred -u ~/.lnd/seed_phrase.txt

Next step: run the setup3 script to enable auto-unlock and the systemd service.

EOF
