package configtest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"

	"azugo.io/azugo"
	"azugo.io/azugo/token"
	"azugo.io/azugo/user"
	corehttp "azugo.io/core/http"
	"github.com/go-quicktest/qt"
	"github.com/valyala/fasthttp"

	"github.com/gmb-lib/go-authbyte/permissions"
	pkerrors "github.com/gmb-lib/go-platform-kit/errors"

	"github.com/gmb-lib/go-configbyte/configserver"
	"github.com/gmb-lib/go-configbyte/configtest"
	"github.com/gmb-lib/go-configbyte/contract"
	"github.com/gmb-lib/go-configbyte/match"
)

const section = "orders"

var schema = contract.Schema(section)

// The lists the section carries, and how each is judged.
var kinds = map[string]match.Kind{
	"priorities": {Word: "priority", Required: []string{"label"}, Compared: []string{"label"}},
	"fields":     {Word: "field", Required: []string{"label", "fieldType"}, Fixed: []string{"fieldType", "unit"}, Compared: []string{"label"}},
}

var order = []string{"fields", "priorities"}

// owner is a faithful owner of the section, in memory — and, with a flaw set,
// one that breaks the contract in exactly one way.
type owner struct {
	mu   sync.Mutex
	held map[string]map[string][]match.Entry // tenant → list → items, in order
	flaw string
	// The owner's history of transports: tenant → the lines, oldest first.
	exports map[string][]string
	imports map[string][]configtest.Import
}

func newOwner(flaw string) *owner {
	return &owner{flaw: flaw, exports: map[string][]string{}, imports: map[string][]configtest.Import{}, held: map[string]map[string][]match.Entry{
		"tenant-live": {
			"priorities": {{"key": "high", "label": "High", "status": "active"}},
			"fields":     {{"key": "weight", "label": "Weight", "fieldType": "number", "unit": "kg", "status": "active"}},
		},
		"tenant-other": {"priorities": {}, "fields": {}},
	}}
}

func (o *owner) render(tenant string) []byte {
	lists := map[string]any{"schema": schema}
	for _, name := range order {
		entries := o.held[tenant][name]
		if entries == nil {
			entries = []match.Entry{}
		}
		if o.flaw == "identifiers" {
			for i := range entries {
				entries[i]["id"] = fmt.Sprint(i)
			}
		}
		lists[name] = entries
	}
	b, _ := json.Marshal(lists)

	return b
}

func (o *owner) token(tenant string, b []byte) string {
	if o.flaw == "frozen" {
		return contract.Token(schema, tenant, nil)
	}

	return contract.Token(schema, tenant, b)
}

func (o *owner) ConfigRead(_ context.Context, tenant string) (configserver.Read, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b := o.render(tenant)

	return configserver.Read{Section: b, Version: o.token(tenant, b)}, nil
}

func (o *owner) ConfigVersion(ctx context.Context, tenant string) (string, error) {
	r, err := o.ConfigRead(ctx, tenant)

	return r.Version, err
}

func (o *owner) ConfigExported(_ context.Context, e configserver.Export) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.flaw != "exportUnrecorded" {
		o.exports[e.Tenant] = append(o.exports[e.Tenant], e.PartHash)
	}

	return nil
}

func (o *owner) exported(tenant string) []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return append([]string{}, o.exports[tenant]...)
}

func (o *owner) imported(tenant string) []configtest.Import {
	o.mu.Lock()
	defer o.mu.Unlock()

	return append([]configtest.Import{}, o.imports[tenant]...)
}

