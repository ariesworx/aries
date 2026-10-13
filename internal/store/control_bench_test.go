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
// as `starfixd control init` run beside the daemon leaves them, so that
// a benchmark has thousands without thousands of writes: values is one
// "(?, …)" for each row, and args its arguments.
func plantControl(b *testing.B, c *Control, insert string, values []string, args []any, perRow int) {
	b.Helper()
	const chunk = 500
	for lo := 0; lo < len(values); lo += chunk {
		hi := min(lo+chunk, len(values))
		err := c.s.write(b.Context(), Actor{Principal: "dana", Session: "sess-d", Machine: "laptop-d"}, func(w *wtx) error {
			w.quiet = true
			_, err := w.exec(b.Context(), insert+strings.Join(values[lo:hi], ","), args[lo*perRow:hi*perRow]...)
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
			c, err := OpenControl(ctx, newDSN(b), ControlOptions{Admins: []string{"dana"}, CommitInterval: -1})
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
				var values []string
				var args []any
				for i := range n {
					values = append(values, "(?, 'active', 'dana', ?, ?, 1, ?)")
					args = append(args, fmt.Sprintf("principal-%05d", i), now, now, i)
				}
				plantControl(b, c, `INSERT INTO principals (name, state, created_by, created_at, updated_at, rev, write_id) VALUES `, values, args, 4)
			})
			grew("key", 2*n, func() {
				var values []string
				var args []any
				for i := range 2 * n {
					key, fp := benchKey(b, i)
					values = append(values, "(?, ?, ?, '', 'dana', ?)")
					args = append(args, fp, fmt.Sprintf("principal-%05d", i/2), key, now)
				}
				plantControl(b, c, "INSERT INTO principal_keys (fingerprint, principal, `key`, comment, added_by, added_at) VALUES ", values, args, 4)
			})
			grew("project", projects, func() {
				var values []string
				var args []any
				for i := range projects {
					values = append(values, "(?, ?, ?, 'acct', ?, 'archived', 'dana', ?, ?, 1, ?)")
					args = append(args, fmt.Sprintf("00000000-0000-4000-8000-%012d", i), fmt.Sprintf("project-%03d", i), fmt.Sprintf("p%d", i),
						fmt.Sprintf("starfix_p%03d", i), now, now, i)
				}
				plantControl(b, c, `INSERT INTO projects (id, name, prefix, account, db_name, state, created_by, created_at, updated_at, rev, write_id) VALUES `,
					values, args, 7)
			})
			grew("member", 4*n, func() {
				var values []string
				var args []any
				for i := range 4 * n {
					values = append(values, "(?, ?, 'member', 'dana', ?, ?, 1, ?)")
					args = append(args, fmt.Sprintf("principal-%05d", i/4), fmt.Sprintf("00000000-0000-4000-8000-%012d", (i/4+i%4*7)%projects), now, now, i)
				}
				plantControl(b, c, `INSERT INTO members (principal, project, role, added_by, added_at, updated_at, rev, write_id) VALUES `, values, args, 5)
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
