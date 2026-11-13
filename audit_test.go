package caudao

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- FINDING 2: zero/missing price rates make Cost() return $0 -------------

func TestValidateRejectsBadLongPrompt(t *testing.T) {
	long := &Price{InputPerMTok: 0.5, OutputPerMTok: 2.5}
	cases := map[string]Price{
		"no threshold":   {InputPerMTok: 0.1, OutputPerMTok: 0.5, LongPrompt: long},
		"cheaper input":  {InputPerMTok: 1, OutputPerMTok: 0.5, LongPromptOverTokens: 100000, LongPrompt: long},
		"zero output":    {InputPerMTok: 0.1, OutputPerMTok: 0.5, LongPromptOverTokens: 100000, LongPrompt: &Price{InputPerMTok: 0.5}},
		"nested tier":    {InputPerMTok: 0.1, OutputPerMTok: 0.5, LongPromptOverTokens: 100000, LongPrompt: &Price{InputPerMTok: 0.5, OutputPerMTok: 2.5, LongPrompt: long}},
		"negative cache": {InputPerMTok: 0.1, OutputPerMTok: 0.5, LongPromptOverTokens: 100000, LongPrompt: &Price{InputPerMTok: 0.5, OutputPerMTok: 2.5, CacheReadMult: -1}},
	}
	for name, p := range cases {
		c := &Config{DailyTotalUSD: 25, Prices: PriceTable{"claude-haiku-5-5": p}}
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted a long_prompt price that can under-charge", name)
		}
	}
	ok := &Config{DailyTotalUSD: 25, Prices: PriceTable{"claude-haiku-5-5": {InputPerMTok: 0.1, OutputPerMTok: 0.5, LongPromptOverTokens: 100000, LongPrompt: long}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("Validate refused a good long_prompt price: %v", err)
	}
}

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
	c = &Config{DailyTotalUSD: 25, Prices: PriceTable{"claude-": {InputPerMTok: 15, OutputPerMTok: 75, WebSearchPer1K: -10}}}
	if err := c.Validate(); err == nil {
		t.Fatal("Validate accepted a negative web search fee")
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

func TestExampleConfigIsValid(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(ExampleConfig), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if p, _ := c.Prices.Lookup("claude-haiku-5-5"); p.LongPrompt == nil {
		t.Fatal("the example has no long-prompt tier for claude-haiku-5-5")
	}
}
