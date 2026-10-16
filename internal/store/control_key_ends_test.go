package store

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"
)

// The migration gives every key from before no end, and the origin of
// a key an admin added.
func TestControlMigrationKeyEnds(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	// The control store as the three migrations before key ends left it.
	before := fstest.MapFS{}
	for _, name := range []string{"0001_init.sql", "0002_schema_name.sql", "0003_identities.sql"} {
		b, err := fs.ReadFile(controlMigrationFiles, "controlmigrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		before["controlmigrations/"+name] = &fstest.MapFile{Data: b}
	}
	old := controlSchema
	old.fsys = before
	db := testDB(t, dsn)
	d, err := ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn := db.sqlDB(d.Schema, 1)
	t.Cleanup(func() { _ = conn.Close() })
	if n, err := migrate(t.Context(), conn, identityAt, old, true); err != nil || n != 3 {
		t.Fatalf("migrate to version 3 ran %d: %v", n, err)
	}
	key, fp := testKey(t, "alice")
	for _, stmt := range []string{
		`INSERT INTO principals (name, state, created_by, created_at, updated_at, rev) VALUES ('alice', 'active', 'import', $1, $1, 1)`,
		`INSERT INTO principal_keys (fingerprint, principal, "key", comment, added_by, added_at) VALUES ('` + fp + `', 'alice', '` + key + `', '', 'dana', $1)`,
	} {
		if _, err := conn.ExecContext(t.Context(), stmt, identityAt); err != nil {
			t.Fatal(err)
		}
	}

	c := openControl(t, dsn, ControlOptions{})
	ks, err := c.Keys(t.Context(), "alice")
	if err != nil || len(ks) != 1 || ks[0].ExpiresAt != nil || ks[0].Origin != OriginAdmin || ks[0].Machine != "" {
		t.Fatalf("Keys(alice) after the migration = %+v, %v; want one key with no end, origin admin and no machine", ks, err)
	}
	if p, _, ok := c.AuthorizedKey(fp); !ok || p != "alice" {
		t.Errorf("AuthorizedKey(alice's key) after the migration = %q, %v; want alice's", p, ok)
	}
	for _, stmt := range []string{
		`UPDATE principal_keys SET origin = 'robot'`,
		`UPDATE principal_keys SET machine = '` + string(make([]byte, 65)) + `'`,
	} {
		if _, err := c.s.w.ExecContext(t.Context(), stmt); err == nil {
			t.Errorf("%s: no error, want the schema to refuse it", stmt)
		}
	}
}

// AddKeyEnding gives a key an end: from the store's clock for a
// duration, or as given, either to the second; it refuses an end that
// has passed, one past MaxKeyLifetime, and both at once.
func TestAddKeyEnding(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{})
	clk.add(500 * time.Millisecond) // the end is to the second, cut down
	now := c.Now()
	mustPrincipal(t, c, "alice")
	tests := []struct {
		name string
		end  KeyEnd
		want time.Time // zero: no end
		err  error
	}{
		{name: "none", end: KeyEnd{}},
		{name: "in an hour", end: KeyEnd{In: time.Hour}, want: identityAt.Add(time.Hour)},
		{name: "at a time", end: KeyEnd{At: identityAt.Add(48*time.Hour + 1500*time.Millisecond)}, want: identityAt.Add(48*time.Hour + time.Second)},
		{name: "at a time in another zone", end: KeyEnd{At: identityAt.Add(time.Hour).In(time.FixedZone("UTC-11", -11*3600))}, want: identityAt.Add(time.Hour)},
		{name: "at the lifetime's ceiling", end: KeyEnd{In: MaxKeyLifetime}, want: identityAt.Add(MaxKeyLifetime)},
		{name: "past the ceiling", end: KeyEnd{In: MaxKeyLifetime + time.Second}, err: ErrInvalid},
		{name: "at a time past the ceiling", end: KeyEnd{At: now.Add(MaxKeyLifetime + time.Second)}, err: ErrInvalid},
		{name: "now", end: KeyEnd{At: now}, err: ErrInvalid},
		{name: "within the second that has begun", end: KeyEnd{At: now.Add(100 * time.Millisecond)}, err: ErrInvalid},
		{name: "in the past", end: KeyEnd{At: now.Add(-time.Hour)}, err: ErrInvalid},
		{name: "a negative duration", end: KeyEnd{In: -time.Hour}, err: ErrInvalid},
		{name: "both", end: KeyEnd{At: now.Add(time.Hour), In: time.Hour}, err: ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line, fp := testKey(t, "alice "+tc.name)
			k, err := c.AddKeyEnding(t.Context(), dana, "alice", line, tc.end)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Errorf("AddKeyEnding(%+v) = %+v, %v; want %v", tc.end, k, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("AddKeyEnding(%+v): %v", tc.end, err)
			}
			if got := endOf(k); !got.Equal(tc.want) || k.Origin != OriginAdmin {
				t.Errorf("AddKeyEnding(%+v) = end %v, origin %q; want %v, admin", tc.end, got, k.Origin, tc.want)
			}
			ks, err := c.Keys(t.Context(), "alice")
			if err != nil {
				t.Fatalf("Keys(alice): %v", err)
			}
			if got := endOf(keyOf(ks, fp)); !got.Equal(tc.want) {
				t.Errorf("Keys(alice) gives the key end %v, want %v", got, tc.want)
			}
		})
	}
}

