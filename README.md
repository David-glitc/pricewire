# PriceWire

TWAP price oracle adapter for BDEX V3 on BOT Chain, plus a Go service that reads it, reports pool health and serves a small JSON API.
Stack: Foundry (Solidity 0.8.24, paris) + Go 1.23 (stdlib only).

## The gap
BOT Chain has no Chainlink-style feeds. Anything that needs a price (lending, vaults, prediction markets, payouts in USDT) has to read a DEX pool,
and a raw spot price is trivially manipulable. BDEX V3 pools keep an on-chain observation history, but nobody has wrapped it with the checks
that make it safe to use. A live survey of WBOT/USDT shows why those checks matter:

| Tier | State on mainnet (5 Nov 2026) |
|---|---|
| 0.01% | no pool |
| 0.05% | exists, liquidity 3.2e9, price **27% above** the 0.3% pool |
| 0.3% | deep (2e19), 1024-slot history, 1 WBOT = 12.47 USDT |
| 1% | no liquidity, flagged |

A naive "read the first pool" integration would have quoted the wrong price. PriceWire picks the deepest *healthy* tier and refuses to answer otherwise.

## Contract (`src/PriceWire.sol`) - stateless, no owner, no tokens, no admin
- `consult(tokenIn, tokenOut, fee, amountIn, window, minLiquidity)` - TWAP quote from one tier, reverts `Unhealthy(flags)`.
- `consultBest(tokenIn, tokenOut, amountIn, window, minLiquidity)` - deepest healthy tier of the four, reverts `NoHealthyPool`.
- `health(pool, window, minLiquidity)` / `healthAll(a, b, window, minLiquidity)` - never revert; return flags and numbers.
- Flags: `NO_POOL`, `LOW_LIQUIDITY`, `SHORT_HISTORY` (observation history shorter than the window), `SPOT_DEVIATION` (spot tick more than 200 ticks, about 2%, from the TWAP tick), `LOCKED` (mid-swap read).
- Own tick math (`sqrtRatioX128`, Q128, product of precomputed 1.0001^(-2^i/2) factors; checked against Uniswap's MAX/MIN sqrt ratio and the live pool's `sqrtPriceX96`). Window is at least 60 seconds.

## Go service (`service/`)
Reads PriceWire with plain `eth_call` (no keys, no state changes). Endpoints, all JSON with CORS:
- `GET /healthz` - RPC reachable, block, oracle address
- `GET /v1/pairs`
- `GET /v1/price?pair=WBOT-USDT&window=1800` - price, pool used, history, liquidity. `503` with per-tier reasons when no tier is healthy; `502` when the RPC itself fails (an outage is never reported as "no healthy pool").
- `GET /v1/health?pair=WBOT-USDT` - all four tiers with flags, spot/TWAP prices, status `ok|degraded|down` (healthy tiers 2+/1/0), and a warning when tiers disagree by more than 5%.
5-second cache. Env: `RPC_URL`, `PRICEWIRE`, `LISTEN`.

## Risks and how they are handled
| Risk | Handling |
|---|---|
| Flash-loan / single-block price push | TWAP over at least 60 s (use 30 min+ for money) and a spot-vs-TWAP deviation flag. |
| Manipulating a thin pool over the window | `minLiquidity` is the caller's choice; use the cost of moving the pool, not just a raw number. A 30 min TWAP on a 2e19-liquidity pool costs real capital to move; the 0.05% pool above would not. `consultBest` prefers the deepest tier. |
| Stale or young pool | `SHORT_HISTORY` when the oldest observation is younger than the window (also catches cardinality 1). |
| Mid-swap read | `LOCKED` flag. |
| Tier disagreement | Reported by the service as a warning; the contract uses the deepest healthy tier only. Residual: the oracle does not cross-check tiers on-chain (gas). |
| Token ordering and decimals | Quote math handles both directions; the service applies decimals. Tested both ways. |
| Upgrade / admin risk | None: immutable factory address, no state. |

## Run it
```bash
forge test                                                              # 9 unit/fuzz tests
forge test --match-contract Fork --fork-url https://rpc.botchain.ai -vv  # 4 tests against live BDEX pools (read-only)
(cd service && go test ./...)                                           # 10 tests with a fake JSON-RPC node
bash script/demo.sh   # anvil fork of BOT Chain + deploy PriceWire locally + start the service + curl it (needs ~$HOME/.local/gotc/go or go in PATH)
```
(`lib/` is not committed: `forge install foundry-rs/forge-std OpenZeppelin/openzeppelin-contracts --no-git`.)
