package caudao

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
)

type ctxKey int

const metaKey ctxKey = 0

type reqMeta struct {
	model   string
	release func() // frees this request's budget reservation
}

// Proxy is the circuit-breaker reverse proxy. Point ANTHROPIC_BASE_URL at it.
//
// Metered surface: POST /v1/messages (streaming and non-streaming).
// Free surface: GETs and POST /v1/messages/count_tokens pass through.
// Everything else is refused — fail-closed — so a new billable endpoint can
// never sneak past the meter.
type Proxy struct {
	cfg    *Config
	ledger *Ledger
	rp     *httputil.ReverseProxy
}

// NewProxy builds the proxy from a validated config and an open ledger.
func NewProxy(cfg *Config, ledger *Ledger) (*Proxy, error) {
	target, err := parseUpstream(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	p := &Proxy{cfg: cfg, ledger: ledger}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// SetURL, not a hand-rolled Scheme/Host copy: it joins the
			// upstream's path prefix, so a gateway at
			// https://gw.corp/anthropic is not silently truncated.
			pr.SetURL(target)
			// The meter parses the response body as text. A gzip/br/zstd
			// body would sail past it unmetered, so refuse to let upstream
			// compress: ask for identity and drop any client preference.
			pr.Out.Header.Set("Accept-Encoding", "identity")
			// Auth headers pass through untouched; caudao never reads,
			// stores, or logs them.
		},
		FlushInterval:  -1, // flush every write: SSE stays real-time
		ModifyResponse: p.modifyResponse,
		ErrorLog:       log.New(io.Discard, "", 0),
	}
	return p, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/caudao/status":
		p.status(w)
	case r.Method == http.MethodGet:
		p.rp.ServeHTTP(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens":
		p.rp.ServeHTTP(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		p.meteredMessages(w, r)
	default:
		refuse(w, http.StatusForbidden, "caudao_unmetered_endpoint",
			fmt.Sprintf("caudao refuses %s %s: not a metered endpoint (fail-closed)", r.Method, r.URL.Path))
	}
}

func (p *Proxy) meteredMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		refuse(w, http.StatusBadRequest, "caudao_bad_request", "caudao: could not read request body")
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Model == "" {
		refuse(w, http.StatusBadRequest, "caudao_bad_request", "caudao: request has no model field")
		return
	}
	if _, ok := p.cfg.Prices.Lookup(req.Model); !ok {
		refuse(w, http.StatusBadRequest, "caudao_unpriced_model",
			fmt.Sprintf("caudao: model %q has no configured price (fail-closed); add it to the prices table", req.Model))
		return
	}
	if trip, reason := p.overBudget(req.Model); trip {
		refuse(w, http.StatusTooManyRequests, "caudao_budget_exhausted", "caudao: "+reason+" — request refused")
		return
	}

	// Hold a pessimistic estimate against the budget for as long as this
	// request is in flight, so concurrent requests cannot all spend the same
	// headroom. Settled spend replaces it when the response completes.
	release := p.ledger.Reserve(req.Model, p.reservation(req.Model))
	meta := &reqMeta{model: req.Model, release: release}
	defer meta.free()

	r = r.WithContext(context.WithValue(r.Context(), metaKey, meta))
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	p.rp.ServeHTTP(w, r)
}

// free releases the reservation once; safe to call from several paths.
func (m *reqMeta) free() {
	if m != nil && m.release != nil {
		m.release()
	}
}

// reservation is what one in-flight request holds against the budget. It is
// deliberately pessimistic — the cost of a fairly long reply — because the
// overshoot a race can produce is bounded by how much is NOT reserved.
func (p *Proxy) reservation(model string) float64 {
	price, ok := p.cfg.Prices.Lookup(model)
	if !ok {
		return 0
	}
	return price.Cost(reservationUsage)
}

// reservationUsage is the shape of reply a reservation assumes. It only has
// to cover the window between forwarding a request and its first usage event,
// so it is a modest placeholder rather than a worst case: too large a value
// would refuse legitimate concurrent traffic on a small budget.
var reservationUsage = Usage{InputTokens: 2_000, OutputTokens: 500}

