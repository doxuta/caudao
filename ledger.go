package caudao

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Ledger is the persistent daily spend record. All methods are safe for
// concurrent use; every mutation is flushed to disk with an atomic rename so
// a crash cannot lose more than the in-flight update.
type Ledger struct {
	mu   sync.Mutex
	path string
	now  func() time.Time // injectable for tests
	data ledgerFile

	// In-flight reservations. Without them the budget check is a
	// time-of-check/time-of-use race: N concurrent requests all read the same
	// committed total, all pass, and all spend. Reserving a pessimistic
	// estimate up front bounds the overshoot to what is actually in flight.
	reserved      map[string]float64
	reservedTotal float64
}

type ledgerFile struct {
	// Day is the local date the totals belong to, e.g. "2026-08-26".
	Day    string                `json:"day"`
	Models map[string]ModelSpend `json:"models"`
	Total  float64               `json:"total_usd"`
}

// ModelSpend is the accumulated spend for one model on the current day.
type ModelSpend struct {
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	USD          float64 `json:"usd"`
}

// OpenLedger loads (or initializes) the ledger at path.
func OpenLedger(path string) (*Ledger, error) {
	l := &Ledger{path: path, now: time.Now}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &l.data); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if l.data.Models == nil {
		l.data.Models = map[string]ModelSpend{}
	}
	l.reserved = map[string]float64{}
	return l, nil
}

func (l *Ledger) day() string { return l.now().Format("2006-01-02") }

// rollover resets the totals when the local date moves FORWARD. Comparing
// with != would let a backwards clock step — an NTP correction, a timezone
// change, a VM restored from a snapshot — wipe the day's spend and refill the
// budget, which is the one direction a budget must never move on its own.
func (l *Ledger) rollover() {
	if d := l.day(); d > l.data.Day {
		l.data = ledgerFile{Day: d, Models: map[string]ModelSpend{}}
		l.reserved = map[string]float64{}
		l.reservedTotal = 0
	}
}

// Add records usage and cost for model and persists the ledger. It returns
// the model's new daily total and the overall daily total.
func (l *Ledger) Add(model string, u Usage, usd float64) (modelUSD, totalUSD float64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover()
	s := l.data.Models[model]
	s.InputTokens += u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens
	s.OutputTokens += u.OutputTokens
	s.USD += usd
	l.data.Models[model] = s
	l.data.Total += usd
	return s.USD, l.data.Total, l.persist()
}

// Spent returns today's spend for one model and overall, INCLUDING money
// reserved by requests still in flight. Enforcement must see committed plus
// reserved, or concurrent requests each see a stale total.
func (l *Ledger) Spent(model string) (modelUSD, totalUSD float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover()
	return l.data.Models[model].USD + l.reserved[model], l.data.Total + l.reservedTotal
}

// Committed returns today's settled spend, excluding reservations. This is
// what the status endpoint and the ledger file report.
func (l *Ledger) Committed(model string) (modelUSD, totalUSD float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover()
	return l.data.Models[model].USD, l.data.Total
}

// Reserve holds estUSD against model's budget for a request about to be
// forwarded, so a concurrent request cannot spend the same headroom. The
// returned release must be called exactly once when the response finishes;
// after that only the amounts passed to Add count.
func (l *Ledger) Reserve(model string, estUSD float64) (release func()) {
	l.mu.Lock()
	l.rollover()
	l.reserved[model] += estUSD
	l.reservedTotal += estUSD
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.reserved[model] -= estUSD; l.reserved[model] <= 0 {
				delete(l.reserved, model)
			}
			if l.reservedTotal -= estUSD; l.reservedTotal < 0 {
				l.reservedTotal = 0
			}
		})
	}
}

// Snapshot returns a copy of today's ledger for status endpoints.
func (l *Ledger) Snapshot() (day string, models map[string]ModelSpend, total float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover()
	cp := make(map[string]ModelSpend, len(l.data.Models))
	for k, v := range l.data.Models {
		cp[k] = v
	}
	return l.data.Day, cp, l.data.Total
}

// persist writes the ledger atomically. Caller holds mu.
func (l *Ledger) persist() error {
	if l.path == "" {
		return nil // in-memory ledger (tests, demo)
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(l.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}
