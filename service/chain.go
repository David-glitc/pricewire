package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// Function selectors of PriceWire (cast sig).
const (
	selHealthAll   = "8a2522bb" // healthAll(address,address,uint32,uint128)
	selConsultBest = "ff074de0" // consultBest(address,address,uint128,uint32,uint128)
)

var flagNames = []struct {
	bit  uint8
	name string
}{{1, "NO_POOL"}, {2, "LOW_LIQUIDITY"}, {4, "SHORT_HISTORY"}, {8, "SPOT_DEVIATION"}, {16, "LOCKED"}}

var feeTiers = [4]uint{100, 500, 3000, 10000}

// Health mirrors PriceWire.Health.
type Health struct {
	Fee            uint     `json:"fee"`
	Pool           string   `json:"pool"`
	Flags          uint8    `json:"flags_raw"`
	FlagNames      []string `json:"flags"`
	Healthy        bool     `json:"healthy"`
	Liquidity      string   `json:"liquidity"`
	Cardinality    uint16   `json:"cardinality"`
	CardinalityNxt uint16   `json:"cardinality_next"`
	HistorySeconds uint32   `json:"history_seconds"`
	SpotTick       int64    `json:"spot_tick"`
	TwapTick       int64    `json:"twap_tick"`
	DeviationTicks uint32   `json:"deviation_ticks"`
}

// RevertError is a JSON-RPC level error (for eth_call: the call reverted), as opposed to a transport failure.
type RevertError struct{ Msg string }

func (e *RevertError) Error() string { return "rpc error: " + e.Msg }

// RPC is a minimal JSON-RPC client (eth_call, eth_blockNumber).
type RPC struct {
	URL    string
	Client *http.Client
}

func NewRPC(url string) *RPC { return &RPC{URL: url, Client: &http.Client{Timeout: 20 * time.Second}} }

func (r *RPC) do(method string, params []any, out any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := r.Client.Post(r.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("bad rpc response: %w", err)
	}
	if env.Error != nil {
		return &RevertError{Msg: env.Error.Message + " " + env.Error.Data}
	}
	return json.Unmarshal(env.Result, out)
}

func (r *RPC) Call(to string, data []byte) ([]byte, error) {
	var res string
	if err := r.do("eth_call", []any{map[string]string{"to": to, "data": "0x" + hex.EncodeToString(data)}, "latest"}, &res); err != nil {
		return nil, err
	}
	return hex.DecodeString(strings.TrimPrefix(res, "0x"))
}

func (r *RPC) BlockNumber() (uint64, error) {
	var res string
	if err := r.do("eth_blockNumber", []any{}, &res); err != nil {
		return 0, err
	}
	n, ok := new(big.Int).SetString(strings.TrimPrefix(res, "0x"), 16)
	if !ok {
		return 0, errors.New("bad block number")
	}
	return n.Uint64(), nil
}

// ---- ABI helpers (static types only)

func word(b []byte, i int) []byte { return b[i*32 : (i+1)*32] }

func addrWord(a string) ([]byte, error) {
	a = strings.TrimPrefix(strings.ToLower(a), "0x")
	raw, err := hex.DecodeString(a)
	if err != nil || len(raw) != 20 {
		return nil, fmt.Errorf("bad address %q", a)
	}
	return append(make([]byte, 12), raw...), nil
}

func uintWord(v *big.Int) []byte { return v.FillBytes(make([]byte, 32)) }

func signed(w []byte) int64 {
	v := new(big.Int).SetBytes(w)
	if w[0]&0x80 != 0 { // two's complement 256-bit
		v.Sub(v, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	return v.Int64()
}

func decodeHealth(b []byte, fee uint) Health {
	pool := "0x" + hex.EncodeToString(word(b, 0)[12:])
	flags := uint8(new(big.Int).SetBytes(word(b, 1)).Uint64())
	h := Health{
		Fee: fee, Pool: pool, Flags: flags, Healthy: flags == 0,
		Liquidity:      new(big.Int).SetBytes(word(b, 2)).String(),
		Cardinality:    uint16(new(big.Int).SetBytes(word(b, 3)).Uint64()),
		CardinalityNxt: uint16(new(big.Int).SetBytes(word(b, 4)).Uint64()),
		HistorySeconds: uint32(new(big.Int).SetBytes(word(b, 5)).Uint64()),
		SpotTick:       signed(word(b, 6)), TwapTick: signed(word(b, 7)),
		DeviationTicks: uint32(new(big.Int).SetBytes(word(b, 8)).Uint64()),
		FlagNames:      []string{},
	}
	for _, f := range flagNames {
		if flags&f.bit != 0 {
			h.FlagNames = append(h.FlagNames, f.name)
		}
	}
	return h
}

// HealthAll calls PriceWire.healthAll and decodes the 4 fee tiers.
func (r *RPC) HealthAll(wire, a, b string, window uint32, minLiq *big.Int) ([4]Health, error) {
	var out [4]Health
	wa, err := addrWord(a)
	if err != nil {
		return out, err
	}
	wb, err := addrWord(b)
	if err != nil {
		return out, err
	}
	sel, _ := hex.DecodeString(selHealthAll)
	data := append(sel, wa...)
	data = append(data, wb...)
	data = append(data, uintWord(big.NewInt(int64(window)))...)
	data = append(data, uintWord(minLiq)...)
	res, err := r.Call(wire, data)
	if err != nil {
		return out, err
	}
	if len(res) != 36*32 {
		return out, fmt.Errorf("unexpected healthAll length %d", len(res))
	}
	for i := 0; i < 4; i++ {
		out[i] = decodeHealth(res[i*9*32:(i+1)*9*32], feeTiers[i])
	}
	return out, nil
}

// ConsultBest calls PriceWire.consultBest: amountOut plus the health of the chosen pool. A revert means no healthy pool.
func (r *RPC) ConsultBest(wire, tokenIn, tokenOut string, amountIn *big.Int, window uint32, minLiq *big.Int) (*big.Int, Health, error) {
	wa, err := addrWord(tokenIn)
	if err != nil {
		return nil, Health{}, err
	}
	wb, err := addrWord(tokenOut)
	if err != nil {
		return nil, Health{}, err
	}
	sel, _ := hex.DecodeString(selConsultBest)
	data := append(sel, wa...)
	data = append(data, wb...)
	data = append(data, uintWord(amountIn)...)
	data = append(data, uintWord(big.NewInt(int64(window)))...)
	data = append(data, uintWord(minLiq)...)
	res, err := r.Call(wire, data)
	if err != nil {
		return nil, Health{}, err
	}
	if len(res) != 10*32 {
		return nil, Health{}, fmt.Errorf("unexpected consultBest length %d", len(res))
	}
	return new(big.Int).SetBytes(res[:32]), decodeHealth(res[32:], 0), nil
}
