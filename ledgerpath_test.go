package caudao

import (
	"os"
	"path/filepath"
	"testing"
)

// A relative ledger_path used to resolve against the working directory, not
// the config file. `caudao init` writes "caudao-ledger.json", so the same
// `caudao serve -config /etc/caudao/caudao.json` started from another
// directory opened a fresh ledger at $0 and the day's budget was whole again.
// The test opens, spends and restarts from different directories and asks
// whether the restart still sees the spend.
func TestLedgerFollowsTheConfigNotTheWorkingDirectory(t *testing.T) {
	const spend = 0.25
	writeConfig := func(t *testing.T, dir, ledgerPath string) string {
		t.Helper()
		body := `{"daily_total_usd": 1, "prices": {"m": {"input_per_mtok": 1, "output_per_mtok": 1}}`
		if ledgerPath != "" {
			body += `, "ledger_path": "` + filepath.ToSlash(ledgerPath) + `"`
		}
		path := filepath.Join(dir, "caudao.json")
		if err := os.WriteFile(path, []byte(body+"}"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// run loads the config from cwd, opens its ledger and returns it.
	run := func(t *testing.T, cwd, cfgPath string) *Ledger {
		t.Helper()
		t.Chdir(cwd)
		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		l, err := OpenLedger(cfg.LedgerPath)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	restartSees := func(t *testing.T, cfgPath string, b, c string) float64 {
		t.Helper()
		l := run(t, b, cfgPath)
		if _, _, err := l.Add("m", Usage{OutputTokens: 1}, spend); err != nil {
			t.Fatal(err)
		}
		_, total := run(t, c, cfgPath).Committed("m")
		return total
	}

	t.Run("relative path, started from two other directories", func(t *testing.T) {
		a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
		cfg := writeConfig(t, a, "caudao-ledger.json")
		if got := restartSees(t, cfg, b, c); got != spend {
			t.Fatalf("restart from another directory sees $%.2f spent, want $%.2f", got, spend)
		}
		if _, err := os.Stat(filepath.Join(a, "caudao-ledger.json")); err != nil {
			t.Fatalf("ledger not next to the config: %v", err)
		}
	})
	t.Run("relative path with a subdirectory", func(t *testing.T) {
		a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
		cfg := writeConfig(t, a, filepath.Join("state", "ledger.json"))
		if got := restartSees(t, cfg, b, c); got != spend {
			t.Fatalf("restart sees $%.2f spent, want $%.2f", got, spend)
		}
		if _, err := os.Stat(filepath.Join(a, "state", "ledger.json")); err != nil {
			t.Fatalf("ledger not under the config's directory: %v", err)
		}
	})
	t.Run("no ledger_path is durable, next to the config", func(t *testing.T) {
		a, b, c := t.TempDir(), t.TempDir(), t.TempDir()
		cfg := writeConfig(t, a, "")
		if got := restartSees(t, cfg, b, c); got != spend {
			t.Fatalf("restart sees $%.2f spent, want $%.2f", got, spend)
		}
		if _, err := os.Stat(filepath.Join(a, "caudao-ledger.json")); err != nil {
			t.Fatalf("default ledger not next to the config: %v", err)
		}
	})

	// Controls: these already worked and must keep working.
	t.Run("control: absolute path is kept as written", func(t *testing.T) {
		a, b, c, d := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
		abs := filepath.Join(d, "spend.json")
		cfg := writeConfig(t, a, abs)
		if got := restartSees(t, cfg, b, c); got != spend {
			t.Fatalf("restart sees $%.2f spent, want $%.2f", got, spend)
		}
		if _, err := os.Stat(abs); err != nil {
			t.Fatalf("absolute ledger path not used: %v", err)
		}
	})
	t.Run("control: serve run in the config's directory", func(t *testing.T) {
		a := t.TempDir()
		writeConfig(t, a, "caudao-ledger.json")
		if got := restartSees(t, "caudao.json", a, a); got != spend {
			t.Fatalf("restart sees $%.2f spent, want $%.2f", got, spend)
		}
		if _, err := os.Stat(filepath.Join(a, "caudao-ledger.json")); err != nil {
			t.Fatalf("ledger moved: %v", err)
		}
	})
}