// overBudget reports whether the model or the day is out of budget.
func (p *Proxy) overBudget(model string) (bool, string) {
	modelSpent, totalSpent := p.ledger.Spent(model)
	if budget := p.cfg.ModelBudget(model); modelSpent >= budget {
		return true, fmt.Sprintf("daily budget for %s exhausted ($%.4f of $%.2f)", model, modelSpent, budget)
	}
	if totalSpent >= p.cfg.DailyTotalUSD {
		return true, fmt.Sprintf("daily total budget exhausted ($%.4f of $%.2f)", totalSpent, p.cfg.DailyTotalUSD)
	}
	return false, ""
}

// account records usage and answers whether the breaker should trip now.
func (p *Proxy) account(model string, u Usage) (bool, string) {
	price, ok := p.cfg.Prices.Lookup(model)
	if !ok {
		return true, fmt.Sprintf("model %q lost its price mid-flight", model)
	}
	if _, _, err := p.ledger.Add(model, u, price.Cost(u)); err != nil {
		log.Printf("caudao: ledger persist failed (still enforcing in memory): %v", err)
	}
	return p.overBudget(model)
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	meta, _ := resp.Request.Context().Value(metaKey).(*reqMeta)
	if meta == nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	// Fail closed: if the body is encoded in a form the meter cannot parse,
	// refuse it rather than forwarding an unmetered response. Upstream was
	// asked for identity, so a content-encoding here is a surprise.
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return unmeterable(resp, fmt.Sprintf("upstream replied with Content-Encoding %q, which caudao cannot meter", enc))
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		resp.Body = newMeterReader(resp.Body, func(u Usage) (bool, string) {
			// Real usage supersedes the estimate: release the reservation
			// before accounting, so a request is never tripped by its own
			// placeholder — only by committed spend and other requests'
			// reservations.
			meta.free()
			return p.account(meta.model, u)
		}, meta.free)
		resp.Header.Del("Content-Length")
		return nil
	}
	if !strings.HasPrefix(ct, "application/json") {
		return unmeterable(resp, fmt.Sprintf("upstream replied with Content-Type %q on a metered endpoint, which caudao cannot meter", ct))
	}
	// Non-streaming: responses are small; read, account, pass through.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	var msg struct {
		Usage Usage `json:"usage"`
	}
	meta.free() // real (or estimated) spend replaces the reservation
	if err := json.Unmarshal(body, &msg); err != nil || msg.Usage == (Usage{}) {
		// A billable reply whose usage caudao could not read is a free pass.
		// Charge a pessimistic estimate so the breaker still moves, and say so.
		log.Printf("caudao: could not read usage from a %d-byte %s reply for %s (%v) — charging the unmetered estimate",
			len(body), ct, meta.model, err)
		p.account(meta.model, unmeteredEstimate)
	} else {
		p.account(meta.model, msg.Usage) // post-hoc: blocks the NEXT request
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return nil
}

func (p *Proxy) status(w http.ResponseWriter) {
	day, models, total := p.ledger.Snapshot()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"day":             day,
		"models":          models,
		"total_usd":       total,
		"daily_total_usd": p.cfg.DailyTotalUSD,
	})
}

// unmeterableEstimate is what caudao charges when a billable response
// arrives in a form it cannot read. It is deliberately pessimistic: a wrong
// high charge closes the breaker early, a wrong zero charge disables it.
var unmeteredEstimate = Usage{InputTokens: 50_000, OutputTokens: 10_000}

// unmeterable replaces an unreadable body with an Anthropic-shaped error.
// The upstream response is consumed and discarded: fail closed.
func unmeterable(resp *http.Response, reason string) error {
	resp.Body.Close()
	payload, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    "caudao_unmeterable_response",
			"message": "caudao: " + reason + " (fail-closed)",
		},
	})
	resp.StatusCode = http.StatusBadGateway
	resp.Status = "502 Bad Gateway"
	resp.Header = resp.Header.Clone()
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Del("Content-Encoding")
	resp.Header.Set("Content-Length", fmt.Sprint(len(payload)))
	resp.Body = io.NopCloser(bytes.NewReader(payload))
	resp.ContentLength = int64(len(payload))
	return nil
}

// refuse writes an Anthropic-shaped error so SDKs surface it cleanly.
func refuse(w http.ResponseWriter, code int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": msg,
		},
	})
}
