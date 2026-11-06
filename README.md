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
  streaming...  10 deltas, spent $0.0335
  streaming...  15 deltas, spent $0.0485

💥 {"type":"error","error":{"type":"caudao_budget_exhausted","message":"caudao: daily total budget exhausted ($0.0515 of $0.05) — circuit opened mid-stream"}}

⏱  stream cut after 15 deltas in 431ms — final spend $0.0515 (cap $0.05)
→ follow-up request while the breaker is open:
  HTTP 402 {"error":{"message":"caudao: daily total budget exhausted ($0.0515 of $0.05) — request refused","type":"caudao_budget_exhausted"},"type":"error"}
```

That demo needs **no API key** — a mock upstream ships in the binary.

## Why

Agent frameworks retry. Loops run overnight. A prompt bug can turn one task
into ten thousand Opus calls. Client-side budget flags only work if every
client honors them; caudao sits **under** all of them, at the network layer,
and enforces the budget on the live token stream:

- **Metered mid-stream** — input and cache tokens come from `message_start`.
  Output tokens are reported only by `message_delta`, which a real reply sends
  once, after its last byte ([Proof](#proof)), so caudao estimates them from the
  bytes of each content delta as it flows and trips on that. Reported usage
  replaces the estimate when it arrives; a reply cut before then keeps it.
- **Fail-closed** — a model with no configured price is refused; a $0 price is
  refused at startup; a POST to an endpoint caudao doesn't meter is refused; and a
  reply caudao cannot read (compressed, or not JSON on a metered endpoint) is
  answered with 502 rather than forwarded unmetered — and charged a pessimistic
  estimate anyway, because upstream already billed those tokens and a free
  refusal would let a retrying agent loop forever with the ledger at zero.
  Upstream is asked for `Accept-Encoding: identity` so the meter can always see
  the stream.
- **Durable ledger** — daily spend survives restarts (atomic JSON writes); a
  tripped breaker stays open until local midnight, and the day only ever rolls
  FORWARD, so a backwards clock step cannot refill the budget.
- **Reserved, not raced** — each in-flight request holds a small reservation
  against its budget, so concurrent streams cannot all pass the same check and
  spend the same headroom (before this, 24 concurrent streams overshot a $0.10
  cap by 48x).
- **Invisible otherwise** — bytes pass through unmodified (no response
  buffering: one line on the common path, one event while reassembling a
  multi-line `data:` field, ~115 MB/s metering throughput); auth headers are never
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

`daily_per_model_usd` keys are model-name prefixes, and each prefix is one
shared ceiling: with `"claude-opus": 15.0`, `claude-opus-4-1` and
`claude-opus-4-5` together may spend $15, not $15 each. The longest matching
prefix owns a model, so a narrower entry such as `"claude-opus-5": 5.0` is its
own pool. Models matching no prefix are held only by `daily_total_usd`.

Cache tokens are billed too (1.25× input for cache writes, 0.1× for reads,
both overridable per model).

## Proof

Measured 2026-11-05 on an M3 Pro, with `claude-haiku-5-5` through Claude Code 2.1.312.

**Real streams report output once, at the end.** In all 4 replies captured
(`--include-partial-messages`; 153, 240, 172 and 289 content deltas) there was
exactly one `message_delta`, and it was the second-to-last event. That is what
these replies did, not a promise about the API. Until 2026-11-05 caudao charged
output only from that event, and its own mock sent one after every delta, so
every test passed while a real reply could not be cut.

**The cut, on a test server with that event order.** Replies of 400 deltas, each
64 bytes, 8,000 tokens reported at the end, $4.00 at the test price, against a
$0.80 cap:

| | streams cut mid-reply | deltas reaching the client | ledger after the stream |
|---|---|---|---|
| before the fix | 0 of 50 | 400 of 400 | $4.00, 5x the cap |
| after the fix | 1,000 of 1,000 | at most 49 of 400 | at most $0.8015 |

From the moment upstream wrote the delta that crossed the cap:

| over 1,000 streams | p50 | p99 | max |
|---|---|---|---|
| client has the breaker event | 268 µs | 521 µs | 2.2 ms |
| upstream sees the hang-up | 1.26 ms | 1.35 ms | 2.7 ms |

This is loopback and the proxy's own cost, nothing more. The test server only
checks for a hang-up between deltas, which are 1 ms apart. Run it:
`CAUDAO_PROOF_N=1000 go test -run TestCutProof -v -count=1 .`

**One real reply through a real proxy.** A 500-word essay with a $0.004 cap (output
priced at a placeholder $5/MTok, input near zero). The client got 136 deltas and
1,593 bytes of text and no `message_delta`; the same prompt uncapped ran to 3,157
bytes. The ledger held 801 output tokens, 798 of them the estimate.

**At the cap the agent stops, and is told why.** Until v0.3.0 a budget refusal was a
429. The breaker stays open until midnight, so every retry was wasted. Measured
2026-11-06, Claude Code 2.1.312 and `anthropic` 2.6.0 for Python, against a local
server that always answers with the same refusal:

| refusal | Claude Code requests | what Claude Code prints | Python SDK requests |
|---|---|---|---|
| 429 | 11, the last at 183 s | `Server is temporarily limiting requests (not your usage limit)`, then caudao's text | 3 |
| 429 with `x-should-retry: false` | 1 | the same wrong line | not run |
| 402, what caudao sends now | 1 | `402 caudao: daily total budget exhausted`, exit 1 | 1 |

Through a real `caudao serve` on the real API, one capped reply: cut, exit 1, 7 seconds
from start to finish. The run before the fix (2026-11-05) spent 137 seconds in 429
retries after its cut. The TypeScript and Go SDKs have the same retry rule (408, 409,
429, every 5xx) in their source; only Python was run.

**What still fails.**

- **Hidden thinking is invisible until the end.** `thinking_delta` carried 0 bytes
  in all 4 replies while 197 to 3,076 thinking tokens were billed, 16% to 74% of
  each reply's output. Those tokens are charged at the final `message_delta`, so a
  long thinking pass is not cut; the visible text after it is.
- **The estimate is deliberately high.** At 2 bytes a token it never under-counted
  visible text here (measured 3.19 and 3.16 English, 2.31 Vietnamese, 2.28 Korean),
  but it over-counts English about 1.6x, so a reply near the cap can be cut early.
- **A cut reply is charged an estimate.** Upstream never reports it, and may bill
  for tokens generated before it noticed the hang-up, which is not measured.
- **A harness with its own retry loop may still loop.** Only Claude Code and the
  Python SDK were measured. Anything else that retries every failure keeps hitting a
  402; each one is answered locally and spends nothing. Match on the error type
  `caudao_budget_exhausted`, not on the status.
- **The cap in the message is rounded to cents**, so a $0.004 test cap reads `$0.00`.
- **A 502 is still retried, on purpose.** A reply caudao cannot read is charged and
  answered 502, so a retrying client walks the ledger to the cap and then gets the 402.
- One live cut is one live cut. The numbers above are the test server's.

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
