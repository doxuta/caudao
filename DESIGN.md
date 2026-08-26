# Design notes

## The one decision that matters: fail-closed

Every choice follows from one rule: **when caudao cannot price something, it
refuses it.** No price for the model → 400. No budget left → 429. A POST to
an endpoint the meter doesn't understand → 403. The alternative — "pass it
through and hope" — is how budget tools quietly stop being budget tools when
a provider ships a new endpoint or model.

The cost of this rule is maintenance honesty: the price table lives in the
user's config, not the binary, because prices change and a stale hardcoded
table is worse than none.

## Mid-stream metering

The interesting systems work is `meterReader` (sse.go): it wraps the upstream
response body, passes every byte through unmodified, and parses SSE lines as
they flow. Design constraints:

- **No response buffering.** It holds at most one SSE line, so time-to-first-
  token overhead is a line scan (~150 MB/s throughput measured; real LLM
  streams are KB/s).
- **Incremental charging.** `message_start` charges input + cache tokens
  once; each `message_delta` carries a cumulative `output_tokens`, so the
  charge is the delta against the last seen value. Partial responses are
  charged for exactly what streamed before the cut — including the request
  that trips the breaker (spend is recorded first, then the circuit opens).
- **A clean cut.** On trip, the reader stops forwarding, emits one synthetic
  `event: error` with type `caudao_budget_exhausted` (Anthropic-shaped, so
  SDKs surface it as a normal API error), then EOF. Closing the wrapped body
  releases the upstream connection, which stops upstream token generation.

Non-streaming responses are small; they are read fully, accounted, and passed
through — post-hoc accounting there means the *next* request is blocked.

## Reservations

The budget check and the spend it authorises are not atomic: without help, N
concurrent requests all read the same committed total, all pass, and all
spend. Measured overshoot before the fix was 48x on a $0.10 cap with 24
streams.

Each forwarded request therefore holds a modest reservation, released the
moment its first real usage event lands — the reservation only has to cover
the window between forwarding and first usage, after which committed spend
does the work. A request is never tripped by its own reservation, only by
committed spend and by what other requests are holding.

## Ledger

A mutex-guarded in-memory day record persisted with write-to-temp + atomic
rename on every update. A crash loses at most the in-flight update; a restart
cannot forget that the day's budget is spent. SQLite would be more general
and was rejected: one process, one small JSON object, zero dependencies.

Day rollover uses local time deliberately — "my agent's daily budget" means
the operator's day, not UTC's.

## What was cut (scope discipline)

Multi-provider routing, auth management, per-session budgets, a web UI,
Prometheus metrics. Each is a good feature; all of them together are how a
circuit breaker becomes a platform. `/caudao/status` returns JSON; pipe it
into whatever you already run.

## Provenance & AI disclosure

caudao is a clean-room public rebuild of the budget-cap layer of my private
self-hosted LLM gateway (the system that routes my coding agents' traffic in
production, with allowlists and deploy locks). It was built with an AI coding
agent under my direction and review; the test suite — byte-for-byte SSE
passthrough, mid-stream trip, race-clean concurrent ledger — is how I
verified the agent's output rather than trusting it.
