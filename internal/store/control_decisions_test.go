package store

import (
	"errors"
	"testing"
)

// TestControlOwnKeys pins decision 19: nobody changes the keys of their
// own account, whatever their rights, and a refusal records nothing.
// Another admin with reach may, and so may the operator.
func TestControlOwnKeys(t *testing.T) {
	c := accessWorld(t)
	ctx := t.Context()
	// alice administers her only project, so she is within her own reach.
	alicesKey := mustKey(t, c, "alice", "alice").Fingerprint
	danasKey := func() string { _, fp := testKey(t, "dana"); return fp }()
	bobsKey := func() string { _, fp := testKey(t, "bob"); return fp }()
	line := func(seed string) string { l, _ := testKey(t, seed); return l }

	refused := []struct {
		name   string
		do     func() error
		reason AccessReason
	}{
		{"a member adds a key to themselves", func() error { _, err := c.AddKey(ctx, bob, "bob", line("bob 2")); return err }, AccessOwnKeys},
		// Who a key belongs to is not said to someone who administers
		// nothing, so a member's own key is refused as any other is.
		{"a member removes their own key", func() error { _, err := c.RemoveKey(ctx, bob, bobsKey); return err }, AccessAdmin},
		{"a project admin with reach over themselves adds", func() error { _, err := c.AddKey(ctx, alice, "alice", line("alice 2")); return err }, AccessOwnKeys},
		{"a project admin with reach over themselves removes", func() error { _, err := c.RemoveKey(ctx, alice, alicesKey); return err }, AccessOwnKeys},
		{"a server admin adds", func() error { _, err := c.AddKey(ctx, dana, "dana", line("dana 2")); return err }, AccessOwnKeys},
		{"a server admin removes", func() error { _, err := c.RemoveKey(ctx, dana, danasKey); return err }, AccessOwnKeys},
		{"a server admin with no row adds", func() error { _, err := c.AddKey(ctx, sam, "sam", line("sam")); return err }, AccessOwnKeys},
		{"a server admin with no row adds themselves with a key", func() error { _, err := c.AddPrincipal(ctx, sam, "sam", "", line("sam")); return err }, AccessOwnKeys},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			seq := controlSeq(t, c)
			err := tc.do()
			var ae *AccessError
			if !errors.As(err, &ae) || ae.Reason != tc.reason || !errors.Is(err, ErrForbidden) {
				t.Fatalf("got %v, want an *AccessError with reason %q", err, tc.reason)
			}
			wantEvents(t, c, seq)
		})
	}
	for _, fp := range []string{alicesKey, danasKey, bobsKey} {
		if _, _, ok := c.AuthorizedKey(fp); !ok {
			t.Errorf("AuthorizedKey(%s) = not found after the refusals, want the key still there", fp)
		}
	}
	if _, err := c.Principal(ctx, "sam"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Principal(sam) after the refused add = %v, want ErrNotFound", err)
	}

	allowed := []struct {
		name string
		do   func() error
	}{
		{"another server admin adds to a server admin", func() error { _, err := c.AddKey(ctx, sam, "dana", line("dana 2")); return err }},
		{"another server admin removes a server admin's", func() error { _, err := c.RemoveKey(ctx, sam, danasKey); return err }},
		{"a project admin with reach adds to another project admin", func() error { _, err := c.AddKey(ctx, ivy, "alice", line("alice 2")); return err }},
		{"a project admin with reach removes another's", func() error { _, err := c.RemoveKey(ctx, ivy, alicesKey); return err }},
		{"a project admin adds to a member", func() error { _, err := c.AddKey(ctx, alice, "bob", line("bob 2")); return err }},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.do(); err != nil {
				t.Errorf("got %v, want it allowed", err)
			}
		})
	}

	// The operator's store is exempt, as it is from the role rules: its
	// principal is no one's account.
	op := openControl(t, newDSN(t), ControlOptions{Operator: true})
	operator := Actor{Principal: OperatorPrincipal, Session: "control-init", Machine: "server"}
	if _, err := op.AddPrincipal(ctx, operator, "dana", "", line("dana")); err != nil {
		t.Errorf("the operator adds a server admin with a key: %v", err)
	}
	if _, err := op.AddKey(ctx, operator, "dana", line("dana 2")); err != nil {
		t.Errorf("the operator adds a server admin's key: %v", err)
	}
}

// TestControlKeyRemoveSaysNothingOfOthers checks that a project admin
// who asks to remove a key learns nothing from the refusal: a key of a
// principal out of reach is refused as a key that does not exist is.
func TestControlKeyRemoveSaysNothingOfOthers(t *testing.T) {
	c := accessWorld(t)
	_, carols := testKey(t, "carol") // carol is in a project alice does not administer
	_, nobodys := testKey(t, "nobody")
	var msgs []string
	for _, fp := range []string{carols, nobodys} {
		_, err := c.RemoveKey(t.Context(), alice, fp)
		var ae *AccessError
		if !errors.As(err, &ae) || ae.Reason != AccessReach || ae.Principal != "" {
			t.Fatalf("RemoveKey(alice, %s) = %v; want an *AccessError with reason reach that names no principal", fp, err)
		}
		msgs = append(msgs, err.Error())
	}
	if msgs[0] != msgs[1] {
		t.Errorf("a key out of reach is refused with %q, and one that does not exist with %q; want the same", msgs[0], msgs[1])
	}
	// A server admin is told that a key does not exist.
	if _, err := c.RemoveKey(t.Context(), dana, nobodys); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveKey(dana, unknown) = %v, want ErrNotFound", err)
	}
}

