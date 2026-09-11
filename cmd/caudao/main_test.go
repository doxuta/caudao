package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/doxuta/caudao"
)

// The demo counts the message_delta events it sees go past. An SSE event is
// two lines here -- "event: message_delta" and its "data: {...}" line -- and
// both contain the substring "message_delta", so a substring match anywhere in
// the line counted every event twice and the demo reported roughly double the
// number of deltas that actually streamed.
func TestDemoCountsEventsNotLines(t *testing.T) {
	// Same wiring and the same numbers as demo().
	mock := &caudao.MockUpstream{InputTokens: 3000, Deltas: 500, TokensPerDelta: 10, Delay: 25 * time.Millisecond}
	up := httptest.NewServer(mock)
	defer up.Close()
	cfg := &caudao.Config{
		Upstream:      up.URL,
		DailyTotalUSD: 0.05,
		Prices:        caudao.PriceTable{"mock-model": {InputPerMTok: 1, OutputPerMTok: 500}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ledger, err := caudao.OpenLedger("")
	if err != nil {
		t.Fatal(err)
	}
	p, err := caudao.NewProxy(cfg, ledger)
	if err != nil {
		t.Fatal(err)
	}
	px := httptest.NewServer(p)
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"mock-model","stream":true,"max_tokens":100000,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// counted is what the demo prints; streamed is the ground truth, taken by
	// decoding each data payload instead of matching a substring.
	counted, streamed := 0, 0
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if isDeltaEvent(line) {
			counted++
		}
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(payload), &ev) == nil && ev.Type == "message_delta" {
			streamed++
		}
	}
	if streamed == 0 {
		t.Fatal("no message_delta events streamed at all")
	}
	if counted != streamed {
		t.Fatalf("demo would report %d deltas but %d message_delta events streamed", counted, streamed)
	}
}
