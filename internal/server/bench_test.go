package server

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// BenchmarkRequest measures a request through the server and a real
// store, a read and a write, with traces off, with traces on and the
// trace not sampled, and with every span recorded: what tracing costs a
// request (design §16.6). It needs Postgres.
//
//	go test -run '^$' -bench Request -benchmem ./internal/server/
func BenchmarkRequest(b *testing.B) {
	for _, mode := range []struct {
		name    string
		sampler string // empty: traces off
	}{{"traces off", ""}, {"unsampled", "always_off"}, {"sampled", "always_on"}} {
		var tel *telemetry.Telemetry
		var spans *telemetry.Manual
		if mode.sampler != "" {
			env := map[string]string{"OTEL_TRACES_SAMPLER": mode.sampler}
			var err error
			tel, spans, err = telemetry.NewManual(telemetry.Config{Ops: Ops(), Getenv: func(k string) string { return env[k] }})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
		}
		dsn := newDatabase(b)
		// The metrics stay off in every mode, so that the difference is
		// the spans'.
		st, err := store.Open(b.Context(), dsn, store.Options{Prefix: "sf", Tracer: StoreTracer(tel, project)})
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { _ = st.Close() })
		s, err := New(Config{Store: st, Project: project, PeerCheck: func(net.Conn) error { return nil }, ReapInterval: -1,
			Limits: Limits{WriteRate: 1e9, WriteBurst: 1 << 30}, Telemetry: tel})
		if err != nil {
			b.Fatal(err)
		}
		srv, cli := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.handle(context.Background(), srv)
		}()
		b.Cleanup(func() {
			_ = cli.Close()
			<-done
		})
		enc, dec := proto.NewEncoder(cli), proto.NewDecoder(cli)
		go func() {
			_ = enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: "alice"})
			_ = enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: project, Session: "s-bench", Machine: "bench"})
		}()
		if w, err := dec.Decode(); err != nil || w.Err != nil {
			b.Fatalf("welcome: %+v, %v", w, err)
		}
		var id uint64
		ask := func(op string, args any) *proto.Frame {
			id++
			f, err := proto.Request(id, op, args)
			if err != nil {
				b.Fatal(err)
			}
			f.Traceparent = sampledTP
			sent := make(chan error, 1)
			go func() { sent <- enc.Encode(f) }()
			r, err := dec.Decode()
			if err != nil || <-sent != nil || r.Err != nil {
				b.Fatalf("%s: %+v, %v", op, r, err)
			}
			return r
		}
		r := ask(proto.OpCreate, proto.CreateArgs{Title: "bench"})
		var created proto.CreateResult
		if err := json.Unmarshal(r.OK, &created); err != nil {
			b.Fatal(err)
		}
		for _, req := range []struct {
			name string
			op   string
			args any
		}{
			{"read", proto.OpClaims, proto.LimitArgs{}},
			{"write", proto.OpComment, proto.CommentArgs{ID: created.ID, Body: "progress"}},
		} {
			b.Run(req.name+"/"+mode.name, func(b *testing.B) {
				b.ReportAllocs()
				n := 0
				for b.Loop() {
					ask(req.op, req.args)
					if n++; spans != nil && n%1024 == 0 {
						spans.ResetSpans() // the test's recorder, not the server's memory
					}
				}
			})
		}
	}
}
