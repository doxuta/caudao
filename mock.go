package caudao

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// MockUpstream is a fake Anthropic /v1/messages endpoint that streams
// synthetic SSE with realistic usage events, so the breaker can be
// demonstrated and tested without an API key or a single real token.
type MockUpstream struct {
	InputTokens    int           // billed at message_start
	Deltas         int           // number of message_delta events
	TokensPerDelta int           // output tokens added per delta
	Delay          time.Duration // pause between deltas
}

// DefaultMock streams enough tokens to trip any small demo budget.
func DefaultMock() *MockUpstream {
	return &MockUpstream{InputTokens: 4000, Deltas: 200, TokensPerDelta: 250, Delay: 40 * time.Millisecond}
}

func (m *MockUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	send := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if fl != nil {
			fl.Flush()
		}
	}

	send("message_start", fmt.Sprintf(
		`{"type":"message_start","message":{"id":"msg_mock","model":%q,"usage":{"input_tokens":%d,"output_tokens":1}}}`,
		req.Model, m.InputTokens))

	out := 1
	for i := 0; i < m.Deltas; i++ {
		select {
		case <-r.Context().Done():
			return // client (or the breaker) hung up — stop burning
		case <-time.After(m.Delay):
		}
		send("content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"mock tokens flowing... "}}`)
		out += m.TokensPerDelta
		send("message_delta", fmt.Sprintf(
			`{"type":"message_delta","delta":{"stop_reason":null},"usage":{"output_tokens":%d}}`, out))
	}
	send("message_stop", `{"type":"message_stop"}`)
}
