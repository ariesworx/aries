package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ariesworx/aries/internal/server"
)

// TestRunDaemonLeaksNoSecret is rule 2 for a daemon's start
// (aries-afomcff2): `ariesd serve` as it runs, from a config file whose
// DSN holds a password, with its TLS key in a file, telemetry exported
// to a collector with a credential, and its stand-in key made at start;
// a server admin connected by key proof over HTTPS, a key refused,
// `ariesd status`, and a start refused for want of its database. The
// daemon's log, everything exported, what status printed and the lines
// a refused start prints hold none of the password, the TLS key, the
// stand-in key, the collector's credential or the admin's private key.
func TestRunDaemonLeaksNoSecret(t *testing.T) {
	t.Parallel()
	const password, credential = "Pg-pa55word-0d1e2f3a4b5c", "Bearer-otlp-cr3d-9a8b7c6d5e4f" //nolint:gosec // invented values the test greps for
	var mu sync.Mutex
	var exports [][]byte
	col := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		exports = append(exports, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(col.Close)
	exported := func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return bytes.Join(exports, []byte("\n"))
	}

	cert, key, pool := tlsFiles(t)
	s := newSite(t, "")
	// The DSN, with a password, from the config file alone, which must
	// then be 0600: Postgres here takes the role on trust, so the
	// password is carried and never checked.
	dsn := strings.Replace(s.dsn, "postgres://aries@", "postgres://aries:"+password+"@", 1)
	if dsn == s.dsn {
		t.Fatalf("the site's DSN %q names no role aries to give a password", s.dsn)
	}
	delete(s.env, server.EnvDSN)
	body := fmt.Sprintf("dsn: %s\nproject: %s\nprefix: acme\naccount: acme-2026\ncontrol_schema: %s\nadmins: [dana]\ntransports: [https]\n%s",
		dsn, adopted, s.controlSchema, httpsBlock(cert, key))
	rewrite(t, s, body)
	keys := writeKeys(t, forced(t, "dana", "dana-1"))
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys); got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	d := runSite(t, s, map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": col.URL, "OTEL_METRIC_EXPORT_INTERVAL": "50", "OTEL_BSP_SCHEDULE_DELAY": "50",
		"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=" + credential,
	})
	addr := listening(t, d)
	if f := connectWith(t, addr, pool, signerOf(t, "dana-1")); f.Err != nil || f.Principal != "dana" {
		t.Fatalf("dana over HTTPS = %+v, want welcomed", f)
	}
	if f := connectWith(t, addr, pool, signerOf(t, "nobody-1")); f.Err == nil {
		t.Fatalf("a key the server does not know was welcomed: %+v", f)
	}
	st := status(t, s, d.sock)
	if st.err != nil {
		t.Fatalf("ariesd status: %v\n%s", st.err, st.stderr)
	}
	waitFor(t, "the handshakes' spans to be exported", func() bool { return bytes.Contains(exported(), []byte("handshake")) })
	standIn, err := os.ReadFile(d.key)
	if err != nil {
		t.Fatalf("the stand-in key: %v", err)
	}
	if err := d.stop(); err != nil {
		t.Fatalf("runDaemon = %v", err)
	}

	// A start refused because the database is not there, with the
	// password in its DSN: the lines main prints, and the log.
	refusedLog := &logBuffer{}
	set := s.settings(t)
	set.DSN = strings.Replace(dsn, "host=", "host=/nonexistent", 1)
	if set.DSN == dsn {
		set.DSN = dsn + "&host=/nonexistent"
	}
	dir := t.TempDir()
	set.Socket = dir + "/d.sock"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	log, err := server.NewLogger(refusedLog, "debug", "text")
	if err != nil {
		t.Fatal(err)
	}
	startErr := runDaemon(ctx, set, log, func(string) string { return "" })
	if startErr == nil {
		t.Fatal("a daemon whose database is not there started")
	}
	refused := strings.Join(refusalLines(startErr), "\n") + "\n" + refusedLog.String()

	tlsKey, err := os.ReadFile(key) //nolint:gosec // the test's own key file
	if err != nil {
		t.Fatal(err)
	}
	seed := sha256.Sum256([]byte("dana-1"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	secrets := map[string]string{
		"the DSN's password":         password,
		"the collector's credential": credential,
		"the stand-in key":           strings.TrimSpace(string(standIn)),
		"dana's key, in hex":         hex.EncodeToString(priv),
		"dana's key, in base64":      base64.StdEncoding.EncodeToString(priv),
		"dana's key's seed, in hex":  hex.EncodeToString(seed[:]),
	}
	for i, line := range strings.Split(string(tlsKey), "\n") {
		if len(line) >= 16 && !strings.HasPrefix(line, "-----") {
			secrets[fmt.Sprintf("the TLS key, line %d", i+1)] = line
		}
	}
	sinks := map[string]string{
		"the daemon's log":           d.log.String(),
		"the exports":                string(exported()),
		"what ariesd status printed": st.stdout + st.stderr,
		"the refused start":          refused,
	}
	if !strings.Contains(sinks["the daemon's log"], "ariesd serving") || !strings.Contains(refused, "/nonexistent") {
		t.Fatalf("too little was captured to prove anything:\nlog:\n%s\nrefused:\n%s", d.log.String(), refused)
	}
	for sink, text := range sinks {
		for what, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s holds %s", sink, what)
			}
		}
		if strings.Contains(text, "PRIVATE KEY") {
			t.Errorf("%s holds a private key", sink)
		}
	}
}