func (o *owner) ConfigApply(_ context.Context, a configserver.Apply) (json.RawMessage, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.held[a.Tenant]; !ok {
		o.held[a.Tenant] = map[string][]match.Entry{}
	}

	doc, err := match.Open(a.Section, schema, order...)
	if err != nil {
		return nil, contract.Refusal(section, contract.ReasonBadDocument, err.Error())
	}
	lists := map[string][]contract.Item{}
	judged := map[string][]match.Judged{}
	for _, name := range order {
		have := map[string]match.Entry{}
		for _, e := range o.held[a.Tenant][name] {
			have[e.Key()] = e
		}
		lists[name], judged[name] = kinds[name].Match(doc[name], have)
	}
	if o.flaw == "applyDiffers" && !a.DryRun {
		for name := range lists {
			for i := range lists[name] {
				lists[name][i].Detail = ""
			}
		}
	}
	before := o.token(a.Tenant, o.render(a.Tenant))
	if !a.DryRun && a.Expected != before && o.flaw != "ignoresVersion" {
		return nil, contract.Refusal(section, contract.ReasonVersionMoved, "the configuration changed after the document was previewed")
	}

	refused := contract.AnyRefused(lists["priorities"], lists["fields"])
	write := (!a.DryRun && !refused) || (o.flaw == "previewWrites" && a.DryRun) || (o.flaw == "refusedWrites" && refused)
	if write {
		for _, name := range order {
			for _, j := range judged[name] {
				o.put(a.Tenant, name, j)
			}
		}
	}
	if !a.DryRun && !refused {
		switch o.flaw {
		case "importUnrecorded":
		case "importWithoutDocument":
			o.imports[a.Tenant] = append(o.imports[a.Tenant], configtest.Import{PartHash: a.PartHash})
		default:
			o.imports[a.Tenant] = append(o.imports[a.Tenant], configtest.Import{PartHash: a.PartHash, Document: a.Document})
		}
	}

	r := contract.NewReport(schema, a.DryRun, o.token(a.Tenant, o.render(a.Tenant)), lists)

	return json.Marshal(r)
}

func (o *owner) put(tenant, list string, j match.Judged) {
	e := match.Entry{}
	for k, v := range j.Entry {
		e[k] = v
	}
	e["status"] = j.Entry.Status()
	for i, have := range o.held[tenant][list] {
		if have.Key() == j.Key {
			o.held[tenant][list][i] = e

			return
		}
	}
	o.held[tenant][list] = append(o.held[tenant][list], e)
}

func (o *owner) holds(tenant, list, key, label string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, e := range o.held[tenant][list] {
		if e.Key() == key && (label == "" || match.Text(e["label"]) == label) {
			return true
		}
	}

	return false
}

var set = permissions.MustNew("orders", "Orders", []permissions.Permission{
	{Feature: "order", Act: "view", Description: "See orders", Class: permissions.Ordinary, Plane: permissions.Object},
}, configserver.Permissions())

// serve mounts the owner behind a stub authentication driven by headers.
func serve(t *testing.T, store configserver.Store, ready bool, edit func(o *configserver.Owner)) *azugo.TestClient {
	t.Helper()

	return mount(t, store, ready, edit, false)
}

// mount is serve, and with sectionBehindToken the section answer mounted behind
// the authentication like the other four — the mistake the ladder must catch.
func mount(t *testing.T, store configserver.Store, ready bool, edit func(o *configserver.Owner), sectionBehindToken bool) *azugo.TestClient {
	t.Helper()
	a := azugo.NewTestApp()
	a.AppName = "orders"
	a.RouterOptions().ErrorHandler = pkerrors.Handler(a.AppName, false)
	o := &configserver.Owner{
		Section: section, Audience: "svc:orders", ScopeKeys: []string{"orders"}, Domain: section, Gate: set.Gate(nil),
		Read:  permissions.Levels("orders", "read"),
		Write: configserver.ImportRule(set, permissions.Levels("orders", "admin")),
		Tenant: func(ctx *azugo.Context) (string, bool) {
			if tenant := ctx.User().ClaimValue("tenant"); tenant != "" {
				return tenant, true
			}
			ctx.Error(corehttp.ForbiddenError{})

			return "", false
		},
		Actor: func(ctx *azugo.Context) string { return ctx.User().ID() },
		Store: func(ctx *azugo.Context) (configserver.Store, bool) {
			if !ready {
				ctx.Error(pkerrors.NewProblem("err:orders:notReady", pkerrors.WithStatus(fasthttp.StatusServiceUnavailable)))

				return nil, false
			}

			return store, true
		},
	}
	if edit != nil {
		edit(o)
	}
	v1 := a.Group("/api/v1")
	v1.Use(func(next azugo.RequestHandler) azugo.RequestHandler {
		return func(ctx *azugo.Context) {
			scopes := string(ctx.Request().Header.Peek("X-Scopes"))
			if scopes == "" {
				ctx.Error(corehttp.UnauthorizedError{})

				return
			}
			sub := string(ctx.Request().Header.Peek("X-Sub"))
			if sub == "" {
				sub = "person-1"
			}
			claims := map[string]token.ClaimStrings{}
			if tenant := string(ctx.Request().Header.Peek("X-Tenant")); tenant != "" {
				claims["tenant"] = token.ClaimStrings{tenant}
			}
			ctx.SetUser(user.NewIdentity(sub, scopes, claims))
			next(ctx)
		}
	})
	open := a.Group("/api/v1")
	if sectionBehindToken {
		open = v1
	}
	qt.Assert(t, qt.IsNil(o.Mount(v1, open)))
	a.Start(t)
	t.Cleanup(a.Stop)

	return a.TestClient()
}

