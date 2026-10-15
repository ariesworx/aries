package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
	"github.com/ariesworx/starfix/internal/telemetry"
)

// plantedText marks every piece of text a test puts where a person or an
// agent writes: no span may carry it.
const plantedText = "PLANTED-zq9x-text"

// opArgs is every op with a zero value of its arguments.
var opArgs = map[string]any{
	proto.OpCreate: proto.CreateArgs{}, proto.OpShow: proto.ShowArgs{}, proto.OpList: proto.ListArgs{},
	proto.OpReady: proto.LimitArgs{}, proto.OpBlocked: proto.LimitArgs{}, proto.OpUpdate: proto.UpdateArgs{},
	proto.OpClose: proto.CloseArgs{}, proto.OpReopen: proto.ReopenArgs{}, proto.OpDepAdd: proto.DepArgs{}, proto.OpDepRm: proto.DepArgs{},
	proto.OpLabelAdd: proto.LabelArgs{}, proto.OpLabelRm: proto.LabelArgs{}, proto.OpComment: proto.CommentArgs{},
	proto.OpComments: proto.PageArgs{}, proto.OpHistory: proto.PageArgs{}, proto.OpStart: proto.StartArgs{},
	proto.OpFinish: proto.FinishArgs{}, proto.OpHandoff: proto.HandoffArgs{}, proto.OpDigest: proto.DigestArgs{},
	proto.OpRenew: proto.RenewArgs{}, proto.OpWho: proto.WhoArgs{}, proto.OpInbox: proto.InboxArgs{}, proto.OpAck: proto.AckArgs{},
	proto.OpWatch: proto.WatchArgs{}, proto.OpAccept: proto.AcceptArgs{}, proto.OpUsage: proto.UsageArgs{}, proto.OpClaims: proto.LimitArgs{},
	proto.OpRemember: proto.RememberArgs{}, proto.OpRecall: proto.RecallArgs{}, proto.OpForget: proto.ForgetArgs{}, proto.OpPin: proto.PinArgs{},
	proto.OpPriceSet: proto.PriceSetArgs{}, proto.OpPrices: proto.PricesArgs{}, proto.OpCost: proto.CostArgs{},
	proto.OpHoursLog: proto.HoursLogArgs{}, proto.OpHoursDelete: proto.HoursDeleteArgs{}, proto.OpHours: proto.HoursArgs{},
	proto.OpPlanSet: proto.PlanSetArgs{}, proto.OpPlans: proto.PlansArgs{},
	proto.OpConflicts: proto.ConflictsArgs{}, proto.OpPropose: proto.ProposeArgs{}, proto.OpResolve: proto.ResolveArgs{},
	proto.OpGateAdd: proto.GateAddArgs{}, proto.OpGatePass: proto.GateArgs{}, proto.OpGateWaive: proto.GateArgs{},
	proto.OpGateRearm: proto.GateArgs{}, proto.OpGates: proto.GatesArgs{}, proto.OpDates: proto.DatesArgs{},
	proto.OpGateFail:    proto.GateArgs{},
	proto.OpLockAcquire: proto.LockAcquireArgs{}, proto.OpLockRenew: proto.LockRenewArgs{}, proto.OpLockRelease: proto.LockReleaseArgs{},
	proto.OpLockLeave: proto.LockLeaveArgs{}, proto.OpLocks: proto.LocksArgs{},

	proto.OpProjectAdd: proto.ProjectAddArgs{}, proto.OpProjects: proto.AdminListArgs{}, proto.OpProjectArchive: proto.ProjectArgs{},
	proto.OpProjectActivate: proto.ProjectArgs{}, proto.OpPrincipalAdd: proto.PrincipalAddArgs{}, proto.OpPrincipals: proto.PrincipalsArgs{},
	proto.OpPrincipalDisable: proto.PrincipalArgs{}, proto.OpPrincipalEnable: proto.PrincipalArgs{},
	proto.OpPrincipalWhois: proto.PrincipalWhoisArgs{}, proto.OpMemberSet: proto.MemberSetArgs{}, proto.OpMembers: proto.MembersArgs{},
	proto.OpMemberRemove: proto.MemberArgs{}, proto.OpKeyAdd: proto.KeyAddArgs{}, proto.OpKeys: proto.KeysArgs{},
	proto.OpKeyRemove: proto.KeyRemoveArgs{}, proto.OpStatus: proto.StatusArgs{},
}

