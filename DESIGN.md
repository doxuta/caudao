# Design notes

## The one decision that matters: fail-closed

Every choice follows from one rule: **when caudao cannot price something, it
refuses it.** No price for the model → 400. No budget left → 402. A POST to
an endpoint the meter doesn't understand → 403. The alternative — "pass it
through and hope" — is how budget tools quietly stop being budget tools when
a provider ships a new endpoint or model.

The budget refusal is 402 and not 429 because the breaker does not reopen until
midnight: clients retry a 429, and Claude Code also tells its user the limit is
not theirs. See the Proof block in the README.

The cost of this rule is maintenance honesty: the price table lives in the
user's config, not the binary, because prices change and a stale hardcoded
table is worse than none.

## Mid-stream metering

The interesting systems work is `meterReader` (sse.go): it wraps the upstream
response body, passes every byte through unmodified, and parses SSE lines as
they flow. Design constraints:

- **No response buffering.** A data field that is valid JSON on its own is
  metered and forwarded as its line is read, so the common case holds one SSE
  line and time-to-first-token overhead is a line scan (~115 MB/s throughput
  measured; real LLM streams are KB/s). A payload that does not parse alone
  may be one field of a multi-line event — SSE joins an event's data fields
  with `\n` and dispatches at the blank line — so those lines are held to the
  boundary and the joined payload is parsed there. That path holds one event,
  capped at 1 MiB, past which the block is released unmetered. Holding rather
  than forwarding is deliberate: an event that trips the breaker must still be
  droppable when its last line arrives.
- **Incremental charging.** `message_start` charges input + cache tokens
  once; `message_delta` usage is cumulative, and a server tool grows the input
  after `message_start`, so each field it reports is charged by how much it
  grew (a field left out never refunds). The web search count in
  `server_tool_use` is one of those fields, priced per search, not per token. The price of a delta can depend on what came before it: with a `long_prompt` tier the request is priced on its whole prompt so far, and a delta that carries it over the threshold re-prices what was already charged (`Price.Charge`). Output is reported by `message_delta` as a cumulative `output_tokens`,
  but a real reply sends one, after its last byte, so metering only that cannot
  cut anything. Each content delta therefore adds its bytes to an estimate (2
  bytes a token), held as a reservation so it costs no disk write and can be
  replaced: reported usage releases it and charges the true difference. A stream
  that ends with none reported, cut by the breaker or by the client, turns the
  estimate into spend, because upstream billed those tokens and a free cut would
  refill the budget for the next retry. The mock used to send a `message_delta`
  per delta, which is how this stayed hidden.
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