// A key the principal has already is returned as it is when its end is
// the one asked for, and refused when another is: an end is changed by
// removing the key and adding it again.
func TestAddKeyEndingAgain(t *testing.T) {
	t.Parallel()
	c, _ := clockControl(t, ControlOptions{})
	mustPrincipal(t, c, "alice")
	ctx := t.Context()
	line, _ := testKey(t, "alice")
	if _, err := c.AddKey(ctx, dana, "alice", line); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddKeyEnding(ctx, dana, "alice", line, KeyEnd{}); err != nil {
		t.Errorf("adding a key with no end again: %v, want it as it is", err)
	}
	if k, err := c.AddKeyEnding(ctx, dana, "alice", line, KeyEnd{In: time.Hour}); !errors.Is(err, ErrExists) {
		t.Errorf("giving a key with no end an end by adding it again = %+v, %v; want ErrExists", k, err)
	}
	other, _ := testKey(t, "alice 2")
	end := identityAt.Add(time.Hour)
	if _, err := c.AddKeyEnding(ctx, dana, "alice", other, KeyEnd{At: end}); err != nil {
		t.Fatal(err)
	}
	if k, err := c.AddKeyEnding(ctx, dana, "alice", other, KeyEnd{At: end}); err != nil || !endOf(k).Equal(end) {
		t.Errorf("adding a key again with its own end = %+v, %v; want it as it is", k, err)
	}
	for _, again := range []KeyEnd{{}, {At: end.Add(time.Hour)}} {
		if k, err := c.AddKeyEnding(ctx, dana, "alice", other, again); !errors.Is(err, ErrExists) {
			t.Errorf("adding a key that ends at %v again with end %+v = %+v, %v; want ErrExists", end, again, k, err)
		}
	}
}

