package store

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// benchKey returns the nth public key of the benchmarks, in
// authorized_keys form without a comment, and its fingerprint. The keys
// are made from n, not at random: nothing signs with them.
func benchKey(tb testing.TB, n int) (key, fingerprint string) {
	tb.Helper()
	seed := make([]byte, ed25519.SeedSize)
	binary.BigEndian.PutUint64(seed, uint64(n)+1) //nolint:gosec // a counter
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(seed).Public())
	if err != nil {
		tb.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), ssh.FingerprintSHA256(pub)
}

// plantControl writes rows straight into the control database's tables,
// as `ariesd control init` run beside the daemon leaves them, so that
// a benchmark has thousands without thousands of writes: values is one
// "(?, …)" for each row, and args its arguments.
func plantControl(b *testing.B, c *Control, insert string, n int, row func(p *params, i int) string) {
	b.Helper()
	const chunk = 500
	for lo := 0; lo < n; lo += chunk {
		hi := min(lo+chunk, n)
		var p params
		var values []string
		for i := lo; i < hi; i++ {
			values = append(values, row(&p, i))
		}
		err := c.s.write(b.Context(), Actor{Principal: "dana", Session: "sess-d", Machine: "laptop-d"}, func(w *wtx) error {
			w.quiet = true
			_, err := w.exec(b.Context(), insert+strings.Join(values, ","), p.args...)
			return err
		})
		if err != nil {
			b.Fatalf("plant rows %d to %d: %v", lo, hi, err)
		}
	}
}

// BenchmarkControlIndex says what the control store's index holds in
// memory for each principal, key, project and membership, and what one
// reload of it costs: the daemon reads the whole of it again on every
// tick of its reaper, half a minute apart. It plants n principals with
// two keys each, 100 projects, and four memberships for each principal.
//
//	go test -run '^$' -bench 'ControlIndex' -benchtime 5x ./internal/store/
func BenchmarkControlIndex(b *testing.B) {
	for _, n := range []int{1000, 5000} {
		b.Run(fmt.Sprintf("principals=%d", n), func(b *testing.B) {
			ctx := b.Context()
			c, err := OpenControl(ctx, newDSN(b), ControlOptions{Admins: []string{"dana"}})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := c.Close(); err != nil {
					b.Errorf("close: %v", err)
				}
			})
			now := c.Now()
			// Reported after the loop: its start discards what was
			// reported before it.
			per := map[string]float64{}
			grew := func(what string, units int, plant func()) {
				b.Helper()
				before := heapNow()
				plant()
				if err := c.Reload(ctx); err != nil {
					b.Fatal(err)
				}
				per["heap-B/"+what] = float64(heapNow()-before) / float64(units)
			}
			const projects = 100
			grew("principal", n, func() {
				plantControl(b, c, `INSERT INTO principals (name, state, created_by, created_at, updated_at, rev) VALUES `, n, func(p *params, i int) string {
					return "(" + p.add(fmt.Sprintf("principal-%05d", i)) + ", 'active', 'dana', " + p.add(now, now) + ", 1)"
				})
			})
			grew("key", 2*n, func() {
				plantControl(b, c, `INSERT INTO principal_keys (fingerprint, principal, "key", comment, added_by, added_at) VALUES `, 2*n, func(p *params, i int) string {
					key, fp := benchKey(b, i)
					return "(" + p.add(fp, fmt.Sprintf("principal-%05d", i/2), key) + ", '', 'dana', " + p.add(now) + ")"
				})
			})
			grew("project", projects, func() {
				plantControl(b, c, `INSERT INTO projects (id, name, prefix, account, db_name, state, created_by, created_at, updated_at, rev) VALUES `,
					projects, func(p *params, i int) string {
						return "(" + p.add(fmt.Sprintf("00000000-0000-4000-8000-%012d", i), fmt.Sprintf("project-%03d", i), fmt.Sprintf("p%d", i)) +
							", 'acct', " + p.add(fmt.Sprintf("aries_p%03d", i)) + ", 'archived', 'dana', " + p.add(now, now) + ", 1)"
					})
			})
			grew("member", 4*n, func() {
				plantControl(b, c, `INSERT INTO members (principal, project, role, added_by, added_at, updated_at, rev) VALUES `, 4*n, func(p *params, i int) string {
					return "(" + p.add(fmt.Sprintf("principal-%05d", i/4), fmt.Sprintf("00000000-0000-4000-8000-%012d", (i/4+i%4*7)%projects)) +
						", 'member', 'dana', " + p.add(now, now) + ", 1)"
				})
			})
			b.ReportAllocs()
			for b.Loop() {
				if err := c.Reload(ctx); err != nil {
					b.Fatal(err)
				}
			}
			for unit, v := range per {
				b.ReportMetric(v, unit)
			}
		})
	}
}
