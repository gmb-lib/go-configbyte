package configserver_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/gmb-lib/go-authbyte/permissions/permissionstest"
	pkerrors "github.com/gmb-lib/go-platform-kit/errors"

	"github.com/gmb-lib/go-configbyte/configserver"
	"github.com/gmb-lib/go-configbyte/contract"
)

// store stands in for an owner's own data: one section per tenant, and every
// call recorded.
type store struct {
	mu      sync.Mutex
	report  string
	err     error
	reads   int
	applies []configserver.Apply
}

func (s *store) ConfigRead(_ context.Context, tenant string) (configserver.Read, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.err != nil {
		return configserver.Read{}, s.err
	}

	return configserver.Read{Section: json.RawMessage(`{"schema":"orders-config/1","tenant":"` + tenant + `"}`), Version: "sha256:" + tenant}, nil
}

func (s *store) ConfigVersion(_ context.Context, tenant string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.err != nil {
		return "", s.err
	}

	return "sha256:" + tenant, nil
}

func (s *store) ConfigApply(_ context.Context, a configserver.Apply) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applies = append(s.applies, a)
	if s.err != nil {
		return nil, s.err
	}
	if s.report != "" {
		return json.RawMessage(s.report), nil
	}

	return json.RawMessage(`{"schema":"orders-config/1","dryRun":` + boolText(a.DryRun) + `,"applied":` + boolText(!a.DryRun) +
		`,"refused":false,"version":"sha256:` + a.Tenant + `"}`), nil
}

func boolText(b bool) string {
	if b {
		return "true"
	}

	return "false"
}

func (s *store) applied() []configserver.Apply {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]configserver.Apply{}, s.applies...)
}

// The service's own list: one act of its own, beside the import act this
// package contributes.
var set = permissions.MustNew("orders", "Orders", []permissions.Permission{
	{Feature: "order", Act: "view", Description: "See orders", Class: permissions.Ordinary, Plane: permissions.Object},
}, configserver.Permissions())

// served is an owner mounted at /api/v1 behind a stub authentication driven by
// headers: X-Scopes (absent → 401), X-Sub (default a person) and X-Tenant.
type served struct {
	t       *testing.T
	client  *azugo.TestClient
	store   *store
	gate    *permissions.Gate
	refused []string
	told    []string
	ready   bool
}

