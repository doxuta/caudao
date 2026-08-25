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
	"net/url"
	"strings"
)

type ctxKey int

const metaKey ctxKey = 0

type reqMeta struct {
	model string
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
	target, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("caudao: bad upstream: %w", err)
	}
	p := &Proxy{cfg: cfg, ledger: ledger}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.Host = target.Host
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

	r = r.WithContext(context.WithValue(r.Context(), metaKey, &reqMeta{model: req.Model}))
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	p.rp.ServeHTTP(w, r)
}

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
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body = newMeterReader(resp.Body, func(u Usage) (bool, string) {
			return p.account(meta.model, u)
		})
		resp.Header.Del("Content-Length")
		return nil
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
	if json.Unmarshal(body, &msg) == nil && msg.Usage != (Usage{}) {
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
