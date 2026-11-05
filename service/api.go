package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Token struct {
	Symbol   string
	Address  string
	Decimals int
}

type Config struct {
	Wire        string
	Tokens      map[string]Token
	Pairs       [][2]string // symbols
	DefaultWin  uint32
	MinLiq      *big.Int
	DivergeWarn float64 // fraction between tiers' spot prices that triggers a warning
}

type Server struct {
	cfg   Config
	rpc   *RPC
	mu    sync.Mutex
	cache map[string]cacheEntry
	ttl   time.Duration
	now   func() time.Time
}

type cacheEntry struct {
	at  time.Time
	val any
}

func NewServer(cfg Config, rpc *RPC) *Server {
	return &Server{cfg: cfg, rpc: rpc, cache: map[string]cacheEntry{}, ttl: 5 * time.Second, now: time.Now}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/v1/pairs", s.pairs)
	mux.HandleFunc("/v1/price", s.price)
	mux.HandleFunc("/v1/health", s.health)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) cached(key string, f func() (any, error)) (any, error) {
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && s.now().Sub(e.at) < s.ttl {
		s.mu.Unlock()
		return e.val, nil
	}
	s.mu.Unlock()
	v, err := f()
	if err == nil {
		s.mu.Lock()
		s.cache[key] = cacheEntry{s.now(), v}
		s.mu.Unlock()
	}
	return v, err
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	bn, err := s.rpc.BlockNumber()
	if err != nil {
		fail(w, 503, "rpc unreachable: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "block": bn, "oracle": s.cfg.Wire})
}

func (s *Server) pairs(w http.ResponseWriter, r *http.Request) {
	out := []map[string]string{}
	for _, p := range s.cfg.Pairs {
		out = append(out, map[string]string{"pair": p[0] + "-" + p[1], "base": p[0], "quote": p[1]})
	}
	writeJSON(w, 200, map[string]any{"pairs": out, "default_window_seconds": s.cfg.DefaultWin})
}

func (s *Server) resolve(r *http.Request) (base, quote Token, window uint32, err error) {
	q := r.URL.Query()
	b, qq := strings.ToUpper(q.Get("base")), strings.ToUpper(q.Get("quote"))
	if p := q.Get("pair"); p != "" {
		parts := strings.Split(strings.ToUpper(p), "-")
		if len(parts) != 2 {
			return base, quote, 0, fmt.Errorf("pair must look like WBOT-USDT")
		}
		b, qq = parts[0], parts[1]
	}
	var ok1, ok2 bool
	base, ok1 = s.cfg.Tokens[b]
	quote, ok2 = s.cfg.Tokens[qq]
	if !ok1 || !ok2 {
		return base, quote, 0, fmt.Errorf("unknown token; known: %s", s.knownTokens())
	}
	window = s.cfg.DefaultWin
	if ws := q.Get("window"); ws != "" {
		n, e := strconv.Atoi(ws)
		if e != nil || n < 60 || n > 7*86400 {
			return base, quote, 0, fmt.Errorf("window must be 60..604800 seconds")
		}
		window = uint32(n)
	}
	return base, quote, window, nil
}

func (s *Server) knownTokens() string {
	names := []string{}
	for k := range s.cfg.Tokens {
		names = append(names, k)
	}
	return strings.Join(names, ",")
}

// tickPrice converts a tick into a human price of base in quote (token decimals applied).
func tickPrice(tick int64, base, quote Token) float64 {
	raw := math.Pow(1.0001, float64(tick)) // token1/token0 in raw units
	if strings.ToLower(base.Address) > strings.ToLower(quote.Address) {
		raw = 1 / raw // base is token1: price of base in quote = token0/token1
	}
	return raw * math.Pow10(base.Decimals-quote.Decimals)
}

func human(v *big.Int, dec int) string {
	f := new(big.Float).SetPrec(200).SetInt(v)
	f.Quo(f, new(big.Float).SetPrec(200).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dec)), nil)))
	return f.Text('f', dec)
}

func (s *Server) price(w http.ResponseWriter, r *http.Request) {
	base, quote, window, err := s.resolve(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	key := fmt.Sprintf("price:%s:%s:%d", base.Symbol, quote.Symbol, window)
	v, err := s.cached(key, func() (any, error) {
		one := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(base.Decimals)), nil)
		out, h, err := s.rpc.ConsultBest(s.cfg.Wire, base.Address, quote.Address, one, window, s.cfg.MinLiq)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"base": base.Symbol, "quote": quote.Symbol, "window_seconds": window,
			"price": human(out, quote.Decimals), "amount_out_raw": out.String(), "twap_tick": h.TwapTick,
			"pool": h.Pool, "history_seconds": h.HistorySeconds, "liquidity": h.Liquidity,
		}, nil
	})
	if err != nil {
		// The contract reverts when no tier is healthy: report that as 503 with the per-tier reasons.
		// Transport failures are a different thing (502): never present an outage as "no healthy pool".
		var rev *RevertError
		if !errors.As(err, &rev) {
			fail(w, 502, err.Error())
			return
		}
		hs, e2 := s.rpc.HealthAll(s.cfg.Wire, base.Address, quote.Address, window, s.cfg.MinLiq)
		if e2 != nil {
			fail(w, 502, err.Error())
			return
		}
		writeJSON(w, 503, map[string]any{"error": "no healthy pool for this pair and window", "detail": err.Error(), "tiers": hs})
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	base, quote, window, err := s.resolve(r)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	key := fmt.Sprintf("health:%s:%s:%d", base.Symbol, quote.Symbol, window)
	v, err := s.cached(key, func() (any, error) {
		hs, err := s.rpc.HealthAll(s.cfg.Wire, base.Address, quote.Address, window, s.cfg.MinLiq)
		if err != nil {
			return nil, err
		}
		type tier struct {
			Health
			SpotPrice float64 `json:"spot_price,omitempty"`
			TwapPrice float64 `json:"twap_price,omitempty"`
		}
		tiers := []tier{}
		var prices []float64
		for _, h := range hs {
			t := tier{Health: h}
			if h.Flags&1 == 0 { // pool exists
				t.SpotPrice, t.TwapPrice = tickPrice(h.SpotTick, base, quote), tickPrice(h.TwapTick, base, quote)
				if h.Liquidity != "0" {
					prices = append(prices, t.SpotPrice)
				}
			}
			tiers = append(tiers, t)
		}
		warnings := []string{}
		if len(prices) > 1 {
			lo, hi := prices[0], prices[0]
			for _, p := range prices {
				lo, hi = math.Min(lo, p), math.Max(hi, p)
			}
			if (hi-lo)/lo > s.cfg.DivergeWarn {
				warnings = append(warnings, fmt.Sprintf("fee tiers disagree by %.1f%% (arbitrage gap or thin tier); the oracle only quotes from healthy tiers", (hi-lo)/lo*100))
			}
		}
		healthy := 0
		for _, h := range hs {
			if h.Healthy {
				healthy++
			}
		}
		return map[string]any{"base": base.Symbol, "quote": quote.Symbol, "window_seconds": window,
			"healthy_tiers": healthy, "status": statusOf(healthy), "warnings": warnings, "tiers": tiers}, nil
	})
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, v)
}

func statusOf(healthy int) string {
	if healthy == 0 {
		return "down"
	}
	if healthy == 1 {
		return "degraded"
	}
	return "ok"
}