// TestControlAddPrincipalWithKeys adds a principal and its first keys
// in one write: all of it, or nothing.
func TestControlAddPrincipalWithKeys(t *testing.T) {
	c := openControl(t, newDSN(t), ControlOptions{Limits: ControlLimits{Keys: 2}})
	ctx := t.Context()
	mustProject(t, c, projA)
	mustPrincipal(t, c, "alice", projA+"=admin")
	k1, fp1 := testKey(t, "one")
	k2, fp2 := testKey(t, "two")
	k3, fp3 := testKey(t, "three")

	seq := controlSeq(t, c)
	if _, err := c.AddPrincipal(ctx, alice, "bob", projA, k1+" laptop", k2); err != nil {
		t.Fatalf("AddPrincipal(bob, a, two keys) by the project's admin: %v", err)
	}
	wantEvents(t, c, seq, ev{OpPrincipalAdd, "bob", alice, `"name":"bob"`}, ev{OpMemberSet, projA + "/bob", alice, ""},
		ev{OpKeyAdd, fp1, alice, `"comment":"laptop"`}, ev{OpKeyAdd, fp2, alice, ""})
	for _, fp := range []string{fp1, fp2} {
		if p, _, ok := c.AuthorizedKey(fp); !ok || p != "bob" {
			t.Errorf("AuthorizedKey(%s) = %q, %v; want bob", fp, p, ok)
		}
	}

	for _, tc := range []struct {
		name string
		keys []string
		want error
	}{
		{"a key another principal has", []string{k3, k1}, ErrConflict},
		{"more keys than a principal keeps", []string{k3, testKeyLine(t, "four"), testKeyLine(t, "five")}, ErrInvalid},
		{"a line that is no key", []string{k3, "not a key"}, ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seq := controlSeq(t, c)
			if _, err := c.AddPrincipal(ctx, dana, "carol", projA, tc.keys...); !errors.Is(err, tc.want) {
				t.Fatalf("AddPrincipal(carol, %d keys) = %v, want %v", len(tc.keys), err, tc.want)
			}
			wantEvents(t, c, seq)
			if _, err := c.Principal(ctx, "carol"); !errors.Is(err, ErrNotFound) {
				t.Errorf("Principal(carol) = %v, want ErrNotFound: the write is all or nothing", err)
			}
			if _, _, ok := c.AuthorizedKey(fp3); ok {
				t.Errorf("AuthorizedKey(%s) answers after a refused add", fp3)
			}
		})
	}

	// A key given twice is one key.
	seq = controlSeq(t, c)
	if _, err := c.AddPrincipal(ctx, dana, "erin", "", k3, k3); err != nil {
		t.Fatalf("AddPrincipal(erin) with one key given twice: %v", err)
	}
	wantEvents(t, c, seq, ev{OpPrincipalAdd, "erin", dana, ""}, ev{OpKeyAdd, fp3, dana, ""})
}

// testKeyLine is testKey's line alone.
func testKeyLine(t *testing.T, seed string) string {
	t.Helper()
	line, _ := testKey(t, seed)
	return line
}

