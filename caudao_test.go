package caudao

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig(upstream string, totalUSD float64) *Config {
	c := &Config{
		Upstream:      upstream,
		DailyTotalUSD: totalUSD,
		Prices: PriceTable{
			"claude-opus":  {InputPerMTok: 15, OutputPerMTok: 75},
			"claude-haiku": {InputPerMTok: 0.8, OutputPerMTok: 4},
			"mock-model":   {InputPerMTok: 100, OutputPerMTok: 500},
		},
	}
	if err := c.Validate(); err != nil {
		panic(err)
	}
	return c
}

func memLedger() *Ledger {
	l, _ := OpenLedger("")
	l.data.Models = map[string]ModelSpend{}
	return l
}

func TestPriceLookup(t *testing.T) {
	tab := PriceTable{
		"claude-":       {InputPerMTok: 1, OutputPerMTok: 2},
		"claude-opus-5": {InputPerMTok: 15, OutputPerMTok: 75},
	}
	if p, ok := tab.Lookup("claude-opus-5"); !ok || p.InputPerMTok != 15 {
		t.Fatalf("longest prefix should win, got %+v ok=%v", p, ok)
	}
	if p, ok := tab.Lookup("claude-haiku-4"); !ok || p.InputPerMTok != 1 {
		t.Fatalf("fallback prefix should match, got %+v ok=%v", p, ok)
	}
	if _, ok := tab.Lookup("gpt-9"); ok {
		t.Fatal("unknown model must not match")
	}
}

func TestPriceCostWithCache(t *testing.T) {
	p := Price{InputPerMTok: 10, OutputPerMTok: 20}
	u := Usage{InputTokens: 1_000_000, OutputTokens: 500_000, CacheCreationTokens: 1_000_000, CacheReadTokens: 1_000_000}
	got := p.Cost(u)
	want := 10.0 + 10.0 + 12.5 + 1.0 // in + out + 1.25x write + 0.1x read
	if fmt.Sprintf("%.6f", got) != fmt.Sprintf("%.6f", want) {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestLedgerConcurrentAndRollover(t *testing.T) {
	l := memLedger()
	day := "2026-08-26"
	l.now = func() time.Time { d, _ := time.Parse("2006-01-02", day); return d }

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Add("m", Usage{OutputTokens: 10}, 0.01)
		}()
	}
	wg.Wait()
	if m, total := l.Spent("m"); fmt.Sprintf("%.2f", total) != "1.00" || fmt.Sprintf("%.2f", m) != "1.00" {
		t.Fatalf("concurrent adds lost updates: model=%v total=%v", m, total)
	}
	day = "2026-08-27"
	if _, total := l.Spent("m"); total != 0 {
		t.Fatalf("rollover did not reset: %v", total)
	}
}

func sseStream(events ...string) io.ReadCloser {
	var b bytes.Buffer
	for _, e := range events {
		b.WriteString(e)
	}
	return io.NopCloser(&b)
}

func TestMeterReaderPassthroughAndCounting(t *testing.T) {
	in := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":51}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	var got Usage
	mr := newMeterReader(sseStream(in...), func(u Usage) (bool, string) {
		got.InputTokens += u.InputTokens
		got.OutputTokens += u.OutputTokens
		return false, ""
	})
	out, err := io.ReadAll(mr)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != strings.Join(in, "") {
		t.Fatalf("passthrough altered bytes:\n%q\nvs\n%q", out, strings.Join(in, ""))
	}
	if got.InputTokens != 100 || got.OutputTokens != 51 {
		t.Fatalf("counted %+v, want in=100 out=51", got)
	}
}

func TestMeterReaderTrips(t *testing.T) {
	in := []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n",
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":100}}\n",
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":200}}\n",
	}
	calls := 0
	mr := newMeterReader(sseStream(in...), func(u Usage) (bool, string) {
		calls++
		return calls == 2, "test budget gone" // trip on the first delta
	})
	out, _ := io.ReadAll(mr)
	s := string(out)
	if !strings.Contains(s, "caudao_budget_exhausted") || !strings.Contains(s, "test budget gone") {
		t.Fatalf("breaker event missing: %q", s)
	}
	if strings.Contains(s, "output_tokens\":200") {
		t.Fatal("stream continued after trip")
	}
}

func startProxy(t *testing.T, upstream http.Handler, budget float64) (*httptest.Server, *Ledger) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	cfg := testConfig(up.URL, budget)
	ledger := memLedger()
	p, err := NewProxy(cfg, ledger)
	if err != nil {
		t.Fatal(err)
	}
	px := httptest.NewServer(p)
	t.Cleanup(px.Close)
	return px, ledger
}

func TestBreakerTripsMidStream(t *testing.T) {
	mock := &MockUpstream{InputTokens: 1000, Deltas: 50, TokensPerDelta: 1000, Delay: time.Millisecond}
	px, ledger := startProxy(t, mock, 0.05) // $0.05 vs mock-model $500/MTok output
	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"mock-model","stream":true,"max_tokens":100000,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "caudao_budget_exhausted") {
		t.Fatalf("no breaker event in stream:\n%s", s[:min(len(s), 800)])
	}
	if strings.Count(s, "message_delta") >= mock.Deltas {
		t.Fatal("stream was not cut early")
	}
	if _, total := ledger.Spent("mock-model"); total < 0.05 {
		t.Fatalf("ledger did not record the spend that tripped: %v", total)
	}
}

func TestPreRequestRefusals(t *testing.T) {
	px, ledger := startProxy(t, DefaultMock(), 1.0)

	resp, _ := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"gpt-unknown","messages":[]}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unpriced model: got %d", resp.StatusCode)
	}
	resp.Body.Close()

	ledger.Add("mock-model", Usage{}, 2.0) // exhaust the day
	resp, _ = http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"mock-model","messages":[]}`))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("exhausted budget: got %d", resp.StatusCode)
	}
	var e struct {
		Error struct{ Type string }
	}
	json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	if e.Error.Type != "caudao_budget_exhausted" {
		t.Fatalf("error type = %q", e.Error.Type)
	}

	resp, _ = http.Post(px.URL+"/v1/other", "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unmetered endpoint: got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestNonStreamingAccounting(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":2000,"output_tokens":1000}}`)
	})
	px, ledger := startProxy(t, upstream, 5.0)
	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"msg_1"`) {
		t.Fatalf("body not passed through: %s", body)
	}
	m, _ := ledger.Spent("claude-opus-5")
	want := 2000*15.0/1e6 + 1000*75.0/1e6
	if fmt.Sprintf("%.6f", m) != fmt.Sprintf("%.6f", want) {
		t.Fatalf("accounted %v, want %v", m, want)
	}
}

func TestStatusEndpoint(t *testing.T) {
	px, ledger := startProxy(t, DefaultMock(), 9.0)
	ledger.Add("mock-model", Usage{OutputTokens: 10}, 0.25)
	resp, err := http.Get(px.URL + "/caudao/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got struct {
		TotalUSD float64 `json:"total_usd"`
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Scan()
	if err := json.Unmarshal(sc.Bytes(), &got); err != nil || got.TotalUSD != 0.25 {
		t.Fatalf("status = %s (err %v)", sc.Text(), err)
	}
}

func BenchmarkMeterReaderPassthrough(b *testing.B) {
	var chunk bytes.Buffer
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&chunk, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello world tokens\"}}\n\n")
	}
	payload := chunk.Bytes()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		mr := newMeterReader(io.NopCloser(bytes.NewReader(payload)), func(Usage) (bool, string) { return false, "" })
		io.Copy(io.Discard, mr)
	}
}
