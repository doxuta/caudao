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
	return l, nil
}

func (l *Ledger) day() string { return l.now().Format("2006-01-02") }

// rollover resets the totals when the local date changes. Caller holds mu.
func (l *Ledger) rollover() {
	if d := l.day(); l.data.Day != d {
		l.data = ledgerFile{Day: d, Models: map[string]ModelSpend{}}
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

// Spent returns today's spend for one model and overall.
func (l *Ledger) Spent(model string) (modelUSD, totalUSD float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollover()
	return l.data.Models[model].USD, l.data.Total
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