// TestControlDisabledWins pins decision 20: a principal whose row is
// disabled has no rights anywhere, even when the config file names it a
// server admin; a config admin with no row is not disabled.
func TestControlDisabledWins(t *testing.T) {
	dsn := newDSN(t)
	c := openControl(t, dsn, ControlOptions{})
	ctx := t.Context()
	mustProject(t, c, projA)
	mustPrincipal(t, c, "dana", projA+"=member")
	mustPrincipal(t, c, "alice", projA+"=admin")
	mustPrincipal(t, c, "bob", projA+"=member")

	rights := func(t *testing.T, c *Control, want bool) {
		t.Helper()
		if got := c.IsServerAdmin("dana"); got != want {
			t.Errorf("IsServerAdmin(dana) = %v, want %v", got, want)
		}
		role := Role("")
		if want {
			role = RoleAdmin
		}
		if got := c.Access("dana", projA).Role; got != role {
			t.Errorf("Access(dana, a).Role = %q, want %q", got, role)
		}
		if got := c.Reaches("dana", "bob"); got != want {
			t.Errorf("Reaches(dana, bob) = %v, want %v", got, want)
		}
		if got, err := c.IsAdminOf(ctx, "dana", projA); err != nil || got != want {
			t.Errorf("IsAdminOf(dana, a) = %v, %v; want %v", got, err, want)
		}
	}
	rights(t, c, true)

	// sam, who has no row, is a server admin, and disables dana.
	if !c.IsServerAdmin("sam") || c.Access("sam", projA).Role != RoleAdmin {
		t.Fatalf("sam, a config admin with no row: IsServerAdmin %v, role %q; want a server admin", c.IsServerAdmin("sam"), c.Access("sam", projA).Role)
	}
	if _, err := c.DisablePrincipal(ctx, sam, "dana"); err != nil {
		t.Fatalf("DisablePrincipal(dana) by sam: %v", err)
	}
	rights(t, c, false)

	// Her writes are refused, each with nothing recorded.
	for name, do := range map[string]func() error{
		"add a project":       func() error { _, err := c.AddProject(ctx, dana, newProject(projB)); return err },
		"archive a project":   func() error { _, err := c.ArchiveProject(ctx, dana, projA); return err },
		"add a principal":     func() error { _, err := c.AddPrincipal(ctx, dana, "hal", projA); return err },
		"set a member":        func() error { _, err := c.SetMember(ctx, dana, "bob", projA, RoleAdmin); return err },
		"remove a member":     func() error { _, err := c.RemoveMember(ctx, dana, "bob", projA); return err },
		"add a key":           func() error { _, err := c.AddKey(ctx, dana, "bob", testKeyLine(t, "bob")); return err },
		"disable a principal": func() error { _, err := c.DisablePrincipal(ctx, dana, "bob"); return err },
		"enable herself":      func() error { _, err := c.EnablePrincipal(ctx, dana, "dana"); return err },
		"create a database":   func() error { return c.CreateDatabase(ctx, dana, projA) },
	} {
		seq := controlSeq(t, c)
		if err := do(); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s by a disabled server admin = %v, want ErrForbidden", name, err)
		}
		wantEvents(t, c, seq)
	}

	// She is still a server admin's principal to everyone else: a project
	// admin may not enable her, or give her a key.
	for name, do := range map[string]func() error{
		"enable":    func() error { _, err := c.EnablePrincipal(ctx, alice, "dana"); return err },
		"add a key": func() error { _, err := c.AddKey(ctx, alice, "dana", testKeyLine(t, "dana")); return err },
	} {
		var ae *AccessError
		if err := do(); !errors.As(err, &ae) || ae.Reason != AccessServerAdminPrincipal {
			t.Errorf("%s of a disabled server admin by a project admin = %v, want reason %q", name, err, AccessServerAdminPrincipal)
		}
	}

	// The index says the same when it is read from the database.
	again := openControl(t, dsn, ControlOptions{})
	rights(t, again, false)
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := c.EnablePrincipal(ctx, sam, "dana"); err != nil {
		t.Fatalf("EnablePrincipal(dana) by sam: %v", err)
	}
	rights(t, c, true)
	if _, err := c.SetMember(ctx, dana, "bob", projA, RoleAdmin); err != nil {
		t.Errorf("SetMember by dana, enabled again: %v", err)
	}
}

// TestControlLastServerAdmin refuses disabling the last server admin
// who is not disabled: nobody could then enable anyone remotely.
func TestControlLastServerAdmin(t *testing.T) {
	c := newControl(t) // server admins dana and sam
	ctx := t.Context()
	mustPrincipal(t, c, "dana")
	mustPrincipal(t, c, "sam")
	mustPrincipal(t, c, "bob")

	// Either may be disabled while the other is not.
	if _, err := c.DisablePrincipal(ctx, dana, "sam"); err != nil {
		t.Fatalf("DisablePrincipal(sam) with dana enabled: %v", err)
	}
	seq := controlSeq(t, c)
	_, err := c.DisablePrincipal(ctx, dana, "dana")
	var last *LastAdminError
	if !errors.As(err, &last) || last.Principal != "dana" || !errors.Is(err, ErrInvalid) {
		t.Fatalf("DisablePrincipal(dana), the last enabled server admin = %v; want a *LastAdminError", err)
	}
	wantEvents(t, c, seq)
	if !c.IsServerAdmin("dana") {
		t.Error("IsServerAdmin(dana) = false after the refusal")
	}
	// Disabling someone who is no server admin is no matter.
	if _, err := c.DisablePrincipal(ctx, dana, "bob"); err != nil {
		t.Errorf("DisablePrincipal(bob): %v", err)
	}
	// With sam back, dana may go.
	if _, err := c.EnablePrincipal(ctx, dana, "sam"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DisablePrincipal(ctx, dana, "dana"); err != nil {
		t.Errorf("DisablePrincipal(dana) with sam enabled: %v", err)
	}

	// A config admin with no row is not disabled, so it counts.
	c2 := newControl(t)
	mustPrincipal(t, c2, "dana")
	if _, err := c2.DisablePrincipal(ctx, dana, "dana"); err != nil {
		t.Errorf("DisablePrincipal(dana) while sam, who has no row, is a server admin: %v", err)
	}
}
