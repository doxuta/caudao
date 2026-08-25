// Command caudao runs the spending circuit breaker.
//
//	caudao init                      # write an example caudao.json
//	caudao serve -config caudao.json -listen :8484
//	caudao mock -listen :9787        # fake upstream for testing
//	caudao demo                      # watch the breaker trip, no API key needed
//	caudao status -url http://localhost:8484
//
// Point your agent at it:
//
//	ANTHROPIC_BASE_URL=http://localhost:8484
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/doxuta/caudao"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "init":
		if _, err := os.Stat("caudao.json"); err == nil {
			die(fmt.Errorf("caudao.json already exists"))
		}
		die(os.WriteFile("caudao.json", []byte(caudao.ExampleConfig), 0o644))
		fmt.Println("wrote caudao.json — edit budgets and prices, then: caudao serve")
	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		cfgPath := fs.String("config", "caudao.json", "config file")
		listen := fs.String("listen", ":8484", "listen address")
		fs.Parse(os.Args[2:])
		cfg, err := caudao.LoadConfig(*cfgPath)
		die(err)
		ledgerPath := cfg.LedgerPath
		if ledgerPath == "" {
			ledgerPath = "caudao-ledger.json"
		}
		ledger, err := caudao.OpenLedger(ledgerPath)
		die(err)
		p, err := caudao.NewProxy(cfg, ledger)
		die(err)
		fmt.Printf("caudao: metering %s on %s (daily cap $%.2f)\n", cfg.Upstream, *listen, cfg.DailyTotalUSD)
		fmt.Printf("caudao: point your agent at it: ANTHROPIC_BASE_URL=http://localhost%s\n", *listen)
		die(http.ListenAndServe(*listen, p))
	case "mock":
		fs := flag.NewFlagSet("mock", flag.ExitOnError)
		listen := fs.String("listen", ":9787", "listen address")
		fs.Parse(os.Args[2:])
		fmt.Printf("caudao mock upstream on %s\n", *listen)
		die(http.ListenAndServe(*listen, caudao.DefaultMock()))
	case "status":
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		url := fs.String("url", "http://localhost:8484", "caudao base URL")
		fs.Parse(os.Args[2:])
		resp, err := http.Get(*url + "/caudao/status")
		die(err)
		defer resp.Body.Close()
		io.Copy(os.Stdout, resp.Body)
	case "demo":
		demo()
	default:
		usage()
	}
}

// demo wires a mock upstream and a $0.05 proxy together in-process, fires one
// streaming request, and prints what an out-of-control agent would see.
func demo() {
	upLn, err := net.Listen("tcp", "127.0.0.1:0")
	die(err)
	mock := &caudao.MockUpstream{InputTokens: 3000, Deltas: 500, TokensPerDelta: 10, Delay: 25 * time.Millisecond}
	go http.Serve(upLn, mock)

	cfg := &caudao.Config{
		Upstream:      "http://" + upLn.Addr().String(),
		DailyTotalUSD: 0.05,
		Prices:        caudao.PriceTable{"mock-model": {InputPerMTok: 1, OutputPerMTok: 500}},
	}
	die(cfg.Validate())
	ledger, err := caudao.OpenLedger("") // in-memory for the demo
	die(err)
	p, err := caudao.NewProxy(cfg, ledger)
	die(err)
	pxLn, err := net.Listen("tcp", "127.0.0.1:0")
	die(err)
	go http.Serve(pxLn, p)
	base := "http://" + pxLn.Addr().String()

	fmt.Println("⚡ caudao demo — daily budget: $0.05, mock model at $500/MTok output")
	fmt.Println("→ an 'agent' starts an expensive streaming request through the breaker...")
	fmt.Println()

	resp, err := http.Post(base+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"mock-model","stream":true,"max_tokens":100000,"messages":[{"role":"user","content":"go wild"}]}`))
	die(err)
	defer resp.Body.Close()

	deltas := 0
	sc := bufio.NewScanner(resp.Body)
	start := time.Now()
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, "message_delta"):
			deltas++
			if deltas%5 == 0 {
				_, total := spend(ledger)
				fmt.Printf("  streaming... %3d deltas, spent $%.4f\n", deltas, total)
			}
		case strings.Contains(line, "caudao_budget_exhausted"):
			fmt.Println()
			fmt.Println("💥 " + strings.TrimPrefix(line, "data: "))
		}
	}
	_, total := spend(ledger)
	fmt.Println()
	fmt.Printf("⏱  stream cut after %d deltas in %s — final spend $%.4f (cap $0.05)\n", deltas, time.Since(start).Round(time.Millisecond), total)
	fmt.Println("→ follow-up request while the breaker is open:")
	resp2, err := http.Post(base+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"mock-model","messages":[]}`))
	die(err)
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	fmt.Printf("  HTTP %d %s\n", resp2.StatusCode, strings.TrimSpace(string(body)))
}

func spend(l *caudao.Ledger) (string, float64) {
	day, _, total := l.Snapshot()
	return day, total
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: caudao init|serve|mock|demo|status")
	os.Exit(2)
}

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "caudao:", err)
		os.Exit(1)
	}
}
