#!/usr/bin/env bash
# End-to-end rehearsal on an Anvil fork of BOT Chain mainnet: deploy PriceWire (to the local fork only), run the Go service, query it.
# Uses anvil's public dev key. Nothing is sent to the real chain.
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="$HOME/.local/gotc/go/bin:$PATH"
KEY=0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80
FACTORY=0x1C51c173323ec11BB4e3C4fD2314c225Dc4b5419
anvil --fork-url "${BOT_RPC:-https://rpc.botchain.ai}" --port 8546 >/dev/null 2>&1 & APID=$!
trap 'kill $APID ${SPID:-} 2>/dev/null || true' EXIT
for i in $(seq 1 30); do cast chain-id --rpc-url http://127.0.0.1:8546 >/dev/null 2>&1 && break; sleep 1; done
ADDR=$(forge create src/PriceWire.sol:PriceWire --rpc-url http://127.0.0.1:8546 --private-key $KEY --broadcast --constructor-args $FACTORY 2>&1 | awk '/Deployed to:/{print $3}')
echo "PriceWire on fork: $ADDR"
(cd service && go build -o pricewire . && RPC_URL=http://127.0.0.1:8546 PRICEWIRE=$ADDR LISTEN=127.0.0.1:8088 ./pricewire >/dev/null 2>&1) & SPID=$!
for i in $(seq 1 40); do curl -sf http://127.0.0.1:8088/healthz >/dev/null 2>&1 && break; sleep 1; done
for p in healthz "v1/pairs" "v1/price?pair=WBOT-USDT&window=1800" "v1/health?pair=WBOT-USDT&window=1800" "v1/price?pair=WBOT-USDT&window=99999999"; do
  echo "--- GET /$p"; curl -s -w ' [%{http_code}]\n' "http://127.0.0.1:8088/$p" | cut -c1-700
done
