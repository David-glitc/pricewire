package main

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func w32(v *big.Int) []byte { // two's complement for negatives
	if v.Sign() < 0 {
		v = new(big.Int).Add(v, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	return v.FillBytes(make([]byte, 32))
}

func healthWords(pool string, flags, liq, card, hist int64, spot, twap int64, dev int64) []byte {
	var b []byte
	pw, _ := addrWord(pool)
	b = append(b, pw...)
	for _, v := range []int64{flags, liq, card, card, hist, spot, twap, dev} {
		b = append(b, w32(big.NewInt(v))...)
	}
	return b
}

const poolA = "0x64F418471a1A7932a190E10da5A8551dB5AbeC05"
const zero = "0x0000000000000000000000000000000000000000"

// fakeNode answers eth_call by selector.
func fakeNode(t *testing.T, tiers [4][]byte, consultFail bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		json.Unmarshal(body, &req)
		reply := func(result string, errMsg string) {
			if errMsg != "" {
				w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":3,"message":"` + errMsg + `"}}`))
				return
			}
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + result + `"}`))
		}
		if req.Method == "eth_blockNumber" {
			reply("0x1d0", "")
			return
		}
		data := req.Params[0].(map[string]any)["data"].(string)
		switch {
		case strings.HasPrefix(data, "0x"+selHealthAll):
			var out []byte
			for _, tr := range tiers {
				out = append(out, tr...)
			}
			reply("0x"+hex.EncodeToString(out), "")
		case strings.HasPrefix(data, "0x"+selConsultBest):
			if consultFail {
				reply("", "execution reverted")
				return
			}
			out := w32(big.NewInt(12_469_385)) // 12.469385 USDT
			out = append(out, tiers[2]...)
			reply("0x"+hex.EncodeToString(out), "")
		}
	}))
}

func testServer(t *testing.T, tiers [4][]byte, fail bool) (*Server, *httptest.Server) {
	node := fakeNode(t, tiers, fail)
	cfg := Config{Wire: "0xWIRE", DefaultWin: 1800, MinLiq: big.NewInt(1e6), DivergeWarn: 0.05,
		Tokens: map[string]Token{"WBOT": {"WBOT", "0xD5452816194a3784dBa983426cCe7c122F4abd30", 18}, "USDT": {"USDT", "0xaBabc7Ddc03e501d190C676BF3d92ef0e6e87a3C", 6}},
		Pairs:  [][2]string{{"WBOT", "USDT"}}}
	s := NewServer(cfg, NewRPC(node.URL))
	return s, node
}

