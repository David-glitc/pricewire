package main

import (
	"flag"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
)

func main() {
	rpc := flag.String("rpc", env("RPC_URL", "https://rpc.botchain.ai"), "JSON-RPC endpoint (read-only)")
	wire := flag.String("oracle", env("PRICEWIRE", ""), "deployed PriceWire address")
	addr := flag.String("listen", env("LISTEN", "127.0.0.1:8080"), "listen address")
	win := flag.Uint("window", 1800, "default TWAP window seconds")
	flag.Parse()
	if *wire == "" {
		log.Fatal("set --oracle or PRICEWIRE to the PriceWire address")
	}
	cfg := Config{
		Wire: *wire, DefaultWin: uint32(*win), MinLiq: big.NewInt(1_000_000), DivergeWarn: 0.05,
		Tokens: map[string]Token{
			"WBOT": {"WBOT", "0xD5452816194a3784dBa983426cCe7c122F4abd30", 18},
			"USDT": {"USDT", "0xaBabc7Ddc03e501d190C676BF3d92ef0e6e87a3C", 6},
		},
		Pairs: [][2]string{{"WBOT", "USDT"}},
	}
	log.Printf("pricewire service on %s, oracle %s", *addr, strings.ToLower(*wire))
	log.Fatal(http.ListenAndServe(*addr, NewServer(cfg, NewRPC(*rpc)).Handler()))
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
