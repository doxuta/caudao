package caudao

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// realStream is the event order of a real reply: captured from four streams of
// claude-haiku-5-5 (Claude Code 2.1.312, --include-partial-messages), every one
// had exactly one message_delta, as the second-to-last event. So output tokens
// are not reported until the reply is over.
//
// wrote, if set, gets the time of each content delta; stopped is closed when the
// handler returns, and hungUp holds when the client went away (zero if it never did).
type realStream struct {
	deltas     int
	deltaBytes int
	replyToks  int
	delay      time.Duration
	wrote      []time.Time
	hungUp     time.Time
	stopped    chan struct{}
}

func (s *realStream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.stopped != nil {
		defer close(s.stopped)
	}
	fl := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		fl.Flush()
	}
	send("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`)
	text := `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + strings.Repeat("x", s.deltaBytes) + `"}}`
	for range s.deltas {
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		if r.Context().Err() != nil {
			s.hungUp = time.Now()
			return
		}
		s.wrote = append(s.wrote, time.Now())
		send("content_block_delta", text)
	}
	send("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d}}`, s.replyToks))
	send("message_stop", `{"type":"message_stop"}`)
}

const streamReq = `{"model":"mock-model","stream":true,"max_tokens":100000,"messages":[]}`

// A runaway reply must be cut while it is running. The meter used to charge
// output only at message_delta, which a real stream sends once, after the last
// byte of the reply: every token had reached the client before the breaker moved.
func TestBreakerCutsARealCadenceStream(t *testing.T) {
	up := &realStream{deltas: 400, deltaBytes: 64, replyToks: 8000}
	px, ledger := startProxy(t, up, 0.80)

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(streamReq))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		b.WriteString(sc.Text() + "\n")
	}
	s := b.String()
	if !strings.Contains(s, "caudao_budget_exhausted") {
		t.Fatalf("no breaker event in a reply of %d deltas", up.deltas)
	}
	// Not message_stop: a cut at the final message_delta also drops that, with
	// every delta of the reply already delivered.
	if got := strings.Count(s, `data: {"type":"content_block_delta"`); got >= up.deltas {
		t.Fatalf("the whole reply was forwarded before the cut: %d of %d deltas", got, up.deltas)
	}
	// A cut stream never reports usage, and upstream billed what it generated:
	// the estimate has to stay on the ledger or the next request finds the
	// budget refilled.
	if _, total := ledger.Committed("mock-model"); total < 0.80 {
		t.Fatalf("the cut left $%.4f on the ledger, under the $0.80 cap", total)
	}
	again, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(streamReq))
	if err != nil {
		t.Fatal(err)
	}
	again.Body.Close()
	if again.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request after the cut got %d, want 429", again.StatusCode)
	}
}

// Under budget the estimate must leave no trace: the bytes estimate runs high on
// purpose, and the ledger has to end on what upstream reported.
func TestEstimateGivesWayToReportedUsage(t *testing.T) {
	// replyToks 1 is what message_start already charged: a message_delta that adds
	// nothing still has to clear the estimate.
	for _, replyToks := range []int{400, 1} {
		up := &realStream{deltas: 40, deltaBytes: 64, replyToks: replyToks}
		px, ledger := startProxy(t, up, 100)

		resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(streamReq))
		if err != nil {
			t.Fatal(err)
		}
		drain(resp)

		want := fmt.Sprintf("%.6f", mockPrice(t).Cost(Usage{InputTokens: 10, OutputTokens: replyToks}))
		if got, _ := ledger.Committed("mock-model"); fmt.Sprintf("%.6f", got) != want {
			t.Errorf("reply of %d tokens: ledger holds $%.6f, want the reported $%s", replyToks, got, want)
		}
		if got, _ := ledger.Spent("mock-model"); fmt.Sprintf("%.6f", got) != want {
			t.Errorf("reply of %d tokens: spent is $%.6f with nothing in flight, want $%s: a hold leaked", replyToks, got, want)
		}
	}
}

// A client that hangs up mid-reply does not un-bill the tokens already generated.
func TestHangUpKeepsTheEstimate(t *testing.T) {
	up := &realStream{deltas: 400, deltaBytes: 64, replyToks: 8000, delay: time.Millisecond, stopped: make(chan struct{})}
	px, ledger := startProxy(t, up, 100)

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(streamReq))
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	for seen := 0; seen < 20 && sc.Scan(); {
		if strings.HasPrefix(sc.Text(), "event: content_block_delta") {
			seen++
		}
	}
	resp.Body.Close()
	<-up.stopped
	// The proxy's handler finishes just after upstream's, so give it a moment.
	var total float64
	for range 200 {
		if _, total = ledger.Committed("mock-model"); total > mockPrice(t).Cost(Usage{InputTokens: 10, OutputTokens: 1}) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("a reply cut off by the client left only $%.6f on the ledger", total)
}

