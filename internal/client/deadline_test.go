package client

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// stall is how a stand-in server treats the request it is to leave
// unanswered.
type stall int

const (
	answersAt   stall = iota // reads it and answers after a while
	neverAnswer              // reads it and never answers, keeping the connection open
	neverReads               // reads nothing more, so the request is never written in full
)

// A request the server does not answer within RequestTimeout fails as
// unavailable, naming the op and the wait, and ends the connection,
// which can no longer be used; one answered in time succeeds however
// late; and an op the server may take longer over is waited out. The
// clock is a synctest bubble's, so the waits take no real time.
func TestCallDeadline(t *testing.T) {
	// Serial: a synctest bubble's virtual time takes no real time worth sharing.
	wait := RequestTimeout(proto.OpReady)
	tests := []struct {
		name string
		op   string
		// answered says how many requests the server answers at once,
		// before the one it stalls on.
		answered int
		how      stall
		after    time.Duration // when an answersAt server answers
		wantErr  bool
	}{
		{name: "answered late, within the deadline", op: proto.OpReady, how: answersAt, after: wait - time.Second},
		{name: "never answered, the connection open", op: proto.OpReady, how: neverAnswer, wantErr: true},
		{name: "one answered, then a stall", op: proto.OpShow, answered: 1, how: neverAnswer, wantErr: true},
		{name: "the request never read", op: proto.OpReady, how: neverReads, wantErr: true},
		{name: "answered just past the deadline", op: proto.OpReady, how: answersAt, after: wait + time.Second, wantErr: true},
		{name: "a control write, answered long past the deadline", op: proto.OpProjectArchive, how: answersAt, after: 5 * wait},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, s := pipeConn(t, nil)
				served := make(chan struct{})
				go func() {
					defer close(served)
					for range tc.answered {
						f, err := s.dec.Decode()
						if err != nil {
							return
						}
						s.send(&proto.Frame{T: proto.FrameRes, ID: f.ID, OK: []byte(`{}`)})
					}
					if tc.how == neverReads {
						return
					}
					f, err := s.dec.Decode()
					if err != nil {
						return
					}
					if tc.how == answersAt {
						time.Sleep(tc.after)
						// The client may have hung up by now.
						_ = s.enc.Encode(&proto.Frame{T: proto.FrameRes, ID: f.ID, OK: []byte(`{}`)})
					}
					// Then it reads until the client hangs up.
					for {
						if _, err := s.dec.Decode(); err != nil {
							return
						}
					}
				}()
				defer func() { _ = s.nc.Close(); <-served }()
				for range tc.answered {
					if err := c.Call(t.Context(), proto.OpReady, proto.LimitArgs{}, nil); err != nil {
						t.Fatalf("Call(ready) before the stall = %v", err)
					}
				}
				start := time.Now()
				err := c.Call(t.Context(), tc.op, proto.LimitArgs{}, nil)
				took := time.Since(start)
				if !tc.wantErr {
					if err != nil {
						t.Fatalf("Call(%s) answered after %s = %v, want nil", tc.op, tc.after, err)
					}
					if took != tc.after {
						t.Errorf("Call(%s) returned after %s, want %s, when the server answered", tc.op, took, tc.after)
					}
					if c.Err() != nil {
						t.Errorf("Err() = %v after an answered call, want nil", c.Err())
					}
					return
				}
				if took != wait {
					t.Errorf("Call(%s) returned after %s, want the deadline, %s", tc.op, took, wait)
				}
				pe, ok := errors.AsType[*proto.Error](err)
				if !ok || pe.Code != proto.CodeUnavailable {
					t.Fatalf("Call(%s) to a server that did not answer = %v, want an unavailable *proto.Error", tc.op, err)
				}
				if want := "the server did not answer " + tc.op + " within 1m15s, so aries closed the connection"; pe.Message != want {
					t.Errorf("Call(%s) message = %q, want %q", tc.op, pe.Message, want)
				}
				if !strings.Contains(pe.Fix, "retry") || !strings.Contains(pe.Fix, "ariesd status") {
					t.Errorf("Call(%s) fix = %q, want it to say to retry, and what the admin checks", tc.op, pe.Fix)
				}
				if !Unreachable(err) {
					t.Errorf("Unreachable(%v) = false, want true: the server may not have applied it, and a later attempt may get through", err)
				}
				synctest.Wait()
				select {
				case <-c.Done():
				default:
					t.Error("Done not closed after the deadline: the connection was kept")
				}
				if !errors.Is(c.Err(), err) {
					t.Errorf("Err() = %v, want the deadline's error %v", c.Err(), err)
				}
				start = time.Now()
				if again := c.Call(t.Context(), proto.OpReady, proto.LimitArgs{}, nil); !errors.Is(again, err) || time.Since(start) != 0 {
					t.Errorf("Call after the deadline = %v after %s, want %v at once", again, time.Since(start), err)
				}
			})
		})
	}
}

// RequestTimeout outlasts the server's own request timeout, a minute,
// for every op but those the server may take longer over, which only
// `aries admin` sends.
func TestRequestTimeout(t *testing.T) {
	t.Parallel()
	const server = time.Minute
	tests := []struct {
		op      string
		waitsOn bool // waited on without a deadline
	}{
		{op: proto.OpReady},
		{op: proto.OpCreate},
		{op: proto.OpWatch},
		{op: proto.OpRenew},
		{op: proto.OpStatus},
		{op: proto.OpProjects},
		{op: proto.OpProjectAdd, waitsOn: true},
		{op: proto.OpProjectArchive, waitsOn: true},
		{op: proto.OpProjectActivate, waitsOn: true},
		{op: proto.OpPrincipalAdd, waitsOn: true},
		{op: proto.OpPrincipalDisable, waitsOn: true},
		{op: proto.OpPrincipalEnable, waitsOn: true},
		{op: proto.OpMemberSet, waitsOn: true},
		{op: proto.OpMemberRemove, waitsOn: true},
		{op: proto.OpKeyAdd, waitsOn: true},
		{op: proto.OpKeyRemove, waitsOn: true},
		{op: "an op of a newer client"},
	}
	for _, tc := range tests {
		got := RequestTimeout(tc.op)
		switch {
		case tc.waitsOn && got != 0:
			t.Errorf("RequestTimeout(%q) = %s, want 0: the server may take longer", tc.op, got)
		case !tc.waitsOn && got <= server:
			t.Errorf("RequestTimeout(%q) = %s, want more than the server's %s", tc.op, got, server)
		}
	}
}
