package client

import (
	"errors"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// TestDialPinsAnyListedHostKey: a dial takes the server for itself when
// it presents any one of the pinned host keys, wherever that key is in
// the list, and refuses it as before when it presents none of them,
// naming the key it was offered and every pinned one by fingerprint.
func TestDialPinsAnyListedHostKey(t *testing.T) {
	t.Parallel()
	w := newWelcomer(t, server(proto.ProtoMin, proto.ProtoMax))
	for _, tc := range []struct {
		name string
		pins []string
	}{
		{"the one key pinned", []string{w.hostKey}},
		{"the first of two", []string{w.hostKey, testFprNew}},
		{"the second of two, as while a new key is rolled out", []string{testFpr, w.hostKey}},
		{"the last of three", []string{testFpr, testFprNew, w.hostKey}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := w.dialPinned(t, tc.pins...); err != nil {
				t.Errorf("Dial pinning %v, to a server presenting %s = %v, want it welcomed", tc.pins, w.hostKey, err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		pins []string
		msg  []string
	}{
		{"one key, another", []string{testFpr},
			[]string{"host key mismatch for 127.0.0.1: the server presented " + w.hostKey + ", but .aries.yaml pins " + testFpr}},
		{"two keys, neither", []string{testFpr, testFprNew},
			[]string{"the server presented " + w.hostKey + ", but .aries.yaml pins " + testFpr + " and " + testFprNew}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.dialPinned(t, tc.pins...)
			pe, ok := errors.AsType[*proto.Error](err)
			if !ok || pe.Code != proto.CodeAuth {
				t.Fatalf("Dial pinning %v, to a server presenting %s = %v, want refused with code auth", tc.pins, w.hostKey, err)
			}
			for _, m := range tc.msg {
				if !strings.Contains(pe.Message, m) {
					t.Errorf("Dial pinning %v: message %q, want it to say %q", tc.pins, pe.Message, m)
				}
			}
			if !strings.Contains(pe.Fix, "server.host_keys") {
				t.Errorf("Dial pinning %v: fix %q, want it to name server.host_keys", tc.pins, pe.Fix)
			}
		})
	}
}
