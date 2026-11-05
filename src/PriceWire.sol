// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;
import {Math} from "@openzeppelin/contracts/utils/math/Math.sol";

interface IV3Factory { function getPool(address, address, uint24) external view returns (address); }
interface IV3Pool {
    function slot0() external view returns (uint160 sqrtPriceX96, int24 tick, uint16 observationIndex, uint16 observationCardinality, uint16 observationCardinalityNext, uint8 feeProtocol, bool unlocked);
    function liquidity() external view returns (uint128);
    function observe(uint32[] calldata secondsAgos) external view returns (int56[] memory tickCumulatives, uint160[] memory);
    function observations(uint256 i) external view returns (uint32 blockTimestamp, int56 tickCumulative, uint160 secondsPerLiquidityCumulativeX128, bool initialized);
    function token0() external view returns (address);
}

/// @title PriceWire - stateless TWAP oracle adapter for BDEX V3 (Uniswap V3 style) pools on BOT Chain.
/// No owner, no storage, no tokens held. Every answer comes with a health verdict so callers can refuse stale or thin prices.
contract PriceWire {
    // health flags
    uint8 public constant NO_POOL = 1;          // pool does not exist
    uint8 public constant LOW_LIQUIDITY = 2;    // in-range liquidity below the minimum
    uint8 public constant SHORT_HISTORY = 4;    // oracle history shorter than the requested window
    uint8 public constant SPOT_DEVIATION = 8;   // spot tick differs from the TWAP tick by more than maxDeviationTicks
    uint8 public constant LOCKED = 16;          // pool reentrancy lock set (mid-swap read)

    IV3Factory public immutable factory;
    uint32 public constant MIN_WINDOW = 60;
    uint128 public constant MIN_LIQUIDITY = 1e6;     // raw V3 liquidity units; callers may pass their own
    uint24 public constant MAX_DEVIATION_TICKS = 200; // about 2%
    uint24[4] public FEES = [100, 500, 3000, 10000];

    struct Health {
        address pool; uint8 flags; uint128 liquidity; uint16 cardinality; uint16 cardinalityNext;
        uint32 historySeconds; int24 spotTick; int24 twapTick; uint24 deviationTicks;
    }

    error BadWindow(); error Unhealthy(uint8 flags); error NoHealthyPool(); error SameToken();

    constructor(address v3Factory) { factory = IV3Factory(v3Factory); }

    // ------------------------------------------------------------------ health
    function health(address pool, uint32 window, uint128 minLiquidity) public view returns (Health memory h) {
        h.pool = pool;
        if (pool == address(0)) { h.flags = NO_POOL; return h; }
        (, int24 spot, uint16 idx, uint16 card, uint16 cardNext,, bool unlocked) = IV3Pool(pool).slot0();
        h.spotTick = spot; h.cardinality = card; h.cardinalityNext = cardNext;
        h.liquidity = IV3Pool(pool).liquidity();
        if (!unlocked) h.flags |= LOCKED;
        if (h.liquidity < minLiquidity) h.flags |= LOW_LIQUIDITY;
        h.historySeconds = _history(pool, idx, card);
        if (h.historySeconds < window) { h.flags |= SHORT_HISTORY; h.twapTick = spot; return h; }
        h.twapTick = _twapTick(pool, window);
        int256 d = int256(spot) - int256(h.twapTick);
        h.deviationTicks = uint24(uint256(d < 0 ? -d : d));
        if (h.deviationTicks > MAX_DEVIATION_TICKS) h.flags |= SPOT_DEVIATION;
    }

    /// Seconds of usable oracle history: age of the oldest initialised observation.
    function _history(address pool, uint16 idx, uint16 card) internal view returns (uint32) {
        if (card <= 1) return 0;
        (uint32 tsOldest,,, bool init) = IV3Pool(pool).observations((uint256(idx) + 1) % card);
        if (!init) (tsOldest,,,) = IV3Pool(pool).observations(0);
        return uint32(block.timestamp) - tsOldest; // uint32 wraparound is intended
    }

    function _twapTick(address pool, uint32 window) internal view returns (int24) {
        uint32[] memory ago = new uint32[](2); ago[0] = window; ago[1] = 0;
        (int56[] memory c,) = IV3Pool(pool).observe(ago);
        int56 delta = c[1] - c[0];
        int24 t = int24(delta / int56(uint56(window)));
        if (delta < 0 && (delta % int56(uint56(window)) != 0)) t--; // round toward negative infinity
        return t;
    }

    // ------------------------------------------------------------------ quotes
    /// Time-weighted price of `amountIn` of tokenIn in tokenOut over `window` seconds, from one fee tier. Reverts if unhealthy.
    function consult(address tokenIn, address tokenOut, uint24 fee, uint128 amountIn, uint32 window, uint128 minLiquidity)
        public view returns (uint256 amountOut, Health memory h)
    {
        if (window < MIN_WINDOW) revert BadWindow();
        if (tokenIn == tokenOut) revert SameToken();
        h = health(factory.getPool(tokenIn, tokenOut, fee), window, minLiquidity);
        if (h.flags != 0) revert Unhealthy(h.flags);
        amountOut = quoteAtTick(h.twapTick, amountIn, tokenIn, tokenOut);
    }

    /// Best healthy fee tier, by liquidity. Reverts NoHealthyPool when none passes.
    function consultBest(address tokenIn, address tokenOut, uint128 amountIn, uint32 window, uint128 minLiquidity)
        external view returns (uint256 amountOut, Health memory best)
    {
        if (window < MIN_WINDOW) revert BadWindow();
        if (tokenIn == tokenOut) revert SameToken();
        bool found;
        for (uint256 i; i < 4; i++) {
            Health memory h = health(factory.getPool(tokenIn, tokenOut, FEES[i]), window, minLiquidity);
            if (h.flags == 0 && (!found || h.liquidity > best.liquidity)) { best = h; found = true; }
        }
        if (!found) revert NoHealthyPool();
        amountOut = quoteAtTick(best.twapTick, amountIn, tokenIn, tokenOut);
    }

    /// Health of every fee tier of a pair - for dashboards and the Go service.
    function healthAll(address tokenA, address tokenB, uint32 window, uint128 minLiquidity) external view returns (Health[4] memory out) {
        for (uint256 i; i < 4; i++) out[i] = health(factory.getPool(tokenA, tokenB, FEES[i]), window, minLiquidity);
    }

    // ------------------------------------------------------------------ math
    /// Amount of tokenOut for amountIn at a tick (price of token1 in token0 = 1.0001^tick).
    function quoteAtTick(int24 tick, uint128 amountIn, address tokenIn, address tokenOut) public pure returns (uint256) {
        uint256 sqrtX128 = sqrtRatioX128(tick); // sqrt(token1/token0) in Q128
        if (sqrtX128 <= type(uint128).max) {
            uint256 ratioX128 = Math.mulDiv(sqrtX128, sqrtX128, 1 << 128);
            return tokenIn < tokenOut ? Math.mulDiv(ratioX128, amountIn, 1 << 128) : Math.mulDiv(1 << 128, amountIn, ratioX128);
        }
        uint256 ratioX64 = Math.mulDiv(sqrtX128, sqrtX128, 1 << 192);
        return tokenIn < tokenOut ? Math.mulDiv(ratioX64, amountIn, 1 << 64) : Math.mulDiv(1 << 64, amountIn, ratioX64);
    }

    /// 1.0001^(tick/2) as Q128.128. Independent derivation: product of precomputed 1.0001^(-2^i/2) factors.
    function sqrtRatioX128(int24 tick) public pure returns (uint256) {
        uint256 t = tick < 0 ? uint256(uint24(-tick)) : uint256(uint24(tick));
        require(t <= 887272, "TICK");
        uint256 r = 1 << 128;
        if (t & 0x1 != 0) r = r * 0xfffcb933bd6fad37aa2d162d1a594001 >> 128;
        if (t & 0x2 != 0) r = r * 0xfff97272373d413259a46990580e2139 >> 128;
        if (t & 0x4 != 0) r = r * 0xfff2e50f5f656932ef12357cf3c7fdcb >> 128;
        if (t & 0x8 != 0) r = r * 0xffe5caca7e10e4e61c3624eaa0941ccf >> 128;
        if (t & 0x10 != 0) r = r * 0xffcb9843d60f6159c9db58835c926643 >> 128;
        if (t & 0x20 != 0) r = r * 0xff973b41fa98c081472e6896dfb254bf >> 128;
        if (t & 0x40 != 0) r = r * 0xff2ea16466c96a3843ec78b326b52860 >> 128;
        if (t & 0x80 != 0) r = r * 0xfe5dee046a99a2a811c461f1969c3052 >> 128;
        if (t & 0x100 != 0) r = r * 0xfcbe86c7900a88aedcffc83b479aa3a3 >> 128;
        if (t & 0x200 != 0) r = r * 0xf987a7253ac413176f2b074cf7815e53 >> 128;
        if (t & 0x400 != 0) r = r * 0xf3392b0822b70005940c7a398e4b70f2 >> 128;
        if (t & 0x800 != 0) r = r * 0xe7159475a2c29b7443b29c7fa6e889d8 >> 128;
        if (t & 0x1000 != 0) r = r * 0xd097f3bdfd2022b8845ad8f792aa5825 >> 128;
        if (t & 0x2000 != 0) r = r * 0xa9f746462d870fdf8a65dc1f90e061e4 >> 128;
        if (t & 0x4000 != 0) r = r * 0x70d869a156d2a1b890bb3df62baf32f6 >> 128;
        if (t & 0x8000 != 0) r = r * 0x31be135f97d08fd981231505542fcfa5 >> 128;
        if (t & 0x10000 != 0) r = r * 0x9aa508b5b7a84e1c677de54f3e99bc8 >> 128;
        if (t & 0x20000 != 0) r = r * 0x5d6af8dedb81196699c329225ee604 >> 128;
        if (t & 0x40000 != 0) r = r * 0x2216e584f5fa1ea926041bedfe97 >> 128;
        if (t & 0x80000 != 0) r = r * 0x48a170391f7dc42444e8fa2 >> 128;
        return tick > 0 ? type(uint256).max / r : r;
    }
}
