package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// noEnv is an environment with nothing set: no ssh-agent, no key file.
func noEnv(string) string { return "" }

// TestUnreachableByCode pins which errors say the server was not
// reached: a refusal coded unavailable, and nothing else. A refusal of
// this client or this person, a mistake in the request, the server's
// rate limit and an error that is not a refusal at all are not.
func TestUnreachableByCode(t *testing.T) {
	t.Parallel()
	refusal := func(c proto.Code) *proto.Error { return proto.Errf(c, "a fix", "a message") }
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unavailable", refusal(proto.CodeUnavailable), true},
		{"unavailable, wrapped", fmt.Errorf("finish: %w", refusal(proto.CodeUnavailable)), true},
		{"busy", refusal(proto.CodeBusy), false},
		{"auth", refusal(proto.CodeAuth), false},
		{"version", refusal(proto.CodeVersion), false},
		{"forbidden", refusal(proto.CodeForbidden), false},
		{"invalid", refusal(proto.CodeInvalid), false},
		{"not_found", refusal(proto.CodeNotFound), false},
		{"conflict", refusal(proto.CodeConflict), false},
		{"acceptance", refusal(proto.CodeAcceptance), false},
		{"exists", refusal(proto.CodeExists), false},
		{"cycle", refusal(proto.CodeCycle), false},
		{"no config", noConfigError{pe: refusal(proto.CodeInvalid)}, false},
		{"not a refusal", errors.New("decode finish result: unexpected end of JSON input"), false},
		{"a canceled call", fmt.Errorf("finish: %w", context.Canceled), false},
		{"this machine lacks a program", &setupError{pe: proto.Errf(proto.CodeUnavailable, "install it", "gcloud is not on PATH")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Unreachable(tc.err); got != tc.want {
				t.Errorf("Unreachable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestUnreachableFromDial runs Dial into each way of not connecting and
// checks what it calls unreachable. A host key that does not match the
// pin, and a key the server does not know, are refusals: they must never
// pass for an outage.
func TestUnreachableFromDial(t *testing.T) {
	t.Parallel()
	mine, stranger := newKeyPair(t), newKeyPair(t)
	d := newFakeSSHD(t, map[string]keyPair{"me": mine})
	mine.write(t, d.root, "mine", "", false)
	stranger.write(t, d.root, "stranger", "", false)

	// A port nothing listens on, and one that hangs up on every caller.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()
	hangUp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			nc, err := hangUp.Accept()
			if err != nil {
				return
			}
			_ = nc.Close()
		}
	}()
	t.Cleanup(func() { _ = hangUp.Close(); <-done })

	dial := func(port int, hostKey, key string) error {
		cfg := &Config{Project: testProject, Key: key, Root: d.root}
		cfg.Server.Host, cfg.Server.Port, cfg.Server.User, cfg.Server.HostKey = "127.0.0.1", port, "starfix", hostKey
		c, err := Dial(t.Context(), cfg, Options{Timeout: 10 * time.Second, Getenv: noEnv,
			UserHomeDir: func() (string, error) { return d.home, nil }})
		if err == nil {
			_ = c.Close()
			t.Fatal("Dial succeeded against a stand-in that runs no starfixd")
		}
		return err
	}
	for _, tc := range []struct {
		name string
		err  error
		code proto.Code
		want bool
	}{
		{"nothing listens", dial(refused, d.hostKey, "mine"), proto.CodeUnavailable, true},
		{"the far end hangs up before SSH", dial(hangUp.Addr().(*net.TCPAddr).Port, d.hostKey, "mine"), proto.CodeUnavailable, true},
		{"sshd lets the key in and runs nothing", dial(d.port, d.hostKey, "mine"), proto.CodeUnavailable, true},
		{"host key mismatch", dial(d.port, testFpr, "mine"), proto.CodeAuth, false},
		{"a key the server does not know", dial(d.port, d.hostKey, "stranger"), proto.CodeAuth, false},
		{"a key file that is not there", dial(refused, d.hostKey, "absent"), proto.CodeAuth, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pe, ok := errors.AsType[*proto.Error](tc.err)
			if !ok || pe.Code != tc.code {
				t.Fatalf("Dial = %v, want a refusal coded %s", tc.err, tc.code)
			}
			if got := Unreachable(tc.err); got != tc.want {
				t.Errorf("Unreachable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A connection that drops while a call waits for its answer was lost
// with the request possibly applied: it is unreachable, which is what
// lets the caller queue the write under the key it was sent with.
func TestUnreachableWhenTheConnectionDrops(t *testing.T) {
	t.Parallel()
	c, srv := pipeConn(t, nil)
	go func() {
		srv.req()
		_ = srv.nc.Close()
	}()
	err := c.Call(t.Context(), proto.OpComment, proto.CommentArgs{ID: "sf-a", Body: "x", Idem: "k"}, nil)
	if err == nil || c.Err() == nil {
		t.Fatalf("Call on a dropped connection = %v, Err() = %v; want both set", err, c.Err())
	}
	if !Unreachable(err) {
		t.Errorf("Unreachable(%v) = false, want true", err)
	}
}

func TestKeyInfoConfigured(t *testing.T) {
	t.Parallel()
	for by, want := range map[string]bool{EnvKeyFile: true, "key: in " + ConfigFile: true, byDefault: false, byAgent: false, "": false} {
		if got := (KeyInfo{By: by}).Configured(); got != want {
			t.Errorf("KeyInfo{By: %q}.Configured() = %v, want %v", by, got, want)
		}
	}
}

// TestOffered lists the keys Dial would offer, without connecting: the
// one configured key, or the default identities in ssh's order.
func TestOffered(t *testing.T) {
	t.Parallel()
	a, b := newKeyPair(t), newKeyPair(t)
	root, home := t.TempDir(), t.TempDir()
	a.write(t, root, "named", "", false)
	b.write(t, sshDir(t, home), "id_ed25519", "", false)
	homeDir := func() (string, error) { return home, nil }
	for _, tc := range []struct {
		name string
		key  string
		want []string
	}{
		{"a configured key, alone", "named", []string{a.fingerprint()}},
		{"no key configured: the default identities", "", []string{b.fingerprint()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := Offered(&Config{Key: tc.key, Root: root}, noEnv, homeDir)
			if err != nil {
				t.Fatalf("Offered = %v", err)
			}
			var got []string
			for _, k := range keys {
				got = append(got, k.Fingerprint)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("Offered = %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := Offered(&Config{Key: "absent", Root: root}, noEnv, homeDir); err == nil {
		t.Error("Offered with a key file that is not there = nil error, want Dial's refusal")
	}
}
