// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;
import "forge-std/Test.sol";
import "../src/PriceWire.sol";
// forge test --match-contract Fork --fork-url https://rpc.botchain.ai -vv   (read-only; skipped without a chain-677 fork)
contract Fork is Test {
    address constant FACTORY = 0x1C51c173323ec11BB4e3C4fD2314c225Dc4b5419;
    address constant WBOT = 0xD5452816194a3784dBa983426cCe7c122F4abd30;
    address constant USDT = 0xaBabc7Ddc03e501d190C676BF3d92ef0e6e87a3C;
    PriceWire w;
    function setUp() public { if (block.chainid != 677) vm.skip(true); w = new PriceWire(FACTORY); }

    function test_mathMatchesPoolSqrtPrice() public view {
        address pool = IV3Factory(FACTORY).getPool(WBOT, USDT, 3000);
        (uint160 sqrtP, int24 tick,,,,,) = IV3Pool(pool).slot0();
        // pool's price is somewhere inside the current tick: ours is the tick's lower edge, within one tick (0.005% sqrt)
        uint256 ours = w.sqrtRatioX128(tick); uint256 pools = uint256(sqrtP) << 32;
        assertApproxEqRel(ours, pools, 1e14, "sqrt within one tick");
    }
    function test_healthOfLivePools() public {
        PriceWire.Health[4] memory hs = w.healthAll(WBOT, USDT, 1800, 1e6);
        string[4] memory names = ["0.01%", "0.05%", "0.3%", "1%"];
        for (uint256 i; i < 4; i++) {
            emit log_named_string("tier", names[i]);
            emit log_named_uint("  flags", hs[i].flags); emit log_named_uint("  liquidity", hs[i].liquidity);
            emit log_named_uint("  history s", hs[i].historySeconds); emit log_named_int("  spot", hs[i].spotTick); emit log_named_int("  twap", hs[i].twapTick);
        }
        assertEq(hs[0].flags, w.NO_POOL(), "no 0.01% pool");
        assertEq(hs[2].flags & (w.NO_POOL() | w.SHORT_HISTORY()), 0, "0.3% pool has history");
    }
    function test_consultWbotUsdtMatchesSpot() public {
        // price of 1 WBOT in USDT (6 decimals) over 30 min from the deepest healthy tier
        (uint256 out, PriceWire.Health memory h) = w.consultBest(WBOT, USDT, 1e18, 1800, 1e6);
        emit log_named_address("pool", h.pool); emit log_named_uint("1 WBOT in USDT (6dp)", out);
        uint256 spot = w.quoteAtTick(h.spotTick, 1e18, WBOT, USDT);
        assertGt(out, 0);
        assertApproxEqRel(out, spot, 0.03e18, "twap near spot on a healthy pool");
        // reverse direction roundtrips (within rounding of integer amounts)
        uint256 back = w.quoteAtTick(h.twapTick, uint128(out), USDT, WBOT);
        assertApproxEqRel(back, 1e18, 1e13);
    }
    function test_thinTierIsRejectedAt30Minutes() public view {
        address pool = IV3Factory(FACTORY).getPool(WBOT, USDT, 500);
        PriceWire.Health memory h = w.health(pool, 1800, 1e6);
        // 0.05% pool had observation cardinality 1 when surveyed: must be flagged, never quoted
        if (h.cardinality <= 1) assertTrue(h.flags & w.SHORT_HISTORY() != 0);
    }
}
