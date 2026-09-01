# caudao ⚡

**A fail-closed spending circuit breaker for autonomous AI agents.**
Single Go binary, zero dependencies. Point `ANTHROPIC_BASE_URL` at it and an
unattended overnight agent *physically cannot* burn $300.

Named after the household circuit breaker: when the load gets dangerous, it
doesn't warn. It cuts.

```
$ caudao demo
⚡ caudao demo — daily budget: $0.05, mock model at $500/MTok output
→ an 'agent' starts an expensive streaming request through the breaker...

  streaming...   5 deltas, spent $0.0185
  streaming...  10 deltas, spent $0.0285
  streaming...  15 deltas, spent $0.0435

💥 {"type":"error","error":{"type":"caudao_budget_exhausted","message":"caudao: daily budget for mock-model exhausted ($0.0535 of $0.05) — circuit opened mid-stream"}}

⏱  stream cut after 19 deltas in 258ms — final spend $0.0535 (cap $0.05)
→ follow-up request while the breaker is open:
  HTTP 429 {"error":{"message":"caudao: daily budget for mock-model exhausted…"}}
```

That demo needs **no API key** — a mock upstream ships in the binary.

## Why

Agent frameworks retry. Loops run overnight. A prompt bug can turn one task
into ten thousand Opus calls. Client-side budget flags only work if every
client honors them; caudao sits **under** all of them, at the network layer,
and enforces the budget on the live token stream:

- **Metered mid-stream** — usage is parsed from the SSE events as they flow
  (`message_start` for input + cache tokens, `message_delta` for output), so
  the breaker trips *during* a runaway response, not after it.
- **Fail-closed** — a model with no configured price is refused; a $0 price is
  refused at startup; a POST to an endpoint caudao doesn't meter is refused; and a
  reply caudao cannot read (compressed, or not JSON on a metered endpoint) is
  answered with 502 rather than forwarded unmetered. Upstream is asked for
  `Accept-Encoding: identity` so the meter can always see the stream.
- **Durable ledger** — daily spend survives restarts (atomic JSON writes); a
  tripped breaker stays open until local midnight, and the day only ever rolls
  FORWARD, so a backwards clock step cannot refill the budget.
- **Reserved, not raced** — each in-flight request holds a small reservation
  against its budget, so concurrent streams cannot all pass the same check and
  spend the same headroom (before this, 24 concurrent streams overshot a $0.10
  cap by 48x).
- **Invisible otherwise** — bytes pass through unmodified (no response
  buffering: one line on the common path, one event while reassembling a
  multi-line `data:` field, ~150 MB/s metering throughput); auth headers are never
  read, stored, or logged.

## Use

```bash
go install github.com/doxuta/caudao/cmd/caudao@latest
caudao init            # writes caudao.json — set YOUR budgets and prices
caudao serve           # :8484

export ANTHROPIC_BASE_URL=http://localhost:8484
# run your agent as usual; check spend anytime:
caudao status          # {"day":"2026-08-26","total_usd":3.41,...}
```

Config is deliberately boring JSON — daily total cap, per-model caps, and a
price table you own (prices change; keeping them current is your job, being
suspicious of unknown models is caudao's):

```json
{
  "daily_total_usd": 25.0,
  "daily_per_model_usd": { "claude-opus": 15.0 },
  "prices": {
    "claude-opus":  { "input_per_mtok": 15.0, "output_per_mtok": 75.0 },
    "claude-sonnet": { "input_per_mtok": 3.0, "output_per_mtok": 15.0 }
  }
}
```

Cache tokens are billed too (1.25× input for cache writes, 0.1× for reads,
both overridable per model).

## What it is not

Not an auth gateway, not a router, not multi-provider, not a proxy for your
secrets — one job, done fail-closed. It meters `POST /v1/messages`
(streaming and non-streaming), passes GETs and `count_tokens` through, and
refuses everything else.

## Provenance

caudao is the honest, minimal public distillation of the budget layer I run
in production on my own self-hosted LLM gateway that routes all my coding
agents' traffic (per-model cost caps, deploy locks, allowlists). That system
is private; this is the part of it everyone running agents actually needs,
rebuilt clean with tests: SSE metering verified byte-for-byte, breaker
behavior tested mid-stream, ledger race-clean under `-race`.

## Tiếng Việt

Cầu dao chống cháy ví cho AI agent: proxy Go một binary, đo token ngay giữa
SSE stream và ngắt kết nối đúng lúc ngân sách ngày cạn — agent chạy qua đêm
không thể đốt quá số tiền bạn cho phép. Fail-closed: model không có giá thì
từ chối, endpoint không đo được thì chặn. `caudao demo` xem cầu dao sập trong
60 giây, không cần API key.

MIT © Xuan Tai Doan — built with an AI coding agent under human review; see
[DESIGN.md](DESIGN.md).