func as(c *azugo.TestClient, scopes, sub, tenant string) configtest.Caller {
	out := configtest.Caller{c.WithHeader("X-Scopes", scopes)}
	if sub != "" {
		out = append(out, c.WithHeader("X-Sub", sub))
	}
	if tenant != "" {
		out = append(out, c.WithHeader("X-Tenant", tenant))
	}

	return out
}

func gates(c *azugo.TestClient) configtest.Gates {
	return configtest.Gates{
		Service: configtest.Service{Client: c, Root: "/api/v1", Section: section, Domain: section,
			Audience: "svc:orders", ScopeKeys: []string{"orders"}},
		Anyone: as(c, "anything", "svc:coordinator", ""),
		Readers: []configtest.Caller{
			as(c, "orders/order:view", "", "tenant-a"),
			as(c, "other/thing:do", "", "tenant-a"),
			as(c, "orders:read", "svc:reader", "tenant-a"),
		},
		Unreadable: []configtest.Caller{as(c, "other:read other/order:view", "svc:other", "tenant-a")},
		Writers: []configtest.Caller{
			as(c, "orders/setup:import", "", "tenant-a"),
			as(c, "orders:admin", "svc:admin", "tenant-a"),
		},
		Refused: []configtest.Caller{
			as(c, "orders/order:view", "", "tenant-a"),
			as(c, "orders:admin orders:read", "", "tenant-a"),
			as(c, "orders:read orders:write", "svc:writer", "tenant-a"),
		},
	}
}

func live(c *azugo.TestClient, o *owner) configtest.Live {
	first := func(doc map[string]any, list string) map[string]any {
		l, _ := doc[list].([]any)
		m, _ := l[0].(map[string]any)

		return m
	}

	return configtest.Live{
		Service:     configtest.Service{Client: c, Root: "/api/v1", Section: section, Domain: section},
		Reader:      as(c, "orders/order:view", "", "tenant-live"),
		Admin:       as(c, "orders/setup:import", "", "tenant-live"),
		NonAdmin:    as(c, "orders/order:view orders:admin", "", "tenant-live"),
		OtherTenant: as(c, "orders/order:view", "", "tenant-other"),
		Parts:       order,
		Refusals: []configtest.Refusal{
			{
				Name: "a changed type on an existing key",
				Edit: func(d map[string]any) { first(d, "fields")["fieldType"] = "text" },
				Part: "fields", Key: "weight", Reason: contract.ReasonKeyConflict,
			},
			{
				Name: "a key twice",
				Edit: func(d map[string]any) {
					l, _ := d["priorities"].([]any)
					d["priorities"] = append(l, map[string]any{"key": "high", "label": "Again"})
				},
				Part: "priorities", Key: "high", Reason: contract.ReasonBadDocument,
			},
		},
		Beside: func(d map[string]any) {
			l, _ := d["priorities"].([]any)
			d["priorities"] = append(l, map[string]any{"key": "rush", "label": "Rush"})
		},
		Landed: func(testing.TB) bool { return o.holds("tenant-live", "priorities", "rush", "") },
		Change: configtest.Change{
			Edit:   func(d map[string]any) { first(d, "priorities")["label"] = "Urgent" },
			Part:   "priorities",
			Detail: "label",
			Landed: func(testing.TB) bool { return o.holds("tenant-live", "priorities", "high", "Urgent") },
		},
		Exports: func(testing.TB) []string { return o.exported("tenant-live") },
		Imports: func(testing.TB) []configtest.Import { return o.imported("tenant-live") },
	}
}

