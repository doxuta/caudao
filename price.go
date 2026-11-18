// Package caudao is a fail-closed spending circuit breaker for autonomous AI
// agents: a reverse proxy in front of the Anthropic API that meters token
// spend live from the response stream and cuts the connection the moment a
// budget is exhausted — named after the household circuit breaker.
//
// Fail-closed means: when caudao cannot price a request (unknown model, no
// budget configured), it refuses it rather than letting it through unmetered.
package caudao

import (
	"fmt"
	"sort"
	"strings"
)

// Price is the USD cost per million tokens for one model family.
type Price struct {
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
	// Cache multipliers relative to input price. Zero values fall back to the
	// conventional 1.25× for cache writes and 0.1× for cache reads.
	CacheWriteMult float64 `json:"cache_write_mult,omitempty"`
	CacheReadMult  float64 `json:"cache_read_mult,omitempty"`
	// CacheWrite1hMult prices 1-hour cache writes, which the reply counts in
	// usage.cache_creation.ephemeral_1h_input_tokens. Zero falls back to the
	// documented 2×.
	CacheWrite1hMult float64 `json:"cache_write_1h_mult,omitempty"`
	// WebSearchPer1K is the USD fee per 1,000 web searches, billed on top of
	// tokens. Zero falls back to the documented $10.
	WebSearchPer1K float64 `json:"web_search_per_1k,omitempty"`
	// LongPrompt, when set, is the price of a request whose prompt (input,
	// cache writes and cache reads) is over LongPromptOverTokens: Claude Haiku
	// 5.5 bills every token of such a request at 5x. Its zero cache
	// multipliers and web search fee fall back to this price's.
	LongPromptOverTokens int    `json:"long_prompt_over_tokens,omitempty"`
	LongPrompt           *Price `json:"long_prompt,omitempty"`
	// Fast, when set, is the price of a request sent with "speed": "fast"
	// (Claude Opus 5.5: $8 / $40 instead of $4 / $20). It covers the whole
	// context window, so the base LongPrompt does not apply to it; its zero
	// cache multipliers and web search fee fall back to this price's. A fast
	// request on a price with no Fast is refused.
	Fast *Price `json:"fast,omitempty"`
}

// ForSpeed returns the price of a request sent with the given speed, and false
// when speed is "fast" and p has no fast tier.
func (p Price) ForSpeed(speed string) (Price, bool) {
	if speed != "fast" {
		return p, true
	}
	if p.Fast == nil {
		return Price{}, false
	}
	f := *p.Fast
	if f.CacheWriteMult == 0 {
		f.CacheWriteMult = p.CacheWriteMult
	}
	if f.CacheReadMult == 0 {
		f.CacheReadMult = p.CacheReadMult
	}
	if f.CacheWrite1hMult == 0 {
		f.CacheWrite1hMult = p.CacheWrite1hMult
	}
	if f.WebSearchPer1K == 0 {
		f.WebSearchPer1K = p.WebSearchPer1K
	}
	return f, true
}

// usInferenceMult is what "inference_geo": "us" costs: 1.1x on every token
// category (input, output, cache writes and reads), long-prompt tier included,
// on top of fast mode. The web search fee is not a token price.
const usInferenceMult = 1.1

// ForGeo returns the price of a request sent with the given inference_geo, and
// false for a geo caudao has no multiplier for. Apply it after ForSpeed.
func (p Price) ForGeo(geo string) (Price, bool) {
	switch geo {
	case "", "global":
		return p, true
	case "us":
	default:
		return Price{}, false
	}
	p.InputPerMTok *= usInferenceMult
	p.OutputPerMTok *= usInferenceMult
	if p.LongPrompt != nil {
		l := *p.LongPrompt
		l.InputPerMTok *= usInferenceMult
		l.OutputPerMTok *= usInferenceMult
		p.LongPrompt = &l
	}
	return p, true
}

// at returns the price that applies to a request whose usage so far is u.
func (p Price) at(u Usage) Price {
	if p.LongPrompt == nil || u.InputTokens+u.CacheCreationTokens+u.CacheReadTokens <= p.LongPromptOverTokens {
		return p
	}
	l := *p.LongPrompt
	if l.CacheWriteMult == 0 {
		l.CacheWriteMult = p.CacheWriteMult
	}
	if l.CacheReadMult == 0 {
		l.CacheReadMult = p.CacheReadMult
	}
	if l.CacheWrite1hMult == 0 {
		l.CacheWrite1hMult = p.CacheWrite1hMult
	}
	if l.WebSearchPer1K == 0 {
		l.WebSearchPer1K = p.WebSearchPer1K
	}
	return l
}

