package proto

import (
	"encoding/json"
	"testing"
	"time"
)

// TestWireNames pins the names the protocol settled on for v1.0.0
// (decision 88, the surface review's §4): id for the issue an op is
// about and issue where the record is something else, due for an
// issue's due date as create and update take it, name and fingerprint
// where those are meant, a lock's epoch, and no import-only field on an
// issue. Each value is marshaled whole, so a name that changes or comes
// back fails here.
func TestWireNames(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		v    any
		want string
	}{
		{"Issue", Issue{ID: "ar-a", Parent: "ar-p", Title: "t", Status: "open", Type: "task", Due: &at, CreatedBy: "alice", CreatedAt: at, UpdatedAt: at, Rev: 1},
			`{"id":"ar-a","parent":"ar-p","title":"t","status":"open","priority":0,"type":"task","due":"2026-10-15T12:00:00Z","created_by":"alice","created_at":"2026-10-15T12:00:00Z","updated_at":"2026-10-15T12:00:00Z","rev":1}`},
		{"Summary", Summary{ID: "ar-a", Title: "t", Status: "open", Due: &at},
			`{"id":"ar-a","title":"t","status":"open","priority":0,"due":"2026-10-15T12:00:00Z"}`},
		{"Claim", Claim{Issue: "ar-a", By: "alice", Session: "s", Machine: "m", Epoch: 2, ExpiresAt: at},
			`{"issue":"ar-a","by":"alice","session":"s","machine":"m","epoch":2,"expires_at":"2026-10-15T12:00:00Z"}`},
		{"Overlap", Overlap{Issue: "ar-a", By: "bob", Session: "s"},
			`{"issue":"ar-a","by":"bob","session":"s"}`},
		{"HoursLogArgs", HoursLogArgs{Issue: "ar-a", Seconds: 60},
			`{"issue":"ar-a","seconds":60}`},
		{"LockReleaseResult", LockReleaseResult{Name: "deploy", Epoch: 3},
			`{"name":"deploy","epoch":3}`},
		{"PrincipalResult", PrincipalResult{Name: "bob", Rev: 1, State: "active"},
			`{"name":"bob","rev":1,"state":"active"}`},
		{"MemberSetResult", MemberSetResult{Name: "bob", Rev: 1, Project: "p", ProjectName: "web", Role: "member", Change: "added"},
			`{"name":"bob","rev":1,"project":"p","project_name":"web","role":"member","change":"added"}`},
		{"MemberRemoveResult", MemberRemoveResult{Name: "bob", Project: "p", ProjectName: "web"},
			`{"name":"bob","project":"p","project_name":"web"}`},
		{"KeyResult", KeyResult{Fingerprint: "SHA256:x", Principal: "bob", Type: "ssh-ed25519"},
			`{"fingerprint":"SHA256:x","principal":"bob","type":"ssh-ed25519"}`},
		// The identity names of the surface review's §9: display_name, and
		// a time is expires_at.
		{"PrincipalAddArgs", PrincipalAddArgs{Name: "bob", Kind: KindService, Owner: "alice", DisplayName: "Bob", Invite: "b@example.com", Provider: "google"},
			`{"name":"bob","kind":"service","owner":"alice","display_name":"Bob","invite":"b@example.com","provider":"google"}`},
		{"PrincipalSetArgs", PrincipalSetArgs{Name: "bob", DisplayName: new(""), Kind: KindPerson},
			`{"name":"bob","display_name":"","kind":"person"}`},
		{"Identity", Identity{ID: 7, Principal: "bob", Provider: "google", Address: "b@example.com", State: IdentityPending, InvitedBy: "dana",
			InvitedAt: at, ExpiresAt: &at},
			`{"id":7,"principal":"bob","provider":"google","address":"b@example.com","state":"pending","invited_by":"dana","invited_at":"2026-10-15T12:00:00Z","expires_at":"2026-10-15T12:00:00Z"}`},
		{"InviteAddArgs", InviteAddArgs{Name: "bob", Provider: "google", Address: "b@example.com"},
			`{"name":"bob","provider":"google","address":"b@example.com"}`},
		{"IdentityArgs", IdentityArgs{ID: 7}, `{"id":7}`},
		// Trust rules (N5): a list is a bare plural, and a lifetime is
		// token_lifetime, as in workload: settings.
		{"TrustAddArgs", TrustAddArgs{Name: "ci-main", Principal: "ci", Provider: "github-actions", Claims: map[string]string{"repository_id": "1"}, TokenLifetime: "1h"},
			`{"name":"ci-main","principal":"ci","provider":"github-actions","claims":{"repository_id":"1"},"token_lifetime":"1h"}`},
		{"TrustSetArgs", TrustSetArgs{Name: "ci-main", State: TrustDisabled}, `{"name":"ci-main","state":"disabled"}`},
		{"TrustRulesArgs", TrustRulesArgs{Principal: "ci"}, `{"principal":"ci"}`},
		// A key's end (S1, identity.md §12): expires_at, or expires_in for
		// a duration the server counts from its own time.
		{"KeyAddArgs", KeyAddArgs{Name: "bob", Key: "ssh-ed25519 AAAA", ExpiresAt: &at},
			`{"name":"bob","key":"ssh-ed25519 AAAA","expires_at":"2026-10-15T12:00:00Z"}`},
		{"KeyAddArgs in", KeyAddArgs{Name: "bob", Key: "ssh-ed25519 AAAA", ExpiresIn: "30d"},
			`{"name":"bob","key":"ssh-ed25519 AAAA","expires_in":"30d"}`},
		{"KeyResult with an end", KeyResult{Fingerprint: "SHA256:x", Principal: "bob", Type: "ssh-ed25519", ExpiresAt: &at},
			`{"fingerprint":"SHA256:x","principal":"bob","type":"ssh-ed25519","expires_at":"2026-10-15T12:00:00Z"}`},
		{"Key", Key{Fingerprint: "SHA256:x", Type: "ssh-ed25519", AddedBy: "dana", AddedAt: at, ExpiresAt: &at, Origin: "admin", Machine: "laptop"},
			`{"fingerprint":"SHA256:x","type":"ssh-ed25519","added_by":"dana","added_at":"2026-10-15T12:00:00Z","expires_at":"2026-10-15T12:00:00Z","origin":"admin","machine":"laptop"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := json.Marshal(tc.v)
			if err != nil || string(b) != tc.want {
				t.Errorf("json.Marshal(%T) = %s, %v\nwant %s", tc.v, b, err, tc.want)
			}
		})
	}
	if OpResolve != "conflict.resolve" {
		t.Errorf("OpResolve = %q, want conflict.resolve, as its event is", OpResolve)
	}
	// A list is a bare plural, as principals, keys and members are.
	if OpInvites != "invites" || OpIdentities != "identities" {
		t.Errorf("the identity lists are %q and %q, want invites and identities", OpInvites, OpIdentities)
	}
}
