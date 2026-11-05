// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;
import "forge-std/Test.sol";
import "../src/PriceWire.sol";

contract MockPool {
    // minimal V3 pool whose observations follow a constant tick, with controllable spot/liquidity/cardinality
    int24 public spot; uint128 public liq; uint16 public card; uint32 public oldest; int24 public avgTick; bool public unlocked = true; uint16 public idx;
    address public token0;
    constructor(address t0) { token0 = t0; }
    function set(int24 _spot, int24 _avg, uint128 _liq, uint16 _card, uint32 _oldest) external { spot = _spot; avgTick = _avg; liq = _liq; card = _card; oldest = _oldest; }
    function setLocked(bool l) external { unlocked = !l; }
    function slot0() external view returns (uint160, int24, uint16, uint16, uint16, uint8, bool) { return (0, spot, idx, card, card, 0, unlocked); }
    function liquidity() external view returns (uint128) { return liq; }
    function observations(uint256) external view returns (uint32, int56, uint160, bool) { return (oldest, 0, 0, oldest != 0); }
    function observe(uint32[] calldata ago) external view returns (int56[] memory c, uint160[] memory s) {
        c = new int56[](ago.length); s = new uint160[](ago.length);
        // cumulative(t) = avgTick * (t - start); only differences matter
        for (uint256 i; i < ago.length; i++) c[i] = int56(avgTick) * (int56(uint56(1_000_000)) - int56(uint56(ago[i])));
    }
}
contract MockFactory { mapping(bytes32 => address) public pools;
    function set(address a, address b, uint24 f, address p) external { (a, b) = a < b ? (a, b) : (b, a); pools[keccak256(abi.encode(a, b, f))] = p; }
    function getPool(address a, address b, uint24 f) external view returns (address) { (a, b) = a < b ? (a, b) : (b, a); return pools[keccak256(abi.encode(a, b, f))]; } }