// soaked returns a copy of args, a struct, with plantedText and the
// field's name in every string it holds, at any depth: fields, pointers,
// slices and the values of maps.
func soaked(args any) any {
	v := reflect.New(reflect.TypeOf(args)).Elem()
	soak(v, reflect.TypeOf(args).Name())
	return v.Interface()
}

func soak(v reflect.Value, path string) {
	if !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(plantedText + " in " + path)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		soak(v.Elem(), path)
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			return
		}
		for i := range v.NumField() {
			soak(v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		soak(v.Index(0), path)
	case reflect.Map:
		if v.Type().Elem().Kind() != reflect.String {
			return
		}
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(reflect.New(v.Type().Key()).Elem(), reflect.ValueOf(plantedText+" in "+path))
		v.Set(m)
	}
}

// soakServer sends m's server requests with plantedText in every place
// text can go: requests that succeed, so that the text is stored and
// read back, then every op with every string of its arguments soaked,
// most of which are refused with a message that quotes them.
func soakServer(t *testing.T, m *multi) {
	t.Helper()
	// A session and a machine are the client's text too.
	a := mustDialTo(t, m.Server, store.Actor{Principal: "alice", Session: plantedText + "-session", Machine: plantedText + "-machine"}, projA)
	d := mustDialTo(t, m.Server, store.Actor{Principal: "dana", Session: plantedText + "-admin-session", Machine: "desk"}, projA)

	text := func(field string) string { return plantedText + " in a " + field }
	is := mustAsk[proto.CreateResult](a, proto.OpCreate, proto.CreateArgs{Title: text("title"), Body: text("body"), Design: text("design"),
		Acceptance: "- [ ] " + text("acceptance item"), Notes: text("notes"), Labels: []string{"planted-zq9x-label"},
		Paths: []string{"src/planted-zq9x-text/file.go"}})
	title, body := text("new title"), text("new body")
	mustAsk[proto.WriteResult](a, proto.OpUpdate, proto.UpdateArgs{ID: is.ID, Rev: is.Rev, Title: &title, Body: &body})
	mustAsk[proto.CommentResult](a, proto.OpComment, proto.CommentArgs{ID: is.ID, Body: text("comment")})
	mustAsk[proto.StartResult](a, proto.OpStart, proto.StartArgs{ID: is.ID})
	mustAsk[proto.WriteResult](a, proto.OpHandoff, proto.HandoffArgs{ID: is.ID, Note: text("handoff"),
		HandoffFields: proto.HandoffFields{State: "partial", Next: text("next step"), Branch: "feature/planted-zq9x-text"}})
	mustAsk[proto.WriteResult](a, proto.OpRemember, proto.RememberArgs{Key: "deploy", Body: text("memory")})
	mustAsk[proto.RecallResult](a, proto.OpRecall, proto.RecallArgs{Text: text("recall")})
	mustAsk[proto.ShowResult](a, proto.OpShow, proto.ShowArgs{ID: is.ID, Full: true})
	mustAsk[proto.ListResult](a, proto.OpList, proto.ListArgs{})
	mustAsk[proto.FinishResult](a, proto.OpFinish, proto.FinishArgs{ID: is.ID, Reason: text("reason"), Waived: map[int]string{1: text("waiver")},
		Discovered: []proto.Discovered{{Title: text("discovered title")}}})

	if len(opArgs) != len(handlers)+len(adminHandlers) {
		t.Fatalf("opArgs lists %d ops and the server handles %d: list the new op, with its arguments", len(opArgs), len(handlers)+len(adminHandlers))
	}
	for op, zero := range opArgs {
		args := soaked(zero)
		for _, c := range []*conn{a, d} {
			// With the caller's trace context, whose tracestate is text
			// too. Whether the request is served does not matter here.
			traced(c, op, args, sampledTP, "vendor="+plantedText+"-state")
		}
	}
	// An op, an id and a trace context are the client's strings too.
	traced(a, plantedText+".op", struct{}{}, "00-"+plantedText+"-01", plantedText)
	traced(a, proto.OpShow, proto.ShowArgs{ID: plantedText}, "", "")
	a.hangUp()
	d.hangUp()
}

