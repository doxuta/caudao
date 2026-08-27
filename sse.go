package caudao

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// meterReader wraps an SSE response body, passing every byte through
// unmodified while parsing usage events line-by-line. When onUsage reports
// the breaker has tripped, the reader emits one final synthetic error event
// and ends the stream; closing it releases the upstream connection.
//
// It never buffers more than one SSE line, so time-to-first-token overhead is
// a line scan, not a response buffer.
type meterReader struct {
	src     *bufio.Reader
	closer  io.Closer
	outbox  bytes.Buffer
	onUsage func(delta Usage) (trip bool, reason string)
	// onDone runs exactly once when the stream ends, however it ends: it
	// releases this request's budget reservation.
	onDone     func()
	doneOnce   sync.Once
	tripped    bool
	tripReason string
	done       bool

	lastOutput   int
	inputCharged bool
}

func newMeterReader(body io.ReadCloser, onUsage func(Usage) (bool, string), onDone func()) *meterReader {
	return &meterReader{
		src:     bufio.NewReaderSize(body, 32*1024),
		closer:  body,
		onUsage: onUsage,
		onDone:  onDone,
	}
}

// sseEvent mirrors the parts of Anthropic stream events we meter.
type sseEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage Usage `json:"usage"`
	} `json:"message"`
	Usage Usage `json:"usage"`
}

func (m *meterReader) Read(p []byte) (int, error) {
	for m.outbox.Len() == 0 {
		if m.done {
			m.finish()
			return 0, io.EOF
		}
		line, err := m.src.ReadBytes('\n')
		if len(line) > 0 {
			m.inspect(line)
			if m.tripped {
				// Drop the remainder of the upstream stream; emit the breaker
				// event instead and finish.
				m.outbox.Reset()
				m.outbox.WriteString(m.breakerEvent())
				m.done = true
				break
			}
			m.outbox.Write(line)
		}
		if err != nil {
			m.done = true
			if err != io.EOF && m.outbox.Len() == 0 {
				return 0, err
			}
			break
		}
	}
	return m.outbox.Read(p)
}

func (m *meterReader) Close() error {
	m.finish()
	return m.closer.Close()
}

// finish runs the completion hook once, whether the stream ended cleanly, was
// cut by the breaker, or the client hung up.
func (m *meterReader) finish() {
	if m.onDone != nil {
		m.doneOnce.Do(m.onDone)
	}
}

func (m *meterReader) inspect(line []byte) {
	payload, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:"))
	if !ok {
		return
	}
	// The space after the colon is optional: the SSE grammar (WHATWG HTML
	// 9.2.6) strips a single leading space if one is present, so "data:{...}"
	// and "data: {...}" are the same event. Requiring the space meant a
	// spec-valid stream parsed as nothing, cost nothing, and never tripped.
	payload = bytes.TrimPrefix(payload, []byte(" "))
	var ev sseEvent
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	var delta Usage
	switch ev.Type {
	case "message_start":
		u := ev.Message.Usage
		if !m.inputCharged {
			delta = u
			m.inputCharged = true
			m.lastOutput = u.OutputTokens
		}
	case "message_delta":
		if out := ev.Usage.OutputTokens; out > m.lastOutput {
			delta = Usage{OutputTokens: out - m.lastOutput}
			m.lastOutput = out
		}
	default:
		return
	}
	if delta == (Usage{}) {
		return
	}
	if trip, reason := m.onUsage(delta); trip {
		m.tripped = true
		m.tripReason = reason
	}
}

func (m *meterReader) breakerEvent() string {
	msg := struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}{Type: "error"}
	msg.Error.Type = "caudao_budget_exhausted"
	msg.Error.Message = "caudao: " + m.tripReason + " — circuit opened mid-stream"
	b, _ := json.Marshal(msg)
	return fmt.Sprintf("event: error\ndata: %s\n\n", b)
}
