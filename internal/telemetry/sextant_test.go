package telemetry

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	sextantProject = "6f1c2b3a-4d5e-4f60-8a7b-9c0d1e2f3a4b"
	sextantOther   = "0a1b2c3d-4e5f-4a6b-8c7d-8e9f0a1b2c3d"
)

// Sextant's exporter offers the cost of each project's window by model,
// as gauges a poll replaces, and its own failures as a counter.
func TestSextantPull(t *testing.T) {
	t.Parallel()
	s, err := NewSextant(t.Context(), SextantConfig{Version: "v1", Pull: true})
	if err != nil {
		t.Fatalf("NewSextant: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	s.SetCost(sextantProject, []SextantCost{{Model: "example-large", USD: 8.35}, {Model: ModelOther, USD: 0.5}})
	s.ScrapeError(sextantProject, SextantOpCost, "unavailable")
	s.ScrapeError(sextantProject, SextantOpCost, "unavailable")
	text := promText(t, s.t)
	for _, want := range []string{
		`sextant_cost_usd{model="example-large",project="` + sextantProject + `"} 8.35`,
		`sextant_cost_usd{model="(other)",project="` + sextantProject + `"} 0.5`,
		`sextant_scrape_errors_total{code="unavailable",op="cost",project="` + sextantProject + `"} 2`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics lacks %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "aries_") || strings.Contains(text, "go_goroutine") {
		t.Errorf("/metrics holds ariesd's instruments, which are not Sextant's:\n%s", text)
	}

	// A poll replaces the project's figures: a model gone from the
	// window is gone from the series, and nil clears the project.
	s.SetCost(sextantProject, []SextantCost{{Model: "example-small", USD: 1}})
	text = promText(t, s.t)
	if strings.Contains(text, "example-large") || !strings.Contains(text, `sextant_cost_usd{model="example-small",project="`+sextantProject+`"} 1`) {
		t.Errorf("after a second poll, /metrics =\n%s", text)
	}
	s.SetCost(sextantProject, nil)
	if text = promText(t, s.t); strings.Contains(text, "sextant_cost_usd{") {
		t.Errorf("after the project's figures were cleared, /metrics =\n%s", text)
	}
}

// Every label takes a value from a fixed list or a cap, whatever a
// caller passes, so a server's text cannot add series without bound
// (rule 18) or put text on a series (rule 2).
func TestSextantSeriesAreBounded(t *testing.T) {
	t.Parallel()
	s, err := NewSextant(t.Context(), SextantConfig{Pull: true, Projects: 1, Models: 2})
	if err != nil {
		t.Fatalf("NewSextant: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	s.SetCost(sextantProject, []SextantCost{{Model: "a", USD: 1}, {Model: "b", USD: 2}, {Model: "c", USD: 3}, {Model: "has space", USD: 4}})
	s.SetCost(sextantOther, []SextantCost{{Model: "a", USD: 5}})
	s.SetCost("not-a-uuid", []SextantCost{{Model: "a", USD: 6}})
	s.ScrapeError(sextantProject, "drop table", "no such code")
	text := promText(t, s.t)
	for _, want := range []string{
		`sextant_cost_usd{model="a",project="` + sextantProject + `"} 1`,
		`sextant_cost_usd{model="b",project="` + sextantProject + `"} 2`,
		// c is past the cap of 2, and a model of the wrong shape is
		// (other), summed.
		`sextant_cost_usd{model="(other)",project="` + sextantProject + `"} 7`,
		// Projects past the cap, and one that is no project's id, are
		// other, summed.
		`sextant_cost_usd{model="a",project="other"} 11`,
		`sextant_scrape_errors_total{code="other",op="unknown",project="` + sextantProject + `"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics lacks %s:\n%s", want, text)
		}
	}
	for _, never := range []string{"has space", "drop table", "no such code", "not-a-uuid", `model="c"`} {
		if strings.Contains(text, never) {
			t.Errorf("/metrics holds %q:\n%s", never, text)
		}
	}
}

// With an endpoint, the figures are pushed over OTLP/HTTP under the
// service name sextant; with nothing configured nothing is exported and
// nothing started, and a nil *Sextant does nothing.
func TestSextantPush(t *testing.T) {
	t.Parallel()
	col := newCollector(t, http.StatusOK, nil)
	s, err := NewSextant(t.Context(), SextantConfig{Version: "v1", Getenv: env{
		"OTEL_EXPORTER_OTLP_ENDPOINT": col.URL, "OTEL_METRIC_EXPORT_INTERVAL": "50"}.get})
	if err != nil {
		t.Fatalf("NewSextant: %v", err)
	}
	s.SetCost(sextantProject, []SextantCost{{Model: "example-large", USD: 8.35}})
	body, _, path := col.carrying(t, "sextant.cost.usd")
	if path != "/v1/metrics" {
		t.Errorf("export posted to %q, want /v1/metrics", path)
	}
	for _, want := range []string{"sextant.cost.usd", "example-large", "service.name", "sextant"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("the export does not carry %q", want)
		}
	}
	if bytes.Contains(body, []byte("ariesd")) {
		t.Errorf("the export names ariesd as its service")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}

	off, err := NewSextant(t.Context(), SextantConfig{})
	if err != nil {
		t.Fatalf("NewSextant with nothing configured: %v", err)
	}
	if off.Enabled() {
		t.Errorf("a Sextant with nothing configured is Enabled")
	}
	off.SetCost(sextantProject, []SextantCost{{Model: "a", USD: 1}})
	off.ScrapeError(sextantProject, SextantOpCost, "unavailable")
	if err := off.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown with nothing configured: %v", err)
	}
	var none *Sextant
	none.SetCost(sextantProject, nil)
	none.ScrapeError(sextantProject, SextantOpCost, "unavailable")
	if err := none.Shutdown(ctx); err != nil {
		t.Errorf("nil Shutdown: %v", err)
	}
}

// The listener serves Sextant's /metrics and health as it does ariesd's.
func TestSextantServe(t *testing.T) {
	t.Parallel()
	s, err := NewSextant(t.Context(), SextantConfig{Pull: true})
	if err != nil {
		t.Fatalf("NewSextant: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	s.ScrapeError(sextantProject, SextantOpConnect, "auth")
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l, ServeOptions{}) }()
	base := "http://" + l.Addr().String()
	if body := get(t, base+"/metrics"); !strings.Contains(body, `sextant_scrape_errors_total{code="auth",op="connect",project="`+sextantProject+`"} 1`) {
		t.Errorf("/metrics =\n%s", body)
	}
	if body := get(t, base+"/healthz"); body != "ok\n" {
		t.Errorf("/healthz = %q", body)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve: %v", err)
	}
}