// holds reports whether s holds the secret, in any case.
func holds(s string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(plantedText)) || strings.Contains(strings.ToLower(s), "planted-zq9x")
}

// Nothing a span carries is issue text, a memory's text, a comment, a
// key, a DSN or a token (design §16.4): ids, codes and small numbers
// only. Requests with a recognizable secret in every field that takes
// text, served and refused, leave it in no span: not in a name, an
// attribute, a status, an event or the tracestate.
func TestSpansCarryNoText(t *testing.T) {
	t.Parallel()
	tel, m := spanned(t, telemetry.Config{})
	mu := adminWorldTel(t, Limits{Limits: store.Limits{}, WriteRate: 100000, WriteBurst: 100000, ControlWriteRate: 100000, ControlWriteBurst: 100000}, tel)
	m.ResetSpans()
	soakServer(t, mu)
	mu.tick(t.Context(), true)

	spans := m.Spans()
	names := map[string]int{}
	for _, sp := range spans {
		names[sp.Name]++
		bad := holds(sp.Name) || holds(sp.StatusNote) || holds(sp.TraceState) || sp.Events != 0 || sp.Links != 0
		for k, v := range sp.Attrs {
			bad = bad || holds(k) || holds(v)
		}
		if bad {
			t.Errorf("a span carries the client's text, or an event:\n%s", telemetry.SpanList([]telemetry.SpanData{sp}))
		}
	}
	// The run was not empty: requests that succeeded and that were
	// refused, an unknown op, the store's work and the reaper's.
	for _, name := range []string{"create", "update", "comment", "finish", "remember", "show", "key.add", "principal.whois", "unknown",
		"handshake", "store.write.attempt", "store.write.wait", "store.open", "reaper", "access.sweep", "keys_file.write"} {
		if names[name] == 0 {
			t.Errorf("no span named %s was recorded: the run did not cover it", name)
		}
	}
	if len(names) < len(opArgs) {
		t.Errorf("%d span names recorded, want one for each of the %d ops at least", len(names), len(opArgs))
	}
}

// The same, read where it leaves the machine: the bytes posted to the
// collector hold none of the text, and none of the names either, with
// telemetry: principals: at its default.
func TestExportedSpansCarryNoText(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var posted [][]byte
	col := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		posted = append(posted, b)
		mu.Unlock()
	}))
	t.Cleanup(col.Close)
	env := map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": col.URL + "/v1/traces", "OTEL_BSP_SCHEDULE_DELAY": "50"}
	tel, err := telemetry.New(t.Context(), telemetry.Config{Version: "v0.2.0", PerProject: true, Ops: Ops(), Getenv: func(k string) string { return env[k] },
		KeyFile: filepath.Join(t.TempDir(), "principals.key")})
	if err != nil {
		t.Fatal(err)
	}
	world := adminWorldTel(t, Limits{WriteRate: 100000, WriteBurst: 100000, ControlWriteRate: 100000, ControlWriteBurst: 100000}, tel)
	soakServer(t, world)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	mu.Lock()
	all := bytes.Join(posted, nil)
	mu.Unlock()
	// The export is there, and readable: it is not compressed.
	for _, want := range []string{"create", "store.write.attempt", "handshake", "starfixd", projA, "p_"} {
		if !bytes.Contains(all, []byte(want)) {
			t.Fatalf("the %d bytes exported do not hold %q: the test reads nothing", len(all), want)
		}
	}
	if holds(string(all)) {
		i := strings.Index(strings.ToLower(string(all)), "planted-zq9x")
		t.Errorf("the exported spans hold the client's text: …%q…", all[max(0, i-80):min(len(all), i+80)])
	}
	for _, name := range []string{"alice", "dana", "laptop", "desk"} {
		if bytes.Contains(all, []byte(name)) {
			t.Errorf("the exported spans hold %q with telemetry: principals: at its default", name)
		}
	}
}
