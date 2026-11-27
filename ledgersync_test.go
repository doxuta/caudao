package caudao

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// diskTotal reads the ledger file the way a restart would.
func diskTotal(t *testing.T, path string) float64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f ledgerFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Total
}

// persist wrote the temp file and renamed it without an fsync. A process crash
// was fine, but after an OS crash or power loss the rename could reach the disk
// before the data, leaving a zero-length ledger that makes serve refuse to start.
// No unit test can cut the power, so this pins the call order that makes the
// write durable: fsync the temp file, rename, fsync the directory. It proves the
// order, not what a given disk does with it.
func TestLedgerSyncsBeforeRenameAndDirAfter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}

	var calls []string
	orig := fsync
	t.Cleanup(func() { fsync = orig })
	fsync = func(f *os.File) error {
		renamed := diskTotal(t, path) == 0.5 // this Add is the first spend
		switch f.Name() {
		case path + ".tmp":
			if renamed {
				t.Error("temp file synced after the rename")
			}
			calls = append(calls, "file")
		case filepath.Dir(path):
			if !renamed {
				t.Error("directory synced before the rename")
			}
			calls = append(calls, "dir")
		default:
			t.Errorf("fsync of unexpected %s", f.Name())
		}
		return orig(f)
	}

	if _, _, err := l.Add("m", Usage{InputTokens: 1}, 0.5); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ","); got != "file,dir" {
		t.Fatalf("fsync calls = %q, want \"file,dir\"", got)
	}
}

// A failed fsync means the temp file's bytes may never reach the disk, so the
// rename must not replace the last good ledger with it.
func TestLedgerKeepsOldFileWhenSyncFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Add("m", Usage{InputTokens: 1}, 0.25); err != nil {
		t.Fatal(err)
	}

	orig := fsync
	t.Cleanup(func() { fsync = orig })
	fsync = func(f *os.File) error {
		if f.Name() == path+".tmp" {
			return errors.New("EIO")
		}
		return orig(f)
	}
	if _, _, err := l.Add("m", Usage{InputTokens: 1}, 0.5); err == nil {
		t.Fatal("Add reported success although the ledger was not synced")
	}
	if got := diskTotal(t, path); got != 0.25 {
		t.Fatalf("ledger on disk holds $%.2f, want the last synced $0.25", got)
	}
}

// One Add is one fsync pair under the ledger mutex; this measures its price.
func BenchmarkLedgerAdd(b *testing.B) {
	l, err := OpenLedger(filepath.Join(b.TempDir(), "ledger.json"))
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, _, err := l.Add("m", Usage{InputTokens: 1}, 0.001); err != nil {
			b.Fatal(err)
		}
	}
}
