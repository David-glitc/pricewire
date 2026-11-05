# PriceWire launch (needs the owner's approval - nothing here has run on mainnet)

Estimated gas (Foundry; ~22 gwei assumed from wave 3's "5M gas ~ 0.11 BOT"):
| Action | Gas | ~BOT |
|---|---|---|
| Deploy PriceWire | ~1.1M | 0.025 |
| `consultBest` / `consult` / `healthAll` | view calls - free off-chain; ~150k when called inside a transaction | 0.003 |

Steps
1. Fresh deployer with ~0.05 BOT (not a wave 1 key).
2. `forge create src/PriceWire.sol:PriceWire --rpc-url https://rpc.botchain.ai --private-key $KEY --broadcast --constructor-args 0x1C51c173323ec11BB4e3C4fD2314c225Dc4b5419`
3. Verify on BOTScan; call `healthAll(WBOT, USDT, 1800, 1000000)` and check it matches the Go service on the fork.
4. Service: `cd service && go build -o pricewire . && PRICEWIRE=<addr> LISTEN=0.0.0.0:8080 ./pricewire`; run behind a reverse proxy (host choice and any paid server needs approval; it is read-only and fits a free tier).
5. Publish the address and the flag semantics so ecosystem projects (predictbox, lending, vaults) can integrate.

Open decisions: public hosting for the API; default `minLiquidity` per pair; adding more pairs (CA, FLOW, Money) once their pools have history.
