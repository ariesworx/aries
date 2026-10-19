package sextant

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/clienttest"
	"golang.org/x/crypto/ssh"
)

func TestMain(m *testing.M) {
	// Sextant dials with the client: no test here reads the real ~/.ssh
	// or ssh-agent.
	restore := clienttest.Isolate()
	code := m.Run()
	restore()
	os.Exit(code)
}

// syncBuffer is a log read while Run still writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// Run serves /metrics, /healthz and /readyz on its listener, polls at
// once, says so on /readyz, counts a server it cannot reach at connect,
// and returns nil once its context ends.
func TestRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeKey(t, filepath.Join(dir, "id_ed25519"))
	cfg, err := Parse([]byte("server:\n  url: https://"+clienttest.DeadPort(t)+"\nkey: id_ed25519\nprojects: ["+projectA+"]\nlimits:\n  request_timeout: 2s\n"), dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	log := &syncBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, cfg, RunEnv{Version: "v1", Machine: "box", Listener: l,
			Logger: slog.New(slog.NewTextHandler(log, nil))})
	}()
	base := "http://" + l.Addr().String()
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, body := fetch(t, base+"/readyz")
		if code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz = %d %q after 20s", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, body := fetch(t, base+"/metrics"); !strings.Contains(body, `sextant_scrape_errors_total{code="unavailable",op="connect",project="`+projectA+`"} 1`) {
		t.Errorf("/metrics lacks the failed connect:\n%s", body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run: %v", err)
	}
	for _, want := range []string{"event=" + EventStart, "event=" + EventScrapeFailed, "event=" + EventStop} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log lacks %s:\n%s", want, log)
		}
	}
	if strings.Contains(log.String(), "BEGIN") {
		t.Errorf("the log holds a key:\n%s", log)
	}
}

// writeKey writes a new key, as ssh-keygen would, at path.
func writeKey(t *testing.T, path string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "sextant@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
