package caudao

import (
	"os"
	"path/filepath"
	"testing"
)

// A ledger in a directory caudao cannot write used to open fine: every Add
// logged "persist failed" and enforced in memory, and the next restart opened
// the old file (or none) and the day's spend was gone. OpenLedger now writes
// once before serving, so an unwritable ledger stops the start instead.
func TestLedgerRefusesToOpenWhereItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	readOnly := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		return dir
	}
	for name, rel := range map[string]string{
		"file in a read-only directory":         "ledger.json",
		"subdirectory of a read-only directory": filepath.Join("state", "ledger.json"),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(readOnly(t), rel)
			if _, err := OpenLedger(path); err == nil {
				t.Fatalf("OpenLedger(%s) succeeded; a restart would forget every dollar spent", path)
			}
		})
	}

	// Controls: green before and after the fix.
	t.Run("writable directory opens", func(t *testing.T) {
		if _, err := OpenLedger(filepath.Join(t.TempDir(), "ledger.json")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("reopen keeps the spend", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ledger.json")
		l, err := OpenLedger(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := l.Add("m", Usage{InputTokens: 10}, 0.25); err != nil {
			t.Fatal(err)
		}
		l2, err := OpenLedger(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, total := l2.Committed("m"); total != 0.25 {
			t.Fatalf("reopened ledger sees $%.2f, want $0.25", total)
		}
	})
	t.Run("in-memory ledger writes nothing", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if _, err := OpenLedger(""); err != nil {
			t.Fatal(err)
		}
		if entries, _ := os.ReadDir("."); len(entries) != 0 {
			t.Fatalf("in-memory ledger created %d files", len(entries))
		}
	})
}