// A key past its end authenticates no one, though the store still holds
// it; the index says when it ended, and when the next key ends.
func TestKeyEndsInTheIndex(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{})
	mustPrincipal(t, c, "alice")
	mustPrincipal(t, c, "bob")
	ctx := t.Context()
	short, shortFP := testKey(t, "alice short")
	long, longFP := testKey(t, "bob long")
	plain, plainFP := testKey(t, "alice plain")
	if _, err := c.AddKeyEnding(ctx, dana, "alice", short, KeyEnd{In: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddKeyEnding(ctx, dana, "bob", long, KeyEnd{In: 3 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddKey(ctx, dana, "alice", plain); err != nil {
		t.Fatal(err)
	}
	hour, three := identityAt.Add(time.Hour), identityAt.Add(3*time.Hour)

	check := func(when string, wantAuthorized []string, wantEnded []string, wantNext time.Time) {
		t.Helper()
		got := map[string]time.Time{}
		for _, k := range c.AuthorizedKeys() {
			got[k.Fingerprint] = endOf(k)
		}
		if len(got) != len(wantAuthorized) {
			t.Errorf("%s, AuthorizedKeys = %v; want %v", when, got, wantAuthorized)
		}
		for _, fp := range wantAuthorized {
			if _, ok := got[fp]; !ok {
				t.Errorf("%s, AuthorizedKeys lacks %s", when, fp)
			}
			if _, _, ok := c.AuthorizedKey(fp); !ok {
				t.Errorf("%s, AuthorizedKey(%s) is not ok", when, fp)
			}
			if _, _, ended := c.EndedKey(fp); ended {
				t.Errorf("%s, EndedKey(%s) says it ended", when, fp)
			}
		}
		ended, next := c.KeyEnds()
		if len(ended) != len(wantEnded) || !next.Equal(wantNext) {
			t.Errorf("%s, KeyEnds = %+v, next %v; want %v, next %v", when, ended, next, wantEnded, wantNext)
		}
		for _, fp := range wantEnded {
			if k := keyOf(ended, fp); k.ExpiresAt == nil {
				t.Errorf("%s, KeyEnds = %+v; want %s with its end", when, ended, fp)
			}
			if p, _, ok := c.AuthorizedKey(fp); ok {
				t.Errorf("%s, AuthorizedKey(%s) = %s: a key past its end authenticated", when, fp, p)
			}
			if _, end, ok := c.EndedKey(fp); !ok || end.IsZero() {
				t.Errorf("%s, EndedKey(%s) = %v, %v; want its end", when, fp, end, ok)
			}
		}
	}
	check("at the start", []string{shortFP, longFP, plainFP}, nil, hour)
	if got := c.AuthorizedKeys(); !endOf(keyOf(got, shortFP)).Equal(hour) || !endOf(keyOf(got, plainFP)).IsZero() {
		t.Errorf("AuthorizedKeys = %+v; want alice's short key ending at %v and her plain key with no end", got, hour)
	}
	clk.add(time.Hour - time.Microsecond)
	check("just before the first end", []string{shortFP, longFP, plainFP}, nil, hour)
	clk.add(time.Microsecond)
	check("at the first end", []string{longFP, plainFP}, []string{shortFP}, three)
	clk.add(2 * time.Hour)
	check("at the second end", []string{plainFP}, []string{shortFP, longFP}, time.Time{})

	// The index loaded again from the database says the same.
	if err := c.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	check("after a reload", []string{plainFP}, []string{shortFP, longFP}, time.Time{})
}

// keyOf finds the key with fingerprint fp in ks.
func keyOf(ks []PrincipalKey, fp string) PrincipalKey {
	for _, k := range ks {
		if k.Fingerprint == fp {
			return k
		}
	}
	return PrincipalKey{}
}

// PruneKeys deletes the keys whose end is more than KeyPruneAfter past,
// quietly: pruning is not history (rule 7).
func TestPruneKeys(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{})
	mustPrincipal(t, c, "alice")
	ctx := t.Context()
	line, fp := testKey(t, "alice")
	plain, _ := testKey(t, "alice plain")
	if _, err := c.AddKeyEnding(ctx, dana, "alice", line, KeyEnd{In: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddKey(ctx, dana, "alice", plain); err != nil {
		t.Fatal(err)
	}
	seq, err := c.LastEventSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clk.add(time.Minute + KeyPruneAfter)
	if n, err := c.PruneKeys(ctx); err != nil || n != 0 {
		t.Errorf("PruneKeys at KeyPruneAfter past the end = %d, %v; want 0: the key stays that long", n, err)
	}
	clk.add(time.Microsecond)
	if n, err := c.PruneKeys(ctx); err != nil || n != 1 {
		t.Errorf("PruneKeys past KeyPruneAfter = %d, %v; want 1", n, err)
	}
	if ks, err := c.Keys(ctx, "alice"); err != nil || len(ks) != 1 || ks[0].Fingerprint == fp {
		t.Errorf("Keys(alice) after pruning = %+v, %v; want the plain key alone", ks, err)
	}
	if _, _, ok := c.EndedKey(fp); ok {
		t.Error("EndedKey still knows the pruned key")
	}
	if after, err := c.LastEventSeq(ctx); err != nil || after != seq {
		t.Errorf("pruning moved the event log from %d to %d (%v); want no event", seq, after, err)
	}
	if n, err := c.PruneKeys(ctx); err != nil || n != 0 {
		t.Errorf("PruneKeys again = %d, %v; want 0", n, err)
	}
}

// A key the operator's `ariesd control init` imports is of origin
// import; any other, admin.
func TestKeyOrigin(t *testing.T) {
	t.Parallel()
	op := openControl(t, newDSN(t), ControlOptions{Operator: true})
	operator := Actor{Principal: OperatorPrincipal, Session: "control-init", Machine: "server"}
	if _, err := op.AddPrincipal(t.Context(), operator, "alice", ""); err != nil {
		t.Fatal(err)
	}
	line, _ := testKey(t, "alice")
	if k, err := op.AddKey(t.Context(), operator, "alice", line); err != nil || k.Origin != OriginImport {
		t.Errorf("AddKey by the operator = %+v, %v; want origin import", k, err)
	}
	if ks, err := op.Keys(t.Context(), "alice"); err != nil || len(ks) != 1 || ks[0].Origin != OriginImport {
		t.Errorf("Keys(alice) = %+v, %v; want one key of origin import", ks, err)
	}
}
