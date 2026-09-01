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
// unmodified while parsing usage events. When onUsage reports the breaker has
// tripped, the reader emits one final synthetic error event and ends the
// stream; closing it releases the upstream connection.
//
// A data field whose payload is valid JSON on its own is metered and forwarded
// the moment its line is read, so the common case buffers one line and
// time-to-first-token overhead is a line scan. A payload that does not parse
// alone may be one field of a multi-line event -- SSE joins the data fields of
// an event with "\n" and dispatches at the blank line -- so those lines are
// held until the event boundary and the joined payload is parsed then. That
// path buffers one event, capped at maxEventBytes.
type meterReader struct {
	src     *bufio.Reader
	closer  io.Closer
	outbox  bytes.Buffer
	onUsage func(delta Usage) (trip bool, reason string)
	// pending holds the raw lines of an event being reassembled, and data
	// holds its data fields joined with "\n". Both are empty on the fast
	// path. Nothing in pending has reached the client yet, so an event that
	// trips the breaker can still be dropped rather than forwarded.
	pending   bytes.Buffer
	data      bytes.Buffer
	joining   bool
	overLimit bool
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
			m.feed(line)
		}
		if err != nil {
			// Upstream stopped without a closing blank line: meter and
			// release whatever event was still being reassembled.
			m.closeEvent()
		}
		if m.tripped {
			// Drop the remainder of the upstream stream -- including any
			// held-back lines of the offending event, which have not been
			// forwarded -- and emit the breaker event instead.
			m.outbox.Reset()
			m.outbox.WriteString(m.breakerEvent())
			m.done = true
			break
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

// maxEventBytes bounds reassembly. An Anthropic usage event is a few hundred
// bytes; anything past this is not an event we can meter, so it is released
// to the client rather than held.
const maxEventBytes = 1 << 20

// feed routes one raw line: either straight to the outbox, or into the
// reassembly buffer when it may be part of a multi-line event.
func (m *meterReader) feed(line []byte) {
	field := bytes.TrimRight(line, "\r\n")

	if m.joining {
		if len(field) == 0 {
			// Event boundary: parse what the data fields spelled out
			// together, then release the whole event.
			m.inspectPayload(m.data.Bytes())
			m.pending.Write(line)
			m.release()
			return
		}
		if payload, ok := bytes.CutPrefix(field, []byte("data:")); ok {
			if m.data.Len() > 0 {
				m.data.WriteByte('\n')
			}
			m.data.Write(bytes.TrimPrefix(payload, []byte(" ")))
		}
		// Non-data fields (event:, id:, retry:, comments) belong to the same
		// event and are held with it so the block stays contiguous.
		m.pending.Write(line)
		if m.pending.Len() > maxEventBytes {
			// Too large to be a usage event. Release it and stop holding
			// lines until the next boundary.
			m.release()
			m.overLimit = true
		}
		return
	}

	if m.overLimit {
		m.outbox.Write(line)
		if len(field) == 0 {
			m.overLimit = false
		}
		return
	}

	payload, ok := bytes.CutPrefix(field, []byte("data:"))
	if !ok {
		m.outbox.Write(line)
		return
	}
	// The space after the colon is optional: the SSE grammar (WHATWG HTML
	// 9.2.6) strips a single leading space if one is present, so "data:{...}"
	// and "data: {...}" are the same event. Requiring the space meant a
	// spec-valid stream parsed as nothing, cost nothing, and never tripped.
	payload = bytes.TrimPrefix(payload, []byte(" "))
	if json.Valid(payload) {
		// Fast path: a complete payload on one line. Meter and forward it
		// immediately, exactly as before -- no reassembly, no added latency.
		m.inspectPayload(payload)
		m.outbox.Write(line)
		return
	}
	// Not parseable alone. It may be the first field of an event whose
	// payload is spread over several data: lines, so hold it.
	m.joining = true
	m.data.Write(payload)
	m.pending.Write(line)
}

// release forwards the held lines of an event and returns to the fast path.
func (m *meterReader) release() {
	m.outbox.Write(m.pending.Bytes())
	m.pending.Reset()
	m.data.Reset()
	m.joining = false
}

// closeEvent ends an event that upstream never terminated with a blank line.
func (m *meterReader) closeEvent() {
	if !m.joining {
		return
	}
	m.inspectPayload(m.data.Bytes())
	m.release()
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

// inspectPayload meters one event's data payload: the bytes of a single-line
// data field, or the data fields of a multi-line event joined with "\n".
func (m *meterReader) inspectPayload(payload []byte) {
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