func get(t *testing.T, s *Server, path string) (int, map[string]any) {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func goodTiers() [4][]byte {
	return [4][]byte{
		healthWords(zero, 1, 0, 0, 0, 0, 0, 0),
		healthWords("0x050a7C2EC050A1D1402053A40a2Eb0F6275ed70a", 0, 3_215_051_297, 100, 3_500_000, 253506, 253506, 0),
		healthWords(poolA, 0, 2_077_459_068_165_796_750, 1024, 370_000, 251089, 251090, 1),
		healthWords(zero, 1, 0, 0, 0, 0, 0, 0),
	}
}

func TestDecodeNegativeTicksAndFlags(t *testing.T) {
	h := decodeHealth(healthWords(poolA, 6, 5, 1, 0, -887, -889, 2), 3000)
	if h.SpotTick != -887 || h.TwapTick != -889 || len(h.FlagNames) != 2 || h.FlagNames[0] != "LOW_LIQUIDITY" || h.FlagNames[1] != "SHORT_HISTORY" || h.Healthy {
		t.Fatalf("bad decode: %+v", h)
	}
}

func TestPriceEndpoint(t *testing.T) {
	s, node := testServer(t, goodTiers(), false)
	defer node.Close()
	code, m := get(t, s, "/v1/price?pair=wbot-usdt&window=900")
	if code != 200 || m["price"] != "12.469385" || m["window_seconds"].(float64) != 900 || m["pool"] != strings.ToLower(poolA) {
		t.Fatalf("%d %v", code, m)
	}
}

func TestPriceDownReportsTiers(t *testing.T) {
	tiers := goodTiers()
	tiers[1] = healthWords(poolA, 4, 5, 1, 10, 1, 1, 0)
	tiers[2] = healthWords(poolA, 2, 0, 1, 10, 1, 1, 0)
	s, node := testServer(t, tiers, true)
	defer node.Close()
	code, m := get(t, s, "/v1/price?base=WBOT&quote=USDT")
	if code != 503 || m["error"] == nil || len(m["tiers"].([]any)) != 4 {
		t.Fatalf("%d %v", code, m)
	}
}

func TestHealthEndpointWarnsOnTierDivergence(t *testing.T) {
	s, node := testServer(t, goodTiers(), false)
	defer node.Close()
	code, m := get(t, s, "/v1/health?pair=WBOT-USDT")
	if code != 200 || m["status"] != "ok" || int(m["healthy_tiers"].(float64)) != 2 {
		t.Fatalf("%d %v", code, m)
	}
	w := m["warnings"].([]any)
	if len(w) != 1 || !strings.Contains(w[0].(string), "disagree") { // ticks 253506 vs 251089 = 27%
		t.Fatalf("expected divergence warning, got %v", w)
	}
}

func TestStatusLevels(t *testing.T) {
	if statusOf(0) != "down" || statusOf(1) != "degraded" || statusOf(2) != "ok" {
		t.Fatal("status")
	}
}

func TestInputValidation(t *testing.T) {
	s, node := testServer(t, goodTiers(), false)
	defer node.Close()
	for _, p := range []string{"/v1/price?pair=FOO-USDT", "/v1/price?pair=WBOT", "/v1/price?pair=WBOT-USDT&window=5", "/v1/health?window=abc&pair=WBOT-USDT"} {
		if code, _ := get(t, s, p); code != 400 {
			t.Fatalf("%s: got %d", p, code)
		}
	}
}

func TestTickPriceMatchesContractExample(t *testing.T) {
	wbot := Token{"WBOT", "0xD5452816194a3784dBa983426cCe7c122F4abd30", 18}
	usdt := Token{"USDT", "0xaBabc7Ddc03e501d190C676BF3d92ef0e6e87a3C", 6}
	// USDT (0xaBab..) > WBOT (0xD545..)? compare hex: 0xa < 0xd so USDT is token0; tick 251090 => token1/token0 = WBOT per USDT raw
	p := tickPrice(251090, wbot, usdt)
	if p < 11 || p > 14 {
		t.Fatalf("price of 1 WBOT should be ~12.4 USDT, got %v", p)
	}
}

func TestCacheAvoidsSecondRPC(t *testing.T) {
	s, node := testServer(t, goodTiers(), false)
	defer node.Close()
	get(t, s, "/v1/price?pair=WBOT-USDT")
	node.Close() // second call must come from cache
	if code, _ := get(t, s, "/v1/price?pair=WBOT-USDT"); code != 200 {
		t.Fatalf("cache miss, %d", code)
	}
}

func TestHealthz(t *testing.T) {
	s, node := testServer(t, goodTiers(), false)
	defer node.Close()
	if code, m := get(t, s, "/healthz"); code != 200 || m["block"].(float64) != 0x1d0 {
		t.Fatalf("%d %v", code, m)
	}
}

func TestTransportErrorIs502NotNoHealthyPool(t *testing.T) {
	s, node := testServer(t, goodTiers(), false)
	node.Close() // RPC unreachable
	code, m := get(t, s, "/v1/price?pair=WBOT-USDT")
	if code != 502 || m["tiers"] != nil {
		t.Fatalf("%d %v", code, m)
	}
}
