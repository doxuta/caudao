package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run the real binary, because the paths under test end in
// os.Exit: the exit code and what was printed before it are the behaviour.
// Note: `go test -coverprofile` does not count subprocess runs, so the
// package's reported coverage does not move with these tests.

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "caudao-cli")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin = filepath.Join(dir, "caudao")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// run executes the binary in dir and returns stdout, stderr and the exit code,
// read from the process itself (never through a pipe).
func run(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		switch {
		case err == nil:
			return stdout.String(), stderr.String(), 0
		case errors.As(err, &ee):
			return stdout.String(), stderr.String(), ee.ExitCode()
		default:
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("caudao %v still running after 10s; stdout=%q stderr=%q", args, stdout.String(), stderr.String())
	}
	return "", "", -1
}

// `caudao status` pointed at something that is not caudao (the upstream API, a
// typo'd port behind another server) printed that server's error body and
// exited 0, so a script checking the exit code read it as a healthy breaker.
// caudao's own /caudao/status always answers 200.
func TestStatusFailsWhenTheURLIsNotCaudao(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusBadGateway} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"type":"error","error":{"type":"not_found_error"}}`, code)
			}))
			defer srv.Close()
			_, stderr, exit := run(t, t.TempDir(), "status", "-url", srv.URL)
			if exit == 0 {
				t.Fatalf("status against a server answering %d exited 0", code)
			}
			if !strings.Contains(stderr, fmt.Sprint(code)) {
				t.Fatalf("stderr does not name the status %d: %q", code, stderr)
			}
		})
	}

	t.Run("control: caudao's own status exits 0", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/caudao/status" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"total_usd":0.25}`)
		}))
		defer srv.Close()
		stdout, stderr, exit := run(t, t.TempDir(), "status", "-url", srv.URL)
		if exit != 0 || !strings.Contains(stdout, `"total_usd":0.25`) {
			t.Fatalf("exit %d, stdout %q, stderr %q", exit, stdout, stderr)
		}
	})
}

// `caudao serve` announced "metering ... point your agent at it" before it
// tried to listen, so a busy port printed the success lines and then died: an
// operator (or an agent reading the output) saw the address to use first.
func TestServeAnnouncesOnlyOnceListening(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"upstream":"http://127.0.0.1:1","daily_total_usd":5,"ledger_path":"ledger.json","prices":{"m":{"input_per_mtok":1,"output_per_mtok":1}}}`)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	stdout, stderr, exit := run(t, dir, "serve", "-config", "caudao.json", "-listen", busy.Addr().String())
	if exit != 1 {
		t.Fatalf("serve on a busy port exited %d, want 1; stderr %q", exit, stderr)
	}
	if strings.Contains(stdout, "metering") || strings.Contains(stdout, "ANTHROPIC_BASE_URL") {
		t.Fatalf("serve announced itself on a port it never got:\n%s", stdout)
	}
}

// Controls: startup failures that already exited 1 before announcing. They
// pin that behaviour at the process level (11-21 only covered OpenLedger).
func TestServeRefusesBadStartup(t *testing.T) {
	t.Run("missing config", func(t *testing.T) {
		stdout, stderr, exit := run(t, t.TempDir(), "serve", "-config", "nope.json", "-listen", "127.0.0.1:0")
		if exit != 1 || strings.Contains(stdout, "metering") || !strings.Contains(stderr, "nope.json") {
			t.Fatalf("exit %d, stdout %q, stderr %q", exit, stdout, stderr)
		}
	})
	t.Run("unwritable ledger directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root writes into a read-only directory")
		}
		dir := t.TempDir()
		ro := filepath.Join(dir, "ro")
		if err := os.Mkdir(ro, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(ro, 0o755) })
		writeConfig(t, dir, `{"upstream":"http://127.0.0.1:1","daily_total_usd":5,"ledger_path":"ro/ledger.json","prices":{"m":{"input_per_mtok":1,"output_per_mtok":1}}}`)
		stdout, stderr, exit := run(t, dir, "serve", "-config", "caudao.json", "-listen", "127.0.0.1:0")
		if exit != 1 || strings.Contains(stdout, "metering") || !strings.Contains(stderr, "not writable") {
			t.Fatalf("exit %d, stdout %q, stderr %q", exit, stdout, stderr)
		}
	})
}

func TestInitRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	if _, stderr, exit := run(t, dir, "init"); exit != 0 {
		t.Fatalf("first init exited %d: %s", exit, stderr)
	}
	writeConfig(t, dir, `{"edited":true}`)
	_, stderr, exit := run(t, dir, "init")
	if exit != 1 || !strings.Contains(stderr, "already exists") {
		t.Fatalf("second init: exit %d, stderr %q", exit, stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "caudao.json")); string(b) != `{"edited":true}` {
		t.Fatalf("init overwrote an edited caudao.json: %s", b)
	}
}

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "caudao.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