func (p Price) webSearch() float64 {
	if p.WebSearchPer1K > 0 {
		return p.WebSearchPer1K / 1000
	}
	return 10.0 / 1000
}

func (p Price) cacheWrite() float64 {
	if p.CacheWriteMult > 0 {
		return p.CacheWriteMult
	}
	return 1.25
}

func (p Price) cacheWrite1h() float64 {
	if p.CacheWrite1hMult > 0 {
		return p.CacheWrite1hMult
	}
	return 2
}

func (p Price) cacheRead() float64 {
	if p.CacheReadMult > 0 {
		return p.CacheReadMult
	}
	return 0.1
}

// Usage is the token usage extracted from an API response or stream event.
type Usage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	// CacheCreation splits CacheCreationTokens by TTL; only the 1-hour part
	// is priced differently.
	CacheCreation CacheCreation `json:"cache_creation"`
	// ServerToolUse counts server-tool calls. Only web search carries a fee;
	// web fetch is free and code execution is billed by container time, which
	// the usage object does not report.
	ServerToolUse ServerToolUse `json:"server_tool_use"`
}

func (u Usage) plus(v Usage) Usage {
	return Usage{
		InputTokens:         u.InputTokens + v.InputTokens,
		OutputTokens:        u.OutputTokens + v.OutputTokens,
		CacheCreationTokens: u.CacheCreationTokens + v.CacheCreationTokens,
		CacheReadTokens:     u.CacheReadTokens + v.CacheReadTokens,
		CacheCreation: CacheCreation{
			OneHourTokens: u.CacheCreation.OneHourTokens + v.CacheCreation.OneHourTokens,
		},
		ServerToolUse: ServerToolUse{
			WebSearchRequests: u.ServerToolUse.WebSearchRequests + v.ServerToolUse.WebSearchRequests,
		},
	}
}

// CacheCreation is usage.cache_creation. A plain struct keeps Usage comparable.
type CacheCreation struct {
	OneHourTokens int `json:"ephemeral_1h_input_tokens"`
}

// ServerToolUse is usage.server_tool_use. A plain struct keeps Usage comparable.
type ServerToolUse struct {
	WebSearchRequests int `json:"web_search_requests"`
}

// Cost returns the USD cost of one request whose whole usage is u.
func (p Price) Cost(u Usage) float64 {
	return p.at(u).rate(u)
}

// Charge returns what delta adds to a request that has already been charged
// for before. A delta that carries the prompt over the long-prompt threshold
// also re-prices what was charged before at the lower rate.
func (p Price) Charge(before, delta Usage) float64 {
	after := before.plus(delta)
	if p.at(after) != p.at(before) {
		return p.Cost(after) - p.Cost(before)
	}
	return p.at(after).rate(delta)
}

// rate prices u at p's own rates, ignoring LongPrompt.
func (p Price) rate(u Usage) float64 {
	in := p.InputPerMTok / 1e6
	out := p.OutputPerMTok / 1e6
	return float64(u.InputTokens)*in +
		float64(u.OutputTokens)*out +
		float64(u.CacheCreationTokens)*in*p.cacheWrite() +
		// 1-hour writes are inside CacheCreationTokens, so they add only the
		// difference: correct whichever event reports the split.
		float64(u.CacheCreation.OneHourTokens)*in*max(p.cacheWrite1h()-p.cacheWrite(), 0) +
		float64(u.CacheReadTokens)*in*p.cacheRead() +
		float64(u.ServerToolUse.WebSearchRequests)*p.webSearch()
}

// PriceTable maps a model-name prefix to its price. Longest prefix wins, so
// "claude-opus-5" can override a broader "claude-" entry.
type PriceTable map[string]Price

// Lookup resolves the price for a model by longest matching prefix.
func (t PriceTable) Lookup(model string) (Price, bool) {
	best := ""
	for prefix := range t {
		if strings.HasPrefix(model, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return Price{}, false
	}
	return t[best], true
}

// Prefixes returns the configured prefixes, longest first (for display).
func (t PriceTable) Prefixes() []string {
	out := make([]string, 0, len(t))
	for p := range t {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// ErrUnknownModel is wrapped into refusals for unpriced models.
var ErrUnknownModel = fmt.Errorf("caudao: model has no configured price (fail-closed)")
