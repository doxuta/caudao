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
	// WebSearchPer1K is the USD fee per 1,000 web searches, billed on top of
	// tokens. Zero falls back to the documented $10.
	WebSearchPer1K float64 `json:"web_search_per_1k,omitempty"`
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
	// ServerToolUse counts server-tool calls. Only web search carries a fee;
	// web fetch is free and code execution is billed by container time, which
	// the usage object does not report.
	ServerToolUse ServerToolUse `json:"server_tool_use"`
}

// ServerToolUse is usage.server_tool_use. A plain struct keeps Usage comparable.
type ServerToolUse struct {
	WebSearchRequests int `json:"web_search_requests"`
}

// Cost returns the USD cost of u at price p.
func (p Price) Cost(u Usage) float64 {
	in := p.InputPerMTok / 1e6
	out := p.OutputPerMTok / 1e6
	return float64(u.InputTokens)*in +
		float64(u.OutputTokens)*out +
		float64(u.CacheCreationTokens)*in*p.cacheWrite() +
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
