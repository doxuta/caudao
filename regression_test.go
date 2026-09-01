package caudao

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression tests for the August 2026 audit. Each fails against the code as
// it stood before the corresponding fix.

// A gzip-encoded upstream reply used to sail past the meter untouched: the
// meter only recognised text/event-stream and read the rest as plain JSON, so
// a compressed body parsed as nothing, cost nothing, and the breaker never
// moved. caudao now asks upstream for identity and fails closed on anything
// it cannot read.
func TestCompressedResponseIsNotUnmetered(t *testing.T) {
	var sawAcceptEncoding string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get("Accept-Encoding")
		// Ignore what was asked for and compress anyway.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		io.WriteString(gz, `{"id":"msg_1","usage":{"input_tokens":900000,"output_tokens":900000}}`)
	})
	px, _ := startProxy(t, upstream, 5.0)

	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if sawAcceptEncoding != "identity" {
		t.Errorf("upstream saw Accept-Encoding %q, want identity", sawAcceptEncoding)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unmeterable reply returned %d, want 502 — it was forwarded unmetered", resp.StatusCode)
	}
	if !strings.Contains(string(body), "caudao_unmeterable_response") {
		t.Fatalf("no fail-closed error in body: %s", body)
	}
}

// A non-JSON reply on the metered endpoint is equally unreadable and must not
// be forwarded either.
func TestUnreadableContentTypeIsRefused(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0x00, 0x01, 0x02})
	})
	px, _ := startProxy(t, upstream, 5.0)
	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("binary reply returned %d, want 502", resp.StatusCode)
	}
}

// A JSON reply whose usage cannot be read used to cost nothing at all — a
// parse failure was a free request. It now charges a pessimistic estimate.
func TestUnparseableUsageStillCharges(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","content":[]}`) // no usage block
	})
	px, ledger := startProxy(t, upstream, 50.0)
	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-5","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if m, _ := ledger.Committed("claude-opus-5"); m <= 0 {
		t.Fatal("a reply with unreadable usage cost nothing — that is a free pass")
	}
}

