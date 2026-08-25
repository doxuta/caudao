package caudao

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config is caudao's whole configuration. Budgets are hard daily ceilings in
// USD; a zero or missing budget means "no requests allowed" for that scope —
// caudao is fail-closed by design, you opt models IN.
type Config struct {
	// Upstream is the real API origin, default https://api.anthropic.com.
	Upstream string `json:"upstream,omitempty"`
	// DailyTotalUSD is the hard ceiling across all models per local day.
	DailyTotalUSD float64 `json:"daily_total_usd"`
	// DailyPerModelUSD are per-model-prefix ceilings (longest prefix wins).
	DailyPerModelUSD map[string]float64 `json:"daily_per_model_usd,omitempty"`
	// Prices per model prefix. A request whose model matches no prefix is
	// refused. Prices change — keep this table yours, not ours.
	Prices PriceTable `json:"prices"`
	// LedgerPath is where daily spend is persisted.
	LedgerPath string `json:"ledger_path,omitempty"`
}

// LoadConfig reads a JSON config file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("caudao: parse %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate enforces the fail-closed invariants.
func (c *Config) Validate() error {
	if c.Upstream == "" {
		c.Upstream = "https://api.anthropic.com"
	}
	if c.DailyTotalUSD <= 0 {
		return fmt.Errorf("caudao: daily_total_usd must be > 0 (fail-closed: no budget, no requests)")
	}
	if len(c.Prices) == 0 {
		return fmt.Errorf("caudao: prices table is empty (fail-closed: unpriced models are refused)")
	}
	return nil
}

// ModelBudget returns the daily ceiling for a model: the per-model ceiling if
// one matches (longest prefix), else the daily total.
func (c *Config) ModelBudget(model string) float64 {
	best := ""
	for prefix := range c.DailyPerModelUSD {
		if len(prefix) > len(best) && hasPrefix(model, prefix) {
			best = prefix
		}
	}
	if best != "" {
		return c.DailyPerModelUSD[best]
	}
	return c.DailyTotalUSD
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}

// ExampleConfig is written by `caudao init` as a starting point.
const ExampleConfig = `{
  "upstream": "https://api.anthropic.com",
  "daily_total_usd": 25.0,
  "daily_per_model_usd": {
    "claude-opus": 15.0,
    "claude-sonnet": 10.0,
    "claude-haiku": 2.0
  },
  "prices": {
    "claude-opus": { "input_per_mtok": 15.0, "output_per_mtok": 75.0 },
    "claude-sonnet": { "input_per_mtok": 3.0, "output_per_mtok": 15.0 },
    "claude-haiku": { "input_per_mtok": 0.8, "output_per_mtok": 4.0 }
  },
  "ledger_path": "caudao-ledger.json"
}
`