func serve(t *testing.T, edit func(o *configserver.Owner, s *served)) *served {
	t.Helper()
	s := &served{t: t, store: &store{}, ready: true}
	a := azugo.NewTestApp()
	a.AppName = "orders"
	a.RouterOptions().ErrorHandler = pkerrors.Handler(a.AppName, false)

	s.gate = set.Gate(func(_ *azugo.Context, required string) { s.refused = append(s.refused, required) })
	o := &configserver.Owner{
		Section: "orders",
		Domain:  "orders",
		Gate:    s.gate,
		Read:    permissions.Levels("orders", "read"),
		Write:   configserver.ImportRule(set, permissions.Levels("orders", "admin")),
		Tenant: func(ctx *azugo.Context) (string, bool) {
			tenant := ctx.User().ClaimValue("tenant")
			if tenant == "" {
				ctx.Error(pkerrors.NewProblem("err:orders:forbidden", pkerrors.WithStatus(fasthttp.StatusForbidden)))

				return "", false
			}

			return tenant, true
		},
		Actor: func(ctx *azugo.Context) string { return "actor:" + ctx.User().ID() },
		Store: func(ctx *azugo.Context) (configserver.Store, bool) {
			if !s.ready {
				ctx.Error(pkerrors.NewProblem("err:orders:notReady", pkerrors.WithStatus(fasthttp.StatusServiceUnavailable)))

				return nil, false
			}

			return s.store, true
		},
		Applied: func(_ *azugo.Context, tenant string) { s.told = append(s.told, tenant) },
	}
	if edit != nil {
		edit(o, s)
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
	qt.Assert(t, qt.IsNil(o.Mount(v1)))

	a.Start(t)
	t.Cleanup(a.Stop)
	s.client = a.TestClient()

	return s
}

// caller is who a request is made as.
type caller struct {
	scopes, sub, tenant string
}

var (
	member        = caller{scopes: "orders/order:view", tenant: "tenant-a"}
	personNoBoxes = caller{scopes: "other/thing:do", tenant: "tenant-a"}
	administrator = caller{scopes: "orders/setup:import orders/order:view", tenant: "tenant-a"}
	personLevels  = caller{scopes: "orders:admin orders:read", tenant: "tenant-a"}
	machineRead   = caller{scopes: "orders:read", sub: "svc:reader", tenant: "tenant-a"}
	machineAdmin  = caller{scopes: "orders:admin", sub: "svc:admin", tenant: "tenant-a"}
	machineImport = caller{scopes: "orders/setup:import", sub: "svc:importer", tenant: "tenant-a"}
	machineWrite  = caller{scopes: "orders:write", sub: "svc:writer", tenant: "tenant-a"}
	machineOther  = caller{scopes: "other:read other/order:view", sub: "svc:other", tenant: "tenant-a"}
)

type answer struct {
	status int
	body   string
	etag   string
}

func (s *served) do(method, path string, c caller, body string, header ...string) answer {
	s.t.Helper()
	opts := []azugo.TestClientOption{}
	if c.scopes != "" {
		opts = append(opts, s.client.WithHeader("X-Scopes", c.scopes))
	}
	if c.sub != "" {
		opts = append(opts, s.client.WithHeader("X-Sub", c.sub))
	}
	if c.tenant != "" {
		opts = append(opts, s.client.WithHeader("X-Tenant", c.tenant))
	}
	for i := 0; i+1 < len(header); i += 2 {
		opts = append(opts, s.client.WithHeader(header[i], header[i+1]))
	}
	var (
		resp *fasthttp.Response
		err  error
	)
	if method == "GET" {
		resp, err = s.client.Get("/api/v1"+path, opts...)
	} else {
		resp, err = s.client.Post("/api/v1"+path, []byte(body), opts...)
	}
	qt.Assert(s.t, qt.IsNil(err))
	defer fasthttp.ReleaseResponse(resp)
	raw, _ := resp.BodyUncompressed()

	return answer{status: resp.StatusCode(), body: string(raw), etag: string(resp.Header.Peek("ETag"))}
}

const doc = `{"schema":"orders-config/1"}`

func TestEveryOperationFailsClosedWithoutAToken(t *testing.T) {
	s := serve(t, nil)
	for _, path := range []string{contract.PathConfig, contract.PathVersion, contract.PathSection} {
		qt.Check(t, qt.Equals(s.do("GET", path, caller{}, "").status, 401), qt.Commentf("GET %s", path))
	}
	for _, path := range []string{contract.PathPreview, contract.PathApply} {
		qt.Check(t, qt.Equals(s.do("POST", path, caller{}, doc).status, 401), qt.Commentf("POST %s", path))
	}
}

// Any person of the tenant reads the section — it is the vocabulary everybody
// works in — verbatim, with its version as the quoted ETag; the version read
// answers the same token alone.
func TestAPersonOfTheTenantReadsTheSectionWithItsVersionAsTheETag(t *testing.T) {
	s := serve(t, nil)
	for _, c := range []caller{member, personNoBoxes} {
		got := s.do("GET", contract.PathConfig, c, "")
		qt.Assert(t, qt.Equals(got.status, 200), qt.Commentf("%v: %s", c, got.body))
		qt.Check(t, qt.Equals(got.body, `{"schema":"orders-config/1","tenant":"tenant-a"}`))
		qt.Check(t, qt.Equals(got.etag, `"sha256:tenant-a"`))

		got = s.do("GET", contract.PathVersion, c, "")
		qt.Assert(t, qt.Equals(got.status, 200))
		qt.Check(t, qt.Equals(got.body, `{"version":"sha256:tenant-a"}`))
	}
}

// Without a tenant there is no section to read: the service's own refusal
// answers, and the store is never asked.
func TestAReadWithoutATenantIsTheServicesRefusal(t *testing.T) {
	s := serve(t, nil)
	for _, path := range []string{contract.PathConfig, contract.PathVersion} {
		got := s.do("GET", path, caller{scopes: "orders/order:view"}, "")
		qt.Check(t, qt.Equals(got.status, 403), qt.Commentf("%s: %s", path, got.body))
		qt.Check(t, qt.StringContains(got.body, "err:orders:forbidden"))
	}
	qt.Check(t, qt.Equals(s.store.reads, 0))
}

// A service acting as itself reads by the reading rung, or holding any
// permission this service declares; nothing of another service's opens it.
func TestAMachineReadsByTheReadingRungOrAnyPermissionOfTheService(t *testing.T) {
	s := serve(t, nil)
	for _, c := range []caller{machineRead, machineImport, {scopes: "orders/order:view", sub: "svc:viewer", tenant: "tenant-a"}} {
		for _, path := range []string{contract.PathConfig, contract.PathVersion} {
			qt.Check(t, qt.Equals(s.do("GET", path, c, "").status, 200), qt.Commentf("%v %s", c, path))
		}
	}
	// A rung is an exact match: the administering rung alone is not the
	// reading one.
	for _, c := range []caller{machineAdmin, machineWrite, machineOther} {
		for _, path := range []string{contract.PathConfig, contract.PathVersion} {
			qt.Check(t, qt.Equals(s.do("GET", path, c, "").status, 403), qt.Commentf("%v %s", c, path))
		}
	}
	qt.Check(t, qt.StringContains(strings.Join(s.refused, "|"), "orders:read or any orders/ permission"))
}

// Previewing and applying are the administrator's: a person by the import
// permission and never by a level; a service acting as itself by the
// administering rung or the permission.
func TestPreviewAndApplyAreTheAdministrators(t *testing.T) {
	s := serve(t, nil)
	for _, c := range []caller{administrator, machineAdmin, machineImport} {
		qt.Check(t, qt.Equals(s.do("POST", contract.PathPreview, c, doc).status, 200), qt.Commentf("preview %v", c))
		qt.Check(t, qt.Equals(s.do("POST", contract.PathApply, c, doc, "If-Match", `"sha256:x"`).status, 200), qt.Commentf("apply %v", c))
	}
	before := len(s.store.applied())
	for _, c := range []caller{member, personNoBoxes, personLevels, machineRead, machineWrite, machineOther} {
		qt.Check(t, qt.Equals(s.do("POST", contract.PathPreview, c, doc).status, 403), qt.Commentf("preview %v", c))
		qt.Check(t, qt.Equals(s.do("POST", contract.PathApply, c, doc, "If-Match", `"sha256:x"`).status, 403), qt.Commentf("apply %v", c))
	}
	qt.Check(t, qt.Equals(len(s.store.applied()), before), qt.Commentf("a refused caller never reaches the store"))
	qt.Check(t, qt.StringContains(strings.Join(s.refused, "|"), "orders:admin or orders/setup:import"))
}

// A writing apply names the version its preview answered. Without If-Match it
// is refused 428 before the store is asked; a strong or weak entity tag reaches
// the store as the version it names; a preview needs none.
func TestAnApplyNamesTheVersionItWasPreviewedAgainst(t *testing.T) {
	s := serve(t, nil)
	got := s.do("POST", contract.PathApply, administrator, doc)
	qt.Check(t, qt.Equals(got.status, 428), qt.Commentf("%s", got.body))
	qt.Check(t, qt.StringContains(got.body, `"code":"err:orders:config_version_required"`))
	qt.Check(t, qt.HasLen(s.store.applied(), 0), qt.Commentf("refused before the store"))

	for _, h := range []string{`"sha256:abc"`, `W/"sha256:abc"`} {
		got = s.do("POST", contract.PathApply, administrator, doc, "If-Match", h)
		qt.Check(t, qt.Equals(got.status, 200), qt.Commentf("If-Match %s: %s", h, got.body))
	}
	got = s.do("POST", contract.PathPreview, administrator, doc)
	qt.Check(t, qt.Equals(got.status, 200))

	calls := s.store.applied()
	qt.Assert(t, qt.HasLen(calls, 3))
	for _, a := range calls[:2] {
		qt.Check(t, qt.DeepEquals(a, configserver.Apply{Tenant: "tenant-a", Actor: "actor:person-1",
			Section: json.RawMessage(doc), DryRun: false, Expected: "sha256:abc"}))
	}
	qt.Check(t, qt.IsTrue(calls[2].DryRun))
	qt.Check(t, qt.Equals(calls[2].Expected, ""))
}

// A body that is not JSON is the same reason the store answers a document it
// cannot read, so it renders exactly like one; the store is never asked.
func TestADocumentThatIsNotJSONIsABadDocument(t *testing.T) {
	s := serve(t, nil)
	for _, path := range []string{contract.PathPreview, contract.PathApply} {
		got := s.do("POST", path, administrator, "not json", "If-Match", `"v"`)
		qt.Check(t, qt.Equals(got.status, 422), qt.Commentf("%s: %s", path, got.body))
		qt.Check(t, qt.StringContains(got.body, `"code":"err:orders:config_bad_document"`))
		qt.Check(t, qt.StringContains(got.body, `"title":"Malformed configuration document"`))
	}
	qt.Check(t, qt.HasLen(s.store.applied(), 0))
}

// A caller who passes the gate and finds no store gets the service's own
// not-ready answer — which is how a test with no database proves the gate let
// it through.
func TestPastTheGateWithNoStoreIsTheServicesNotReady(t *testing.T) {
	s := serve(t, nil)
	s.ready = false
	for _, path := range []string{contract.PathConfig, contract.PathVersion} {
		qt.Check(t, qt.Equals(s.do("GET", path, member, "").status, 503), qt.Commentf("%s", path))
	}
	qt.Check(t, qt.Equals(s.do("POST", contract.PathPreview, administrator, doc).status, 503))
	qt.Check(t, qt.Equals(s.do("POST", contract.PathApply, administrator, doc, "If-Match", `"v"`).status, 503))
	qt.Check(t, qt.Equals(s.do("POST", contract.PathApply, administrator, doc).status, 428), qt.Commentf("the version is asked for before the store"))
}

// An apply that wrote is told once, for its tenant; a preview, a refused
// document and an apply that failed are not.
func TestOnlyAnApplyThatWroteIsTold(t *testing.T) {
	s := serve(t, nil)
	s.do("POST", contract.PathPreview, administrator, doc)
	qt.Check(t, qt.HasLen(s.told, 0), qt.Commentf("a preview wrote nothing"))

	s.do("POST", contract.PathApply, administrator, doc, "If-Match", `"v"`)
	qt.Check(t, qt.DeepEquals(s.told, []string{"tenant-a"}))

	s.store.report = `{"schema":"orders-config/1","dryRun":false,"applied":false,"refused":true,"version":"v"}`
	got := s.do("POST", contract.PathApply, administrator, doc, "If-Match", `"v"`)
	qt.Check(t, qt.Equals(got.status, 200), qt.Commentf("a refusal inside the document is a report, not an error"))
	qt.Check(t, qt.Equals(got.body, s.store.report), qt.Commentf("the report travels verbatim"))

	// A preview is never recorded as a change, even from a store that says it
	// applied: a preview writes nothing by the contract.
	s.store.report = `{"schema":"orders-config/1","dryRun":true,"applied":true,"refused":false,"version":"v"}`
	s.do("POST", contract.PathPreview, administrator, doc)
	qt.Check(t, qt.DeepEquals(s.told, []string{"tenant-a"}), qt.Commentf("a preview was recorded as a change"))

	s.store.report = ""
	s.store.err = errors.New("boom")
	qt.Check(t, qt.Equals(s.do("POST", contract.PathApply, administrator, doc, "If-Match", `"v"`).status, 500))
	qt.Check(t, qt.DeepEquals(s.told, []string{"tenant-a"}))
}

// A store failure goes to the service's own renderer when it has one.
func TestAStoreFailureIsRenderedByTheService(t *testing.T) {
	var rendered []error
	s := serve(t, func(o *configserver.Owner, _ *served) {
		o.Fail = func(ctx *azugo.Context, err error) {
			rendered = append(rendered, err)
			ctx.Error(pkerrors.NewProblem("err:orders:unavailable", pkerrors.WithStatus(fasthttp.StatusBadGateway)))
		}
	})
	s.store.err = errors.New("the database went away")
	qt.Check(t, qt.Equals(s.do("GET", contract.PathConfig, member, "").status, 502))
	qt.Check(t, qt.Equals(s.do("GET", contract.PathVersion, member, "").status, 502))
	qt.Check(t, qt.Equals(s.do("POST", contract.PathPreview, administrator, doc).status, 502))
	qt.Check(t, qt.HasLen(rendered, 3))
}

// Which section answers here, and which sections it refers to, is answered to
// any authenticated caller with no tenant: it says nothing about anybody's
// configuration, and at deploy time there is no person to ask as.
func TestTheSectionAnswerNeedsNoTenantAndNoPerson(t *testing.T) {
	s := serve(t, nil)
	for _, c := range []caller{{scopes: "anything", sub: "svc:coordinator"}, {scopes: "anything"}} {
		got := s.do("GET", contract.PathSection, c, "")
		qt.Check(t, qt.Equals(got.status, 200), qt.Commentf("%v: %s", c, got.body))
		qt.Check(t, qt.Equals(got.body, `{"section":"orders","schema":"orders-config/1","refersTo":[]}`))
	}
	qt.Check(t, qt.Equals(s.store.reads, 0))

	refers := serve(t, func(o *configserver.Owner, _ *served) { o.RefersTo = []string{"stock"} })
	qt.Check(t, qt.Equals(refers.do("GET", contract.PathSection, caller{scopes: "x"}, "").body,
		`{"section":"orders","schema":"orders-config/1","refersTo":["stock"]}`))
}

type routes struct{ paths []string }

func (r *routes) Get(path string, _ azugo.RequestHandler)  { r.paths = append(r.paths, "GET "+path) }
func (r *routes) Post(path string, _ azugo.RequestHandler) { r.paths = append(r.paths, "POST "+path) }

// The five, and nothing else, at the root they are given.
func TestMountRegistersTheFiveOperations(t *testing.T) {
	r := &routes{}
	o := &configserver.Owner{Section: "orders", Domain: "orders", Gate: set.Gate(nil),
		Write:  configserver.ImportRule(set, permissions.Level{}),
		Tenant: func(*azugo.Context) (string, bool) { return "", false }, Actor: func(*azugo.Context) string { return "" },
		Store: func(*azugo.Context) (configserver.Store, bool) { return nil, false }}
	qt.Assert(t, qt.IsNil(o.Mount(r)))
	qt.Check(t, qt.DeepEquals(r.paths, []string{
		"GET /config", "GET /config/version", "POST /config/preview", "POST /config/apply", "GET /config/section",
	}))
}

func TestMountRefusesAnOwnerItCannotServe(t *testing.T) {
	good := func() *configserver.Owner {
		return &configserver.Owner{Section: "orders", Domain: "orders", Gate: set.Gate(nil),
			Write:  configserver.ImportRule(set, permissions.Level{}),
			Tenant: func(*azugo.Context) (string, bool) { return "", false }, Actor: func(*azugo.Context) string { return "" },
			Store: func(*azugo.Context) (configserver.Store, bool) { return nil, false }}
	}
	qt.Assert(t, qt.IsNil(good().Mount(&routes{})))

	for name, edit := range map[string]func(o *configserver.Owner){
		"a section name out of shape": func(o *configserver.Owner) { o.Section = "Orders" },
		"a reference to itself":       func(o *configserver.Owner) { o.RefersTo = []string{"orders"} },
		"no error domain":             func(o *configserver.Owner) { o.Domain = "" },
		"a domain that is a code":     func(o *configserver.Owner) { o.Domain = "err:orders" },
		"no gate":                     func(o *configserver.Owner) { o.Gate = nil },
		"nobody could apply":          func(o *configserver.Owner) { o.Write = configserver.Rule{} },
		"no tenant":                   func(o *configserver.Owner) { o.Tenant = nil },
		"no actor":                    func(o *configserver.Owner) { o.Actor = nil },
		"no store":                    func(o *configserver.Owner) { o.Store = nil },
	} {
		o := good()
		edit(o)
		r := &routes{}
		qt.Check(t, qt.IsNotNil(o.Mount(r)), qt.Commentf("%s", name))
		qt.Check(t, qt.HasLen(r.paths, 0), qt.Commentf("%s: nothing is mounted", name))
	}
}

// The import act is this package's contribution to the service's list: once the
// routes are mounted, the service's own test kit finds every declared
// permission checked and nothing checked that is not declared.
func TestTheImportPermissionIsContributedAndChecked(t *testing.T) {
	gate := set.Gate(nil)
	o := &configserver.Owner{Section: "orders", Domain: "orders", Gate: gate,
		Write:  configserver.ImportRule(set, permissions.Levels("orders", "admin")),
		Tenant: func(*azugo.Context) (string, bool) { return "", false }, Actor: func(*azugo.Context) string { return "" },
		Store: func(*azugo.Context) (configserver.Store, bool) { return nil, false }}
	qt.Assert(t, qt.IsNil(o.Mount(&routes{})))
	gate.OneOf(permissions.Level{}, func(*azugo.Context) {}, set.Declared("order", "view"))
	permissionstest.Check(t, set, gate)

	p, ok := set.Lookup(configserver.ImportFeature, configserver.ImportAct)
	qt.Assert(t, qt.IsTrue(ok))
	qt.Check(t, qt.Equals(p.Name("orders"), "orders/setup:import"))
	qt.Check(t, qt.Equals(p.Class, permissions.TenantConfiguration))
	qt.Check(t, qt.Equals(p.Plane, permissions.Tenant))

	without := permissions.MustNew("orders", "Orders", []permissions.Permission{
		{Feature: "order", Act: "view", Description: "See orders", Class: permissions.Ordinary, Plane: permissions.Object},
	})
	qt.Check(t, qt.PanicMatches(func() { configserver.Import(without) }, `.*orders/setup:import is checked but not declared`))
}

// A service that tells its own machines apart another way says how; the gates
// follow it.
func TestAServiceSaysHowItTellsAMachineFromAPerson(t *testing.T) {
	s := serve(t, func(o *configserver.Owner, _ *served) {
		o.IsMachine = func(ctx *azugo.Context) bool { return strings.HasPrefix(ctx.User().ID(), "client:") }
	})
	qt.Check(t, qt.Equals(s.do("GET", contract.PathConfig, caller{scopes: "orders:read", sub: "client:reader", tenant: "tenant-a"}, "").status, 200))
	qt.Check(t, qt.Equals(s.do("GET", contract.PathConfig, caller{scopes: "other:read", sub: "client:other", tenant: "tenant-a"}, "").status, 403))
	qt.Check(t, qt.Equals(s.do("POST", contract.PathPreview, caller{scopes: "orders:admin", sub: "client:admin", tenant: "tenant-a"}, doc).status, 200))
	qt.Check(t, qt.Equals(s.do("POST", contract.PathPreview, caller{scopes: "orders:admin", sub: "svc:admin", tenant: "tenant-a"}, doc).status, 403),
		qt.Commentf("under this service's rule a svc: subject is a person, who never passes by a rung"))
}