contract PriceWireTest is Test {
    PriceWire w; MockFactory f; MockPool p3000; MockPool p500;
    address A = address(0x1000); address B = address(0x2000); // A < B so A = token0
    function setUp() public {
        vm.warp(1_000_000);
        f = new MockFactory(); w = new PriceWire(address(f));
        p3000 = new MockPool(A); p500 = new MockPool(A);
        f.set(A, B, 3000, address(p3000)); f.set(A, B, 500, address(p500));
        p3000.set(10_000, 10_000, 5e18, 100, 100_000);
    }
    // ---- math
    function test_sqrtRatioKnownPoints() public view {
        assertEq(w.sqrtRatioX128(0), 1 << 128);
        // Uniswap's MAX_SQRT_RATIO (Q96) shifted to Q128; allow 1e-12 relative error
        uint256 max96 = 1461446703485210103287273052203988822378723970342;
        uint256 got = w.sqrtRatioX128(887272);
        assertApproxEqRel(got, max96 << 32, 1e6); // 1e-12 in 1e18 scale
        uint256 min96 = 4295128739;
        assertApproxEqRel(w.sqrtRatioX128(-887272), min96 << 32, 1e9); // tiny numbers: 1e-9
    }
    function testFuzz_tickStepIsSqrt10001(int24 t) public view {
        t = int24(bound(int256(t), -800_000, 800_000));
        uint256 a = w.sqrtRatioX128(t); uint256 b = w.sqrtRatioX128(t + 1);
        // b/a = sqrt(1.0001) = 1.000049998750062496...
        uint256 ratio = Math.mulDiv(b, 1e18, a);
        assertApproxEqAbs(ratio, 1.00004999875006249e18, 1e6);
    }
    function test_quoteDirectionAndPrice() public view {
        // tick 0 -> 1:1 both ways
        assertEq(w.quoteAtTick(0, 1e18, A, B), 1e18); assertEq(w.quoteAtTick(0, 1e18, B, A), 1e18);
        // tick 6932 ~ price 2.0 (1.0001^6931.9 = 2): token1 per token0
        uint256 out = w.quoteAtTick(6932, 1e18, A, B);
        assertApproxEqRel(out, 2e18, 5e14);
        assertApproxEqRel(w.quoteAtTick(6932, 2e18, B, A), 1e18, 5e14);
        // negative ticks mirror
        assertApproxEqRel(w.quoteAtTick(-6932, 1e18, A, B), 0.5e18, 5e14);
        // huge price branch (sqrt > 2^128)
        assertApproxEqRel(w.quoteAtTick(276_324, 1, A, B), 1e12, 1e15); // 1.0001^276324 ~ 1e12
    }
    // ---- health / consult with mocks
    function test_consultHealthy() public view {
        (uint256 out, PriceWire.Health memory h) = w.consult(A, B, 3000, 1e18, 600, 1e6);
        assertEq(h.flags, 0); assertEq(h.twapTick, 10_000); assertEq(h.historySeconds, 900_000);
        assertApproxEqRel(out, 2.7181e18, 1e15); // 1.0001^10000 = e^0.99995 ~ 2.7181
    }
    function test_twapRoundsDownForNegative() public {
        p3000.set(-11, -11, 5e18, 100, 100_000); // average exactly -11
        PriceWire.Health memory h = w.health(address(p3000), 600, 1);
        assertEq(h.twapTick, -11);
    }
    function test_flagsNoPoolLowLiquidityShortHistoryDeviationLocked() public {
        assertEq(w.health(address(0), 600, 1).flags, w.NO_POOL());
        p3000.set(10_000, 10_000, 10, 100, 100_000);
        assertEq(w.health(address(p3000), 600, 1e6).flags, w.LOW_LIQUIDITY());
        p3000.set(10_000, 10_000, 5e18, 100, uint32(block.timestamp) - 30);
        assertEq(w.health(address(p3000), 600, 1e6).flags, w.SHORT_HISTORY());
        p3000.set(10_000, 10_000, 5e18, 1, 100_000); // cardinality 1 => no history
        assertEq(w.health(address(p3000), 600, 1e6).flags, w.SHORT_HISTORY());
        p3000.set(10_500, 10_000, 5e18, 100, 100_000); // spot 500 ticks away
        PriceWire.Health memory h = w.health(address(p3000), 600, 1e6);
        assertEq(h.flags, w.SPOT_DEVIATION()); assertEq(h.deviationTicks, 500);
        p3000.set(10_000, 10_000, 5e18, 100, 100_000); p3000.setLocked(true);
        assertEq(w.health(address(p3000), 600, 1e6).flags, w.LOCKED());
    }
    function test_consultRevertsWhenUnhealthy() public {
        p3000.set(10_500, 10_000, 5e18, 100, 100_000);
        vm.expectRevert(abi.encodeWithSelector(PriceWire.Unhealthy.selector, uint8(8)));
        w.consult(A, B, 3000, 1e18, 600, 1e6);
        vm.expectRevert(PriceWire.BadWindow.selector); w.consult(A, B, 3000, 1e18, 30, 1e6);
        vm.expectRevert(PriceWire.SameToken.selector); w.consult(A, A, 3000, 1e18, 600, 1e6);
        vm.expectRevert(abi.encodeWithSelector(PriceWire.Unhealthy.selector, uint8(1)));
        w.consult(A, B, 10000, 1e18, 600, 1e6);
    }
    function test_consultBestPicksDeepestHealthyTier() public {
        p500.set(9_000, 9_000, 9e18, 100, 100_000); // deeper but different price
        (, PriceWire.Health memory h) = w.consultBest(A, B, 1e18, 600, 1e6);
        assertEq(h.pool, address(p500));
        p500.set(9_000, 9_000, 9e18, 1, 100_000); // unhealthy -> falls back to 3000
        (, h) = w.consultBest(A, B, 1e18, 600, 1e6);
        assertEq(h.pool, address(p3000));
        p3000.set(10_000, 10_000, 1, 100, 100_000);
        vm.expectRevert(PriceWire.NoHealthyPool.selector); w.consultBest(A, B, 1e18, 600, 1e6);
    }
    function test_healthAllShape() public view {
        PriceWire.Health[4] memory hs = w.healthAll(A, B, 600, 1e6);
        assertEq(hs[0].flags, 1); assertEq(hs[2].flags, 0); assertEq(hs[3].flags, 1);
    }
}
