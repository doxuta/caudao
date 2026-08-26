package caudao

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- FINDING 2: zero/missing price rates make Cost() return $0 -------------

func TestValidateRejectsNonPositivePrice(t *testing.T) {
	cases := map[string]PriceTable{
		"output omitted":  {"claude-": {InputPerMTok: 15}},
		"input omitted":   {"claude-": {OutputPerMTok: 75}},
		"both zero":       {"claude-": {}},
		"negative output": {"claude-": {InputPerMTok: 15, OutputPerMTok: -1}},
	}
	for name, prices := range cases {
		c := &Config{DailyTotalUSD: 25, Prices: prices}
		if err := c.Validate(); err == nil {
			t.Fatalf("%s: Validate accepted a price that can never trip the breaker", name)
		}
	}
	c := &Config{DailyTotalUSD: 25, Prices: PriceTable{"claude-": {InputPerMTok: 15, OutputPerMTok: 75, CacheReadMult: -1}}}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted a negative cache multiplier")
	}
}

// ---- FINDING 10: upstream URL validation and path prefix ------------------

func TestValidateRejectsUnusableUpstream(t *testing.T) {
	for _, raw := range []string{"api.anthropic.com", "localhost:8484", "https://", "ftp://api.anthropic.com"} {
		c := &Config{
			Upstream:      raw,
			DailyTotalUSD: 25,
			Prices:        PriceTable{"claude-": {InputPerMTok: 15, OutputPerMTok: 75}},
		}
		if err := c.Validate(); err == nil {
			t.Fatalf("Validate accepted unusable upstream %q", raw)
		}
		c.Upstream = raw
		if _, err := NewProxy(c, memLedger()); err == nil {
			t.Fatalf("NewProxy accepted unusable upstream %q", raw)
		}
	}
}

func TestUpstreamPathPrefixIsPreserved(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"usage":{"input_tokens":10,"output_tokens":10}}`))
	}))
	defer up.Close()

	cfg := testConfig(up.URL+"/anthropic-gateway", 5)
	p, err := NewProxy(cfg, memLedger())
	if err != nil {
		t.Fatal(err)
	}
	px := httptest.NewServer(p)
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","max_tokens":16,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotPath != "/anthropic-gateway/v1/messages" {
		t.Fatalf("upstream path prefix dropped: got %q", gotPath)
	}
}