// A faithful owner passes every rung of both ladders.
func TestAFaithfulOwnerPassesTheLadder(t *testing.T) {
	gates(serve(t, nil, false, nil)).Run(t)

	o := newOwner("")
	live(serve(t, o, true, nil), o).Run(t)
}

// recorder is a test that records its failures instead of reporting them, so a
// test can prove the ladder fails an owner that breaks the contract.
type recorder struct {
	testing.TB
	mu     sync.Mutex
	failed []string
}

func (r *recorder) fail(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append(r.failed, msg)
}
func (r *recorder) Error(args ...any)                 { r.fail(fmt.Sprint(args...)) }
func (r *recorder) Errorf(format string, args ...any) { r.fail(fmt.Sprintf(format, args...)) }
func (r *recorder) Fail()                             { r.fail("failed") }
func (r *recorder) Fatal(args ...any)                 { r.fail(fmt.Sprint(args...)); runtime.Goexit() }
func (r *recorder) Fatalf(format string, args ...any) {
	r.fail(fmt.Sprintf(format, args...))
	runtime.Goexit()
}
func (r *recorder) FailNow() { r.fail("failed"); runtime.Goexit() }
func (r *recorder) Failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.failed) > 0
}

func ladderFails(t *testing.T, run func(tb testing.TB)) (bool, string) {
	t.Helper()
	r := &recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(r)
	}()
	<-done
	first := ""
	if len(r.failed) > 0 {
		first = r.failed[0]
	}

	return r.Failed(), first
}

// Each owner below breaks the contract in one way, and the ladder must say so.
func TestTheLadderFailsAnOwnerThatBreaksTheContract(t *testing.T) {
	for _, flaw := range []string{"identifiers", "frozen", "ignoresVersion", "previewWrites", "refusedWrites", "applyDiffers",
		"exportUnrecorded", "importUnrecorded", "importWithoutDocument"} {
		o := newOwner(flaw)
		c := serve(t, o, true, nil)
		failed, why := ladderFails(t, func(tb testing.TB) { live(c, o).Run(tb) })
		qt.Check(t, qt.IsTrue(failed), qt.Commentf("an owner whose flaw is %q passed the live ladder", flaw))
		t.Logf("%s: %s", flaw, brief(why))
	}

	for name, edit := range map[string]func(o *configserver.Owner){
		"a member may apply": func(o *configserver.Owner) {
			o.Write = configserver.Rule{Perms: []permissions.Perm{set.Declared("order", "view")}}
		},
		"any service may read": func(o *configserver.Owner) {
			o.Read = permissions.Level{Name: "anything", Holds: func(permissions.Scopes) bool { return true }}
		},
		"a person's rungs apply": func(o *configserver.Owner) {
			o.IsMachine = func(*azugo.Context) bool { return true }
		},
		"references it does not have": func(o *configserver.Owner) { o.RefersTo = []string{"stock"} },
		"another audience":            func(o *configserver.Owner) { o.Audience = "svc:stock" },
		"a scope key it does not say": func(o *configserver.Owner) { o.ScopeKeys = []string{"orders", "stock"} },
	} {
		c := serve(t, nil, false, edit)
		failed, why := ladderFails(t, func(tb testing.TB) { gates(c).Run(tb) })
		qt.Check(t, qt.IsTrue(failed), qt.Commentf("an owner where %s passed the gates ladder", name))
		t.Logf("%s: %s", name, brief(why))
	}

	behind := mount(t, nil, false, nil, true)
	failed, why := ladderFails(t, func(tb testing.TB) { gates(behind).Run(tb) })
	qt.Check(t, qt.IsTrue(failed), qt.Commentf("an owner whose section answer needs a token passed the gates ladder"))
	t.Logf("the section answer behind a token: %s", brief(why))

	o := newOwner("")
	c := serve(t, o, true, nil)
	l := live(c, o)
	l.Exports, l.Imports = nil, nil
	failed, why = ladderFails(t, func(tb testing.TB) { l.Run(tb) })
	qt.Check(t, qt.IsTrue(failed), qt.Commentf("a live ladder that cannot read the owner's history ran"))
	t.Logf("no history: %s", brief(why))
}

// brief is the first failure in one line, for the log.
func brief(why string) string {
	s := strings.Join(strings.Fields(why), " ")
	if len(s) > 160 {
		s = s[:160]
	}

	return s
}