// The budget check used to be a time-of-check/time-of-use race: concurrent
// requests all read the same committed total, all passed, and all spent. The
// audit measured a 39x overshoot. Reservations bound it to what is in flight.
func TestConcurrentStreamsCannotBlowThroughTheBudget(t *testing.T) {
	const budget = 0.10
	mock := &MockUpstream{InputTokens: 2000, Deltas: 30, TokensPerDelta: 400, Delay: 2 * time.Millisecond}
	px, ledger := startProxy(t, mock, budget)

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(px.URL+"/v1/messages", "application/json",
				strings.NewReader(`{"model":"mock-model","stream":true,"max_tokens":100000,"messages":[]}`))
			if err != nil {
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()

	_, total := ledger.Committed("mock-model")
	// One in-flight request may exceed the cap; two dozen must not. The
	// pre-fix code reached ~39x.
	if limit := budget * 4; total > limit {
		t.Fatalf("24 concurrent streams spent $%.4f against a $%.2f cap (limit for this test $%.4f)", total, budget, limit)
	}
	if total < budget {
		t.Fatalf("spent only $%.4f — the test did not actually exercise the cap", total)
	}
}

// A backwards clock step used to wipe the day and refill the budget, because
// rollover compared dates with != rather than only moving forward.
func TestClockRollbackDoesNotRefillTheBudget(t *testing.T) {
	l := memLedger()
	day := "2026-08-26"
	l.now = func() time.Time { d, _ := time.Parse("2006-01-02", day); return d }

	l.Add("m", Usage{OutputTokens: 1000}, 5.0)
	if _, total := l.Committed("m"); total != 5.0 {
		t.Fatalf("setup: total = %v", total)
	}

	day = "2026-08-25" // NTP correction, timezone change, restored snapshot
	if _, total := l.Committed("m"); total != 5.0 {
		t.Fatalf("a backwards clock step reset the day: total = %v, want 5", total)
	}

	day = "2026-08-27" // forward again: a real new day still resets
	if _, total := l.Committed("m"); total != 0 {
		t.Fatalf("a real new day did not reset: total = %v", total)
	}
}

// Reservations must be released however the stream ends, or the budget leaks
// until restart.
func TestReservationsAreReleased(t *testing.T) {
	mock := &MockUpstream{InputTokens: 100, Deltas: 3, TokensPerDelta: 10, Delay: time.Millisecond}
	px, ledger := startProxy(t, mock, 100.0)
	for i := 0; i < 5; i++ {
		resp, err := http.Post(px.URL+"/v1/messages", "application/json",
			strings.NewReader(`{"model":"mock-model","stream":true,"messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// Give the proxy a moment to finish releasing.
	time.Sleep(100 * time.Millisecond)
	withReservations, _ := ledger.Spent("mock-model")
	committed, _ := ledger.Committed("mock-model")
	if diff := withReservations - committed; diff > 1e-9 {
		t.Fatalf("$%.6f still reserved after every stream finished", diff)
	}
}

func TestZeroPriceIsRefusedAtConfigTime(t *testing.T) {
	for _, p := range []Price{{InputPerMTok: 0, OutputPerMTok: 5}, {InputPerMTok: 5, OutputPerMTok: 0}} {
		c := &Config{DailyTotalUSD: 1, Prices: PriceTable{"m": p}}
		if err := c.Validate(); err == nil {
			t.Fatalf("a $0 rate (%+v) was accepted; the breaker could never trip", p)
		}
	}
}

func TestUpstreamMustBeDialable(t *testing.T) {
	for _, raw := range []string{"api.anthropic.com", "localhost:8484", "://x", "ftp://x"} {
		c := &Config{Upstream: raw, DailyTotalUSD: 1, Prices: PriceTable{"m": {InputPerMTok: 1, OutputPerMTok: 1}}}
		if err := c.Validate(); err == nil {
			t.Errorf("undialable upstream %q accepted", raw)
		}
	}
	ok := &Config{Upstream: "https://gw.example.com/anthropic", DailyTotalUSD: 1, Prices: PriceTable{"m": {InputPerMTok: 1, OutputPerMTok: 1}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("a valid upstream was refused: %v", err)
	}
}

// The SSE meter used to require the exact prefix "data: " (with a space).
// The space after the colon is optional in the Server-Sent Events grammar --
// WHATWG HTML 9.2.6 strips a single leading space if present -- so an upstream
// emitting "data:{...}" is spec-correct and semantically identical. caudao
// parsed none of those lines, metered nothing, charged nothing, and the
// breaker never moved: an unbounded stream at zero recorded cost, which is the
// exact failure mode the tool exists to prevent.
func TestSpaceLessDataPrefixIsStillMetered(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// No space after the colon -- valid SSE, and what several
		// Anthropic-compatible gateways emit.
		send := func(event, data string) {
			fmt.Fprintf(w, "event: %s\ndata:%s\n\n", event, data)
			if fl != nil {
				fl.Flush()
			}
		}
		send("message_start",
			`{"type":"message_start","message":{"usage":{"input_tokens":2000,"output_tokens":1}}}`)
		out := 1
		for i := 0; i < 50; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Millisecond):
			}
			out += 1000
			send("message_delta",
				fmt.Sprintf(`{"type":"message_delta","usage":{"output_tokens":%d}}`, out))
		}
	})

	px, ledger := startProxy(t, upstream, 0.05) // mock-model is $500/MTok output
	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"mock-model","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if spent, _ := ledger.Committed("mock-model"); spent <= 0 {
		t.Fatalf("a spec-valid \"data:{...}\" stream cost $%.6f -- it was forwarded unmetered", spent)
	}
	if !strings.Contains(string(body), "caudao_budget_exhausted") {
		t.Fatalf("breaker never tripped on a spec-valid \"data:{...}\" stream:\n%s",
			string(body)[:min(len(body), 600)])
	}
}

// An SSE event may carry its payload across several "data:" lines. WHATWG
// HTML 9.2.6 concatenates the data fields of one event with "\n" and
// dispatches at the blank line, so a pretty-printed JSON body split over five
// lines is the same event as the one-line form -- any gateway that re-emits
// with an indenting marshaller produces it. The meter unmarshalled each line
// on its own, so every line failed to parse, no usage was ever seen, the
// ledger stayed at zero and the breaker could not trip: the same fail-open as
// the "data:" prefix bug, from the same wrong assumption that one line is one
// event.
func TestMultiLineDataEventIsStillMetered(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Emit one event with its JSON spread over multiple data: lines.
		sendSplit := func(event, data string) {
			fmt.Fprintf(w, "event: %s\n", event)
			for _, l := range strings.Split(data, "\n") {
				fmt.Fprintf(w, "data: %s\n", l)
			}
			fmt.Fprint(w, "\n")
			if fl != nil {
				fl.Flush()
			}
		}
		sendSplit("message_start", "{\n  \"type\": \"message_start\",\n  \"message\": {\n    \"usage\": {\n      \"input_tokens\": 2000,\n      \"output_tokens\": 1\n    }\n  }\n}")
		out := 1
		for i := 0; i < 50; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Millisecond):
			}
			out += 1000
			sendSplit("message_delta",
				fmt.Sprintf("{\n  \"type\": \"message_delta\",\n  \"usage\": {\n    \"output_tokens\": %d\n  }\n}", out))
		}
	})

	px, ledger := startProxy(t, upstream, 0.05) // mock-model is $500/MTok output
	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"mock-model","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if spent, _ := ledger.Committed("mock-model"); spent <= 0 {
		t.Fatalf("a spec-valid multi-line \"data:\" stream cost $%.6f -- it was forwarded unmetered", spent)
	}
	if !strings.Contains(string(body), "caudao_budget_exhausted") {
		t.Fatalf("breaker never tripped on a spec-valid multi-line \"data:\" stream:\n%s",
			string(body)[:min(len(body), 600)])
	}
}

// Reassembling multi-line events must not eat "data: [DONE]", the
// OpenAI-compatible terminator. It is a single-line, legitimately non-JSON
// payload: unparseable is the normal case for it, not a signal that more
// lines are coming, and the client has to receive it byte for byte.
func TestDoneSentinelIsForwardedVerbatim(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	})

	px, _ := startProxy(t, upstream, 100.0) // budget far above the spend: no trip
	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"mock-model","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("the [DONE] sentinel did not reach the client:\n%q", string(body))
	}
}

// The meter's contract is that every byte upstream sends reaches the client
// unmodified. Reassembly holds lines back until the event boundary, so an
// off-by-one in the flush would drop or double a newline without changing
// anything the other tests assert on.
func TestMultiLineStreamIsForwardedByteExact(t *testing.T) {
	const wire = "event: message_start\n" +
		"data: {\n" +
		"data:   \"type\": \"message_start\",\n" +
		"data:   \"message\": {\"usage\": {\"input_tokens\": 10, \"output_tokens\": 1}}\n" +
		"data: }\n" +
		"\n" +
		": a comment line, no data field at all\n" +
		"\n" +
		"event: message_delta\n" +
		"data:{\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n" +
		"\n" +
		"data: [DONE]\n" +
		"\n"

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, wire)
	})

	px, _ := startProxy(t, upstream, 100.0)
	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"mock-model","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != wire {
		t.Fatalf("stream was not forwarded byte-exact\n got: %q\nwant: %q", string(body), wire)
	}
}

// A stream whose final event never gets its terminating blank line -- upstream
// closed the connection first -- must still be metered and still be forwarded.
// Holding the last event for a boundary that never arrives would lose both.
func TestUnterminatedFinalEventIsMeteredAndFlushed(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1000,\"output_tokens\":1}}}\n\n")
		// No trailing blank line after this one.
		io.WriteString(w, "event: message_delta\ndata: {\n")
		io.WriteString(w, "data:  \"type\": \"message_delta\", \"usage\": {\"output_tokens\": 4000}\n")
		io.WriteString(w, "data: }\n")
	})

	px, ledger := startProxy(t, upstream, 100.0)
	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"mock-model","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), `"output_tokens": 4000`) {
		t.Fatalf("the unterminated final event never reached the client:\n%q", string(body))
	}
	// 1000 input at $100/MTok = $0.10; 4000 output at $500/MTok = $2.00.
	if _, total := ledger.Committed("mock-model"); total < 2.0 {
		t.Fatalf("unterminated final event was not metered: total $%.6f, want >= $2.00", total)
	}
}