// Tool input and visible thinking are output too; only the block's signature is not.
func TestEstimateCountsEveryOutputDelta(t *testing.T) {
	for typ, field := range map[string]string{"text_delta": "text", "input_json_delta": "partial_json", "thinking_delta": "thinking"} {
		var got int
		m := newMeterReader(http.NoBody, func(Usage) (bool, string) { return false, "" }, nil)
		m.onEstimate = func(tokens int) (bool, string) { got = tokens; return false, "" }
		m.inspectPayload([]byte(fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":%q,%q:"%s"}}`, typ, field, strings.Repeat("x", 100))))
		if got != 50 {
			t.Errorf("%s: estimated %d tokens from 100 bytes, want 50", typ, got)
		}
	}
	var got int
	m := newMeterReader(http.NoBody, func(Usage) (bool, string) { return false, "" }, nil)
	m.onEstimate = func(tokens int) (bool, string) { got = tokens; return false, "" }
	m.inspectPayload([]byte(`{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"abcd"}}`))
	if got != 0 {
		t.Errorf("signature_delta estimated %d tokens, want 0", got)
	}
}

func mockPrice(t *testing.T) Price {
	t.Helper()
	price, ok := testConfig("http://x", 1).Prices.Lookup("mock-model")
	if !ok {
		t.Fatal("no mock-model price")
	}
	return price
}

func drain(resp *http.Response) {
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
	}
	resp.Body.Close()
}

// TestCutProof is the README's Proof number: over N real-cadence streams, is the
// reply cut mid-stream, how long after the delta that crossed the cap does the
// client see the cut and upstream see the hang-up, and what is on the ledger.
//
//	CAUDAO_PROOF_N=1000 go test -run TestCutProof -v -count=1 .
func TestCutProof(t *testing.T) {
	n := 20
	if v := os.Getenv("CAUDAO_PROOF_N"); v != "" {
		n, _ = strconv.Atoi(v)
	}
	const budget, deltas = 0.80, 400
	var toClient, toHangUp []time.Duration
	var maxSpent, maxReplySpent float64
	cut, forwarded := 0, 0
	for range n {
		up := &realStream{deltas: deltas, deltaBytes: 64, replyToks: 8000, delay: time.Millisecond, stopped: make(chan struct{})}
		px, ledger := startProxy(t, up, budget)
		resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(streamReq))
		if err != nil {
			t.Fatal(err)
		}
		seen, stop := 0, false
		var cutAt time.Time
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			switch line := sc.Text(); {
			case strings.HasPrefix(line, `data: {"type":"content_block_delta"`):
				seen++
			case strings.HasPrefix(line, "event: message_stop"):
				stop = true
			case strings.Contains(line, "caudao_budget_exhausted"):
				cutAt = time.Now()
			}
		}
		resp.Body.Close()
		<-up.stopped
		forwarded = max(forwarded, seen)
		_, total := ledger.Committed("mock-model")
		maxReplySpent = max(maxReplySpent, total)
		if !stop && !cutAt.IsZero() && seen < len(up.wrote) {
			// The delta that crossed the cap is the first one not forwarded.
			crossed := up.wrote[seen]
			cut++
			maxSpent = max(maxSpent, total)
			toClient = append(toClient, cutAt.Sub(crossed))
			if !up.hungUp.IsZero() {
				toHangUp = append(toHangUp, up.hungUp.Sub(crossed))
			}
		}
		px.Close()
	}
	pct := func(ds []time.Duration, p int) time.Duration {
		if len(ds) == 0 {
			return 0
		}
		slices.Sort(ds)
		return ds[(len(ds)-1)*p/100]
	}
	t.Logf("%d streams of %d deltas, cap $%.2f: %d cut mid-stream; at most %d deltas forwarded; ledger after the stream, at most $%.4f",
		n, deltas, budget, cut, forwarded, maxReplySpent)
	t.Logf("crossing to client: p50 %s p99 %s max %s", pct(toClient, 50), pct(toClient, 99), pct(toClient, 100))
	t.Logf("crossing to upstream hang-up: p50 %s p99 %s max %s (%d seen)", pct(toHangUp, 50), pct(toHangUp, 99), pct(toHangUp, 100), len(toHangUp))
	if cut != n {
		t.Fatalf("%d of %d streams were not cut mid-stream", n-cut, n)
	}
	if maxSpent > 2*budget {
		t.Fatalf("a cut stream left $%.4f on the ledger against a $%.2f cap", maxSpent, budget)
	}
}
