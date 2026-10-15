package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/spike/https/ws"
)

// The three pipes the design weighed (transport.md §3), each carrying
// Aries's own frames (internal/proto) unchanged:
//
//	/v1/connect  a WebSocket: one frame per message, both ways (chosen)
//	/v1/stream   one HTTP request whose body and response are both NDJSON, full duplex
//	/v1/rpc      one request frame per POST, and /v1/events for pushed frames over SSE
//
// Behind them is one stand-in for the daemon's session: hello and
// welcome, then echo, whoami, push and gate.pass.

// frames is a pipe as the session sees it.
type frames interface {
	read() (*proto.Frame, error)
	write(*proto.Frame) error
}

type wsFrames struct{ c *ws.Conn }

func (f wsFrames) read() (*proto.Frame, error) {
	m, err := f.c.Read()
	if err != nil {
		return nil, err
	}
	var fr proto.Frame
	if err := json.Unmarshal(m, &fr); err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	return &fr, nil
}

func (f wsFrames) write(fr *proto.Frame) error {
	b, err := json.Marshal(fr)
	if err != nil {
		return err
	}
	return f.c.Write(b)
}

type streamFrames struct {
	dec *proto.Decoder
	enc *proto.Encoder
}

func (f streamFrames) read() (*proto.Frame, error) { return f.dec.Decode() }
func (f streamFrames) write(fr *proto.Frame) error { return f.enc.Encode(fr) }

// flusher writes and flushes each frame, so none waits in a buffer.
type flusher struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f flusher) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}
	return n, f.rc.Flush()
}

// pushArgs asks for n pushed frames, every so many milliseconds.
type pushArgs struct {
	N     int `json:"n"`
	Every int `json:"every_ms"`
}

// pushed is a pushed frame's payload: when the server sent it, for the
// push latency.
type pushed struct {
	Seq  int   `json:"seq"`
	Sent int64 `json:"sent_ns"`
}

// session is the stand-in daemon's side of one connection.
func (s *server) session(ctx context.Context, f frames, g *grant, pipe string) {
	hello, err := f.read()
	if err != nil || hello.T != proto.FrameHello {
		return
	}
	sess := hello.Session
	if sess == "" {
		sess = "s-" + g.id
	}
	if err := f.write(&proto.Frame{T: proto.FrameWelcome, Version: "spike", Min: 8, Max: 8, Session: sess, Principal: g.principal}); err != nil {
		return
	}
	log.Printf("%s: %s connected (grant %s, %s), session %s", pipe, g.principal, g.id, g.flow, sess)
	reqs := make(chan *proto.Frame)
	go func() {
		defer close(reqs)
		for {
			fr, err := f.read()
			if err != nil {
				return
			}
			select {
			case reqs <- fr:
			case <-ctx.Done():
				return
			}
		}
	}()
	var pushes sync.WaitGroup
	defer pushes.Wait()
	// closing tells the client why, when the grant ended or was revoked,
	// before the connection closes (decision 59).
	closing := func() {
		if why := context.Cause(ctx); errors.Is(why, errSignInEnded) {
			_ = f.write(&proto.Frame{T: proto.FrameEvent, Op: "closed", E: mustJSON(proto.Errf(proto.CodeAuth, "run aries login", why.Error()))})
			log.Printf("%s: closed %s's connection: %v", pipe, g.principal, why)
		}
	}
	for {
		select {
		case <-ctx.Done():
			closing()
			return
		case fr, ok := <-reqs:
			if !ok {
				closing()
				return
			}
			if fr.T != proto.FrameReq {
				continue
			}
			res := &proto.Frame{T: proto.FrameRes, ID: fr.ID}
			switch fr.Op {
			case "echo":
				res.OK = fr.Args
			case "whoami":
				res.OK = mustJSON(map[string]any{"principal": g.principal, "scopes": g.scopes, "grant": g.id, "session": sess, "ends": g.end.UTC().Format(time.RFC3339Nano)})
			case "gate.pass":
				// A person's act from the session cli (decision 68): over
				// HTTPS, only a token with the cli scope may do it.
				if !slices.Contains(g.scopes, scopeCLI) {
					res.Err = proto.Errf(proto.CodeForbidden, "ask a person to pass it with aries gate pass in a terminal", "passing a gate needs aries in a terminal (scope cli)")
				} else {
					res.OK = mustJSON(map[string]string{"passed": "spike"})
				}
			case "push":
				var a pushArgs
				_ = json.Unmarshal(fr.Args, &a)
				a.N = min(max(a.N, 1), 1000)
				pushes.Go(func() {
					for i := range a.N {
						select {
						case <-ctx.Done():
							return
						case <-time.After(time.Duration(a.Every) * time.Millisecond):
						}
						if f.write(&proto.Frame{T: proto.FrameEvent, Op: "inbox", E: mustJSON(pushed{Seq: i, Sent: time.Now().UnixNano()})}) != nil {
							return
						}
					}
				})
				res.OK = mustJSON(map[string]int{"queued": a.N})
			default:
				res.Err = proto.Errf(proto.CodeInvalid, "see the spike's ops", "unknown op "+fr.Op)
			}
			if f.write(res) != nil {
				return
			}
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // a programming error: every value here marshals
	}
	return b
}

// connectWS is the chosen pipe. Authentication is on the upgrade
// request, before anything is hijacked; a refusal is an ordinary 401.
func (s *server) connectWS(w http.ResponseWriter, r *http.Request) {
	g, e := s.authenticate(r, s.base)
	if e != nil {
		s.refuse(w, e, false)
		return
	}
	c, err := ws.Accept(w, r)
	if err != nil {
		return
	}
	defer c.Close()
	ctx, done := s.watch(r.Context(), g)
	defer done()
	// Close the socket when the grant ends, so the read beneath the
	// session returns too.
	stop := context.AfterFunc(ctx, func() { _ = c.SetReadDeadline(time.Now()) })
	defer stop()
	// Proxies close idle connections (nginx after 60 s by default): a
	// ping every 25 s keeps one open with nothing to say.
	pinger := time.NewTicker(25 * time.Second)
	defer pinger.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pinger.C:
				if c.Ping() != nil {
					return
				}
			}
		}
	}()
	s.session(ctx, wsFrames{c}, g, "ws")
}

