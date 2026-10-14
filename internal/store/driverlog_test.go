package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/dolttest"
	"github.com/ariesworx/starfix/internal/logs"
)

// lockedBuffer is a buffer a logger may write to from any goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// What the database driver says of a connection to Dolt that died goes
// to the store's logger, as a line of the schema, and not to the
// process's stderr unformatted (design §16, slice O1's note).
func TestDriverLogsThroughTheStoresLogger(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	// A Dolt of the test's own, and not the pool's, since it is restarted.
	// Its directory is short: t.TempDir's is too long for the path of
	// Dolt's socket.
	dir, err := os.MkdirTemp("", "starfix-dolt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	srv, err := dolttest.Start(ctx, dir)
	if errors.Is(err, dolttest.ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the store integration tests")
	}
	if err != nil {
		t.Fatalf("dolt sql-server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	dsn, err := srv.NewDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var log lockedBuffer
	s := openStore(t, dsn, Options{Logger: slog.New(slog.NewJSONHandler(&log, nil))})
	if _, err := s.Events(ctx, 0, 1); err != nil {
		t.Fatalf("Events before the restart: %v", err)
	}
	if err := srv.Restart(ctx); err != nil {
		t.Fatalf("restart dolt: %v", err)
	}
	// The pooled connections are dead. The driver finds that out when
	// one is used next, says so, and the pool dials again.
	for range 20 {
		_, _ = s.Events(ctx, 0, 1)
		if strings.Contains(log.String(), string(logs.DoltDriver)) {
			break
		}
	}
	var found bool
	for raw := range strings.SplitSeq(strings.TrimSpace(log.String()), "\n") {
		if raw == "" {
			continue
		}
		var l map[string]any
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("the store's log has a line that is not JSON: %q", raw)
		}
		if l[logs.KeyEvent] != string(logs.DoltDriver) {
			continue
		}
		found = true
		if d, _ := l[logs.KeyDetail].(string); d == "" {
			t.Errorf("the driver's line says nothing in %s: %v", logs.KeyDetail, l)
		}
		if l["level"] != "WARN" {
			t.Errorf("the driver's line is at level %v, want WARN", l["level"])
		}
	}
	if !found {
		t.Errorf("after Dolt restarted under open connections the store's log has no %s line:\n%s", logs.DoltDriver, log.String())
	}
}
