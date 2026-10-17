package server

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

// The per-address limits count an IPv4 address alone and an IPv6
// address by its network (rule 18): a client is given a /64 at the
// least, so each of its 2^64 addresses is not a bucket of its own.
func TestAddrBucket(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		addr   string
		prefix int
		want   string
	}{
		{"IPv4 alone", "192.0.2.77", 64, "192.0.2.77"},
		{"IPv6 by its /64", "2001:db8:1:2:aaaa:bbbb:cccc:dddd", 64, "2001:db8:1:2::"},
		{"IPv6 default prefix", "2001:db8:1:2:aaaa:bbbb:cccc:dddd", 0, "2001:db8:1:2::"},
		{"IPv6 by a /48", "2001:db8:1:2:aaaa:bbbb:cccc:dddd", 48, "2001:db8:1::"},
		{"IPv6 by a /128", "2001:db8:1:2:aaaa:bbbb:cccc:dddd", 128, "2001:db8:1:2:aaaa:bbbb:cccc:dddd"},
		{"IPv6 loopback", "::1", 64, "::"},
		{"zone dropped", "fe80::1%eth0", 64, "fe80::"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := addrBucket(netip.MustParseAddr(tc.addr), tc.prefix).String(); got != tc.want {
				t.Errorf("addrBucket(%s, %d) = %s, want %s", tc.addr, tc.prefix, got, tc.want)
			}
		})
	}
	t.Run("invalid address", func(t *testing.T) {
		t.Parallel()
		if got := addrBucket(netip.Addr{}, 64); got.IsValid() {
			t.Errorf("addrBucket of no address = %s, want none", got)
		}
	})
}

// Server.bucketOf reads a peer's address as the listener's own do:
// the port off, an IPv4-mapped address unmapped, then the network.
func TestServerBucketOf(t *testing.T) {
	t.Parallel()
	s := &Server{}
	s.cfg.Limits.HTTPSAddrPrefixV6 = 56
	for remote, want := range map[string]string{
		"192.0.2.1:4000":                 "192.0.2.1",
		"[::ffff:192.0.2.1]:4000":        "192.0.2.1",
		"[2001:db8:0:ff:1:2:3:4]:4000":   "2001:db8::",
		"[2001:db8:0:100:1:2:3:4]:4000":  "2001:db8:0:100::",
		"[2001:db8:0:1ff::9]:4000":       "2001:db8:0:100::",
		"[2001:db8:0:ff:ffff::9%en0]:80": "2001:db8::",
	} {
		if got := s.bucketOf(remote).String(); got != want {
			t.Errorf("bucketOf(%s) = %s, want %s", remote, got, want)
		}
	}
}

// Two addresses of one IPv6 /64 share https_conns_per_addr; another
// /64 does not.
func TestListenerCountsAnIPv6NetworkOnce(t *testing.T) {
	t.Parallel()
	l := &limitedListener{total: 100, perAddr: 2, prefixV6: 64}
	a := netip.MustParseAddr
	for i, addr := range []string{"2001:db8:1:2::1", "2001:db8:1:2:ffff::2"} {
		if got := l.enter(a(addr)); got != "" {
			t.Fatalf("connection %d from %s refused: %s", i+1, addr, got)
		}
	}
	if got := l.enter(a("2001:db8:1:2:1234::3")); got != "conns_per_addr" {
		t.Errorf("a third connection from the /64 = %q, want conns_per_addr", got)
	}
	if got := l.enter(a("2001:db8:1:3::1")); got != "" {
		t.Errorf("a connection from another /64 = %q, want it served", got)
	}
	l.leave(a("2001:db8:1:2:ffff::2"))
	if got := l.enter(a("2001:db8:1:2:9::9")); got != "" {
		t.Errorf("a connection after one of the /64 closed = %q, want it served", got)
	}
	// IPv4 stays one address to a bucket.
	for i, addr := range []string{"192.0.2.1", "192.0.2.1", "192.0.2.2"} {
		if got := l.enter(a(addr)); got != "" {
			t.Errorf("IPv4 connection %d from %s refused: %s", i+1, addr, got)
		}
	}
	if got := l.enter(a("192.0.2.1")); got != "conns_per_addr" {
		t.Errorf("a third connection from one IPv4 address = %q, want conns_per_addr", got)
	}
}

// Addresses of one /64 share signins_pending_per_addr and the auth
// failures a refused sign-in is charged to.
func TestPendingSignInsShareAnIPv6Network(t *testing.T) {
	t.Parallel()
	w := newLimitedSignInWorld(t, Limits{SignInsPendingPerAddr: 2, AuthFailureBurst: 3, AuthFailureRate: 0.01}, nil, nil)
	for i, tc := range []struct {
		from string
		want string
	}{
		{"[2001:db8:1:2::1]", ""},
		{"[2001:db8:1:2:aaaa::2]", ""},
		{"[2001:db8:1:2:bbbb::3]", "temporarily_unavailable"},
		{"[2001:db8:1:2:cccc::4]", "temporarily_unavailable"},
		{"[2001:db8:1:2:dddd::5]", "temporarily_unavailable"},
		{"[2001:db8:1:3::1]", ""},
	} {
		if got, why := refusedAt(w.authorizeFrom(t, newAttempt(t), tc.from)); got != tc.want {
			t.Errorf("request %d from %s refused with %q (%s), want %q", i, tc.from, got, why, tc.want)
		}
	}
	w.clk.advance(11 * time.Minute) // the sign-ins held lapsed; the /64's failures remain
	got, why := refusedAt(w.authorizeFrom(t, newAttempt(t), "[2001:db8:1:2:eeee::6]"))
	if got != "temporarily_unavailable" || !strings.Contains(why, "wait") {
		t.Errorf("a new address of a /64 with no auth failures left = %q (%s), want it refused, saying how long to wait", got, why)
	}
}