// connectStream is HTTP/2 full duplex (or HTTP/1.1 with
// EnableFullDuplex): frames in the request body, frames in the response.
func (s *server) connectStream(w http.ResponseWriter, r *http.Request) {
	g, e := s.authenticate(r, s.base)
	if e != nil {
		s.refuse(w, e, false)
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	ctx, done := s.watch(r.Context(), g)
	defer done()
	s.session(ctx, streamFrames{dec: proto.NewDecoder(r.Body), enc: proto.NewEncoder(flusher{w, rc})}, g, "stream")
}

// The request/response pipe keeps a stream of pushed frames per grant.
var sse = struct {
	sync.Mutex
	streams map[string]chan *proto.Frame
}{streams: map[string]chan *proto.Frame{}}

// rpc answers one request frame per POST; a push goes to the grant's
// event stream.
func (s *server) rpc(w http.ResponseWriter, r *http.Request) {
	g, e := s.authenticate(r, s.base)
	if e != nil {
		s.refuse(w, e, false)
		return
	}
	var fr proto.Frame
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, proto.MaxFrame)).Decode(&fr); err != nil {
		http.Error(w, "one frame is expected", http.StatusBadRequest)
		return
	}
	res := &proto.Frame{T: proto.FrameRes, ID: fr.ID}
	switch fr.Op {
	case "echo":
		res.OK = fr.Args
	case "push":
		var a pushArgs
		_ = json.Unmarshal(fr.Args, &a)
		sse.Lock()
		ch := sse.streams[g.id]
		sse.Unlock()
		if ch == nil {
			res.Err = proto.Errf(proto.CodeInvalid, "open /v1/events first", "no event stream")
			break
		}
		go func() {
			for i := range min(max(a.N, 1), 1000) {
				time.Sleep(time.Duration(a.Every) * time.Millisecond)
				select {
				case ch <- &proto.Frame{T: proto.FrameEvent, Op: "inbox", E: mustJSON(pushed{Seq: i, Sent: time.Now().UnixNano()})}:
				case <-time.After(5 * time.Second):
					return
				}
			}
		}()
		res.OK = mustJSON(map[string]int{"queued": a.N})
	default:
		res.Err = proto.Errf(proto.CodeInvalid, "see the spike's ops", "unknown op "+fr.Op)
	}
	writeJSON(w, 200, res)
}

// events is server-sent events. With nobuf=1 it sends nginx's
// X-Accel-Buffering: no, which is what a server must do to be heard
// through nginx's default buffering.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	g, e := s.authenticate(r, s.base)
	if e != nil {
		s.refuse(w, e, false)
		return
	}
	ch := make(chan *proto.Frame, 16)
	sse.Lock()
	sse.streams[g.id] = ch
	sse.Unlock()
	defer func() {
		sse.Lock()
		delete(sse.streams, g.id)
		sse.Unlock()
	}()
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	if r.URL.Query().Get("nobuf") == "1" {
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(200)
	_, _ = io.WriteString(w, ": open\n\n")
	_ = rc.Flush()
	ctx, done := s.watch(r.Context(), g)
	defer done()
	for {
		select {
		case <-ctx.Done():
			return
		case fr := <-ch:
			b, _ := json.Marshal(fr)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}
