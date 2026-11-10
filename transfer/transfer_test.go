package transfer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-quicktest/qt"

	"github.com/gmb-lib/go-authbyte/authclient"
	pkerrors "github.com/gmb-lib/go-platform-kit/errors"

	"github.com/gmb-lib/go-configbyte/configclient"
	"github.com/gmb-lib/go-configbyte/document"
	"github.com/gmb-lib/go-configbyte/owners"
	"github.com/gmb-lib/go-configbyte/transfer"
)

const (
	ordersSection = `{"schema":"orders-config/1","priorities":[{"key":"high","label":"High","status":"active"}]}`
	stockSection  = `{"schema":"stock-config/1","bins":[{"key":"a1","label":"Shelf A1","status":"active"}]}`
	rolesSection  = `{"schema":"rolebyte-config/1","roles":[{"key":"clerk","label":"Clerk","status":"active"}]}`

	testTenant = "01TENANT"
)

// call is one request an owner received.
type call struct {
	section, audience, scope, method, path string
	header                                 http.Header
	// nilHeader records that the caller was handed no header at all.
	nilHeader bool
	body      []byte
}

// answer is what a stub owner answers one call with.
type answer struct {
	status int
	header http.Header
	body   string
	err    error
}

// stub is one owner holding a section: it answers the read with the section and
// its version, a preview with whether it refuses, and an apply by moving its
// version — or by failing, when told to.
type stub struct {
	mu      sync.Mutex
	section string
	version string
	// refuse makes a preview report a refused item; exportRefused makes a read
	// sent as an export answer 403, the answer to someone who may not apply;
	// readRefused makes every read answer 403.
	refuse, exportRefused, readRefused bool
	// unreached makes every call produce no answer, with this error.
	unreached error
	// etag replaces the read's ETag header, quotes and all, when set.
	etag string
	// failApply fails the apply with this number (1 = the first), 0 for none,
	// answering failStatus (500 when unset) with failBody (a plain problem when
	// unset); onFail, when set, runs as it fails.
	failApply  int
	failStatus int
	failBody   string
	onFail     func()
	// loseApply writes the apply with this number and then loses its answer;
	// dropApply loses the apply with this number before it writes anything.
	// onLose, when set, runs as an answer is lost; unreadableAfterLoss makes
	// every read after a lost answer produce no answer either.
	loseApply, dropApply int
	onLose               func()
	unreadableAfterLoss  bool
	lost                 bool
	// gatewayAfterWrite writes the apply with this number and answers 504 with a
	// proxy's page; garbleAfterWrite writes it and answers 200 with a body that
	// is not a report; authorityRefuses refuses the apply with this number at
	// the token authority, before anything is sent.
	gatewayAfterWrite, garbleAfterWrite, authorityRefuses int
	applies                                               int
	// written is every section body an apply wrote, in order.
	written []string
}

var errLost = errors.New("read tcp: i/o timeout")

func (s *stub) loseAnswer() answer {
	s.lost = true
	if s.onLose != nil {
		s.onLose()
	}

	return answer{err: errLost}
}

func (s *stub) answer(c call) answer {
	s.mu.Lock()
	defer s.mu.Unlock()

	forbidden := answer{status: http.StatusForbidden, header: http.Header{"Content-Type": {"application/problem+json"}},
		body: `{"code":"err:owner:forbidden","title":"Forbidden","status":403}`}

	switch {
	case s.unreached != nil:
		return answer{err: s.unreached}
	case c.method == http.MethodGet && c.path == "/config":
		if s.lost && s.unreadableAfterLoss {
			return answer{err: errLost}
		}
		if s.readRefused || (c.header.Get("Config-Purpose") == "export" && s.exportRefused) {
			return forbidden
		}

		etag := `"` + s.version + `"`
		if s.etag != "" {
			etag = s.etag
		}

		return answer{status: http.StatusOK, header: http.Header{"Etag": {etag}}, body: s.section}
	case c.method == http.MethodPost && c.path == "/config/preview":
		return answer{status: http.StatusOK, body: fmt.Sprintf(`{"dryRun":true,"applied":false,"refused":%t,"version":%q}`, s.refuse, s.version)}
	case c.method == http.MethodPost && c.path == "/config/apply":
		s.applies++
		if s.applies == s.authorityRefuses {
			return answer{err: &authclient.Error{Status: http.StatusForbidden,
				Body: `{"code":"err:auth:scope_not_granted","title":"Forbidden","status":403}`}}
		}
		if s.applies == s.dropApply {
			return s.loseAnswer()
		}
		if s.applies == s.failApply {
			if s.onFail != nil {
				s.onFail()
			}
			status, body := s.failStatus, s.failBody
			if status == 0 {
				status = http.StatusInternalServerError
			}
			if body == "" {
				body = `{"code":"err:owner:internal","title":"Internal","status":500}`
			}

			return answer{status: status, header: http.Header{"Content-Type": {"application/problem+json"}}, body: body}
		}
		if c.header.Get("If-Match") != `"`+s.version+`"` {
			return answer{status: http.StatusPreconditionFailed, body: `{"code":"err:owner:config_version_moved","status":412}`}
		}
		s.written = append(s.written, string(c.body))
		s.section = string(c.body)
		s.version += "+"
		if s.applies == s.loseApply {
			return s.loseAnswer()
		}
		if s.applies == s.gatewayAfterWrite {
			return answer{status: http.StatusGatewayTimeout, body: "<html>504 Gateway Time-out</html>"}
		}
		if s.applies == s.garbleAfterWrite {
			return answer{status: http.StatusOK, body: "<html>ok</html>"}
		}

		return answer{status: http.StatusOK, body: fmt.Sprintf(`{"dryRun":false,"applied":true,"refused":false,"version":%q}`, s.version)}
	}

	return answer{status: http.StatusNotFound, body: `{"code":"err:owner:notFound","status":404}`}
}

// deployment is the three test owners, each a stub, behind one fake caller that
// records every call.
type deployment struct {
	orders, stock, roles *stub
	set                  *owners.Set

	// answerNothing makes the caller answer neither a response nor an error, as
	// a faulty host's caller might.
	answerNothing bool

	mu    sync.Mutex
	calls []call
	warns []string
}

func newDeployment(t *testing.T) *deployment {
	t.Helper()

	set, err := owners.Arrange([]*owners.Owner{
		{Address: "http://orders:8080/api/v1", Section: "orders", Schema: "orders-config/1", Audience: "svc:orders", ScopeKeys: []string{"orders"}},
		{Address: "http://stock:8080/api/v1", Section: "stock", Schema: "stock-config/1", Audience: "svc:stock", ScopeKeys: []string{"stock", "warehouse"}},
		{Address: "http://rolebyte:8080/api/v1", Section: "rolebyte", Schema: "rolebyte-config/1", Audience: "svc:rolebyte", ScopeKeys: []string{"membership"}},
	})
	qt.Assert(t, qt.IsNil(err))

	return &deployment{
		orders: &stub{section: ordersSection, version: "sha256:orders"},
		stock:  &stub{section: stockSection, version: "sha256:stock"},
		roles:  &stub{section: rolesSection, version: "sha256:roles"},
		set:    set,
	}
}

func (d *deployment) Call(ctx context.Context, o *owners.Owner, scope, method, path string, header http.Header, body []byte) (*configclient.Response, error) {
	c := call{section: o.Section, audience: o.Audience, scope: scope, method: method, path: path,
		header: header.Clone(), nilHeader: header == nil, body: append([]byte(nil), body...)}
	if c.header == nil {
		c.header = http.Header{}
	}
	d.mu.Lock()
	d.calls = append(d.calls, c)
	d.mu.Unlock()

	// A real client gives up on a request whose context has ended.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.answerNothing {
		return nil, nil
	}

	s := map[string]*stub{"orders": d.orders, "stock": d.stock, "rolebyte": d.roles}[o.Section]
	a := s.answer(c)
	if a.err != nil {
		return nil, a.err
	}
	h := a.header
	if h == nil {
		h = http.Header{}
	}

	return &configclient.Response{Status: a.status, Header: h, Body: []byte(a.body)}, nil
}

func (d *deployment) transfer() *transfer.Transfer {
	return &transfer.Transfer{Owners: d.set, Warn: func(msg string) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.warns = append(d.warns, msg)
	}}
}

func (d *deployment) recorded() []call {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]call(nil), d.calls...)
}

func callsTo(calls []call, method, path string) []call {
	var out []call
	for _, c := range calls {
		if c.method == method && c.path == path {
			out = append(out, c)
		}
	}

	return out
}

// everyone holds boxes under all three owners' keys.
func everyone() transfer.Person {
	return transfer.Person{Tenant: testTenant,
		Scopes: []string{"orders/setup:import", "stock/setup:import", "warehouse:admin", "membership:admin"}}
}

func importDoc(t *testing.T, sections map[string]string, expect map[string]string) []byte {
	t.Helper()

	doc := map[string]any{"gmbConfig": "1.0", "sections": map[string]json.RawMessage{}}
	for k, v := range sections {
		doc["sections"].(map[string]json.RawMessage)[k] = json.RawMessage(v)
	}
	if expect != nil {
		doc["expect"] = expect
	}
	b, err := json.Marshal(doc)
	qt.Assert(t, qt.IsNil(err))

	return b
}

// problemOf renders err through the package's helper and answers the problem.
func problemOf(t *testing.T, err error) *pkerrors.Problem {
	t.Helper()

	var p *pkerrors.Problem
	qt.Assert(t, qt.IsTrue(errors.As(transfer.Problem(err, "host"), &p)), qt.Commentf("%v", err))

	return p
}

// exportDay is half past one at night in Riga on 10 November: still the 9th in
// UTC, the day the file is named by.
var exportDay = time.Date(2026, 11, 10, 1, 30, 0, 0, time.FixedZone("EET", 2*60*60))

// The export reads every owner's section as an export, in import order and on
// the person's own scopes under each owner's keys, so each owner can record who
// took its part out; it hashes them and names the file by the day in UTC.
func TestTheExportReadsEverySectionAsAnExportOnThePersonsOwnScopes(t *testing.T) {
	d := newDeployment(t)

	file, err := d.transfer().Export(context.Background(), everyone(), d, exportDay)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(file.Name, "configuration-2026-11-09.json"))

	var env document.Document
	qt.Assert(t, qt.IsNil(json.Unmarshal(file.Body, &env)))
	qt.Check(t, qt.Equals(env.GmbConfig, "1.0"))
	qt.Check(t, qt.Equals(env.Tenant.ID, testTenant))
	qt.Check(t, qt.Equals(len(env.Sections), 3))
	qt.Check(t, qt.Equals(env.ContentHash, document.ContentHash(env.Sections)))
	qt.Check(t, qt.Equals(file.Hash, env.ContentHash))
	qt.Check(t, qt.IsTrue(strings.Contains(string(file.Body), "\n  ")), qt.Commentf("the document is indented to be read"))

	calls := d.recorded()
	qt.Assert(t, qt.HasLen(calls, 3))
	want := []struct{ section, audience, scope string }{
		{"orders", "svc:orders", "orders/setup:import"},
		{"stock", "svc:stock", "stock/setup:import warehouse:admin"},
		{"rolebyte", "svc:rolebyte", "membership:admin"},
	}
	for i, w := range want {
		qt.Check(t, qt.Equals(calls[i].section, w.section))
		qt.Check(t, qt.Equals(calls[i].audience, w.audience))
		qt.Check(t, qt.Equals(calls[i].scope, w.scope))
		qt.Check(t, qt.Equals(calls[i].path, "/config"))
		qt.Check(t, qt.Equals(calls[i].header.Get("Config-Purpose"), "export"))
	}
}

// An owner the person holds nothing of is left out of the export and never
// asked; the rest are carried.
func TestTheExportLeavesOutAnOwnerThePersonHoldsNothingOf(t *testing.T) {
	d := newDeployment(t)
	p := transfer.Person{Tenant: testTenant, Scopes: []string{"orders/setup:import", "authority/order:view"}}

	file, err := d.transfer().Export(context.Background(), p, d, exportDay)
	qt.Assert(t, qt.IsNil(err))

	var env document.Document
	qt.Assert(t, qt.IsNil(json.Unmarshal(file.Body, &env)))
	_, has := env.Sections["orders"]
	qt.Check(t, qt.IsTrue(has))
	qt.Check(t, qt.Equals(len(env.Sections), 1))
	for _, c := range d.recorded() {
		qt.Check(t, qt.Equals(c.section, "orders"))
	}
}

// An owner that refuses the export — the person may read its section but not
// apply it — ends the export with the owner's own refusal: half a document
// would be worse than none.
func TestAnOwnerRefusingTheExportEndsItWithItsOwnRefusal(t *testing.T) {
	d := newDeployment(t)
	d.stock.exportRefused = true

	file, err := d.transfer().Export(context.Background(), everyone(), d, exportDay)
	qt.Check(t, qt.IsNil(file))
	var refused *transfer.OwnerRefused
	qt.Assert(t, qt.ErrorAs(err, &refused))
	qt.Check(t, qt.Equals(refused.Section, "stock"))
	qt.Check(t, qt.Equals(refused.Status, http.StatusForbidden))
	qt.Check(t, qt.HasLen(callsTo(d.recorded(), http.MethodGet, "/config"), 2), qt.Commentf("the role register is never asked"))

	p := problemOf(t, err)
	qt.Check(t, qt.Equals(p.Code, "err:owner:forbidden"))
	qt.Check(t, qt.Equals(p.Status, http.StatusForbidden))
}

// Someone who holds nothing any owner accepts is refused before any owner is
// called — the export and the import alike.
func TestNothingHeldAnywhereIsRefusedBeforeAnyOwner(t *testing.T) {
	d := newDeployment(t)
	p := transfer.Person{Tenant: testTenant, Scopes: []string{"authority/order:view", "billing/invoice:view"}}

	_, err := d.transfer().Export(context.Background(), p, d, exportDay)
	qt.Check(t, qt.ErrorIs(err, transfer.ErrNothingHeld))

	_, err = d.transfer().Import(context.Background(), p, d, importDoc(t, map[string]string{"orders": ordersSection}, nil), true)
	qt.Check(t, qt.ErrorIs(err, transfer.ErrNothingHeld))

	qt.Check(t, qt.HasLen(d.recorded(), 0))

	problem := problemOf(t, err)
	qt.Check(t, qt.Equals(problem.Code, "err:request:forbidden"))
	qt.Check(t, qt.Equals(problem.Status, http.StatusForbidden))
	qt.Check(t, qt.Equals(problem.Detail, "this account holds nothing any configuration owner here accepts"))
}

// A dry run fans every section out to its owner's preview, verbatim, on the
// person's own scopes, reports a section nobody here owns and one whose owner
// the person holds nothing of as skipped, and writes nothing.
func TestADryRunPreviewsEveryOwnedSectionAndSkipsTheRest(t *testing.T) {
	d := newDeployment(t)
	p := transfer.Person{Tenant: testTenant, Scopes: []string{"orders/setup:import", "stock/setup:import"}}

	doc := importDoc(t, map[string]string{"orders": ordersSection, "stock": stockSection, "rolebyte": rolesSection,
		"billing": `{"schema":"billing-config/1"}`}, nil)
	imp, err := d.transfer().Import(context.Background(), p, d, doc, true)
	qt.Assert(t, qt.IsNil(err))

	a := imp.Answer
	qt.Check(t, qt.IsTrue(a.DryRun))
	qt.Check(t, qt.Equals(a.Outcome, document.Previewed))
	qt.Check(t, qt.Equals(a.Sections["orders"].Status, document.SectionPreviewed))
	qt.Check(t, qt.Equals(a.Sections["orders"].Version, "sha256:orders"))
	qt.Check(t, qt.Equals(a.Sections["stock"].Status, document.SectionPreviewed))
	qt.Check(t, qt.Equals(a.Sections["rolebyte"].Status, document.SectionSkipped))
	qt.Check(t, qt.IsTrue(strings.Contains(a.Sections["rolebyte"].Detail, "holds nothing the owner")))
	qt.Check(t, qt.Equals(a.Sections["billing"].Status, document.SectionSkipped))
	qt.Check(t, qt.IsTrue(strings.Contains(a.Sections["billing"].Detail, "no service in this deployment")))

	calls := d.recorded()
	previews := callsTo(calls, http.MethodPost, "/config/preview")
	qt.Assert(t, qt.HasLen(previews, 2))
	qt.Check(t, qt.Equals(previews[0].section, "orders"))
	qt.Check(t, qt.Equals(string(previews[0].body), ordersSection))
	qt.Check(t, qt.Equals(previews[1].section, "stock"))
	qt.Check(t, qt.Equals(previews[1].scope, "stock/setup:import"))
	qt.Check(t, qt.HasLen(callsTo(calls, http.MethodPost, "/config/apply"), 0))
}

// A dry run with one refused section answers refused for the document; one
// whose owner cannot be asked answers failed.
func TestADryRunAnswersRefusedOrFailedForTheWholeDocument(t *testing.T) {
	d := newDeployment(t)
	d.stock.refuse = true
	imp, err := d.transfer().Import(context.Background(), everyone(), d,
		importDoc(t, map[string]string{"orders": ordersSection, "stock": stockSection}, nil), true)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(imp.Answer.Outcome, document.Refused))
	qt.Check(t, qt.Equals(imp.Answer.Sections["stock"].Status, document.SectionRefused))

	d = newDeployment(t)
	d.stock.unreached = errors.New("dial tcp: connection refused")
	imp, err = d.transfer().Import(context.Background(), everyone(), d,
		importDoc(t, map[string]string{"orders": ordersSection, "stock": stockSection}, nil), true)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(imp.Answer.Outcome, document.Failed))
	qt.Check(t, qt.Equals(imp.Answer.Sections["stock"].Status, document.SectionFailed))
	qt.Check(t, qt.JSONEquals([]byte(imp.Answer.Sections["stock"].Problem), map[string]any{"code": "err:upstream:unavailable", "status": 502}))
	qt.Check(t, qt.HasLen(d.warns, 1))
}

// An apply names the version each section's dry run answered; without one it is
// refused 428 before any owner is asked anything.
func TestAnApplyWithoutTheReviewedVersionsIsRefusedBeforeAnyOwner(t *testing.T) {
	d := newDeployment(t)

	_, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection},
		map[string]string{"orders": "sha256:orders"}), false)
	var required *transfer.VersionRequired
	qt.Assert(t, qt.ErrorAs(err, &required))
	qt.Check(t, qt.Equals(required.Section, "stock"))
	qt.Check(t, qt.HasLen(d.recorded(), 0))

	p := problemOf(t, err)
	qt.Check(t, qt.Equals(p.Code, document.CodeVersionRequired))
	qt.Check(t, qt.Equals(p.Status, http.StatusPreconditionRequired))
	qt.Check(t, qt.IsTrue(strings.HasPrefix(p.Detail, "the stock section names no version")))
}

// A section that moved after its dry run stops the import: 412, nothing
// previewed and nothing written.
func TestAnApplyStopsWhenASectionMovedAfterItsDryRun(t *testing.T) {
	d := newDeployment(t)

	_, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection},
		map[string]string{"orders": "sha256:orders", "stock": "sha256:older"}), false)
	var moved *transfer.VersionMoved
	qt.Assert(t, qt.ErrorAs(err, &moved))
	qt.Check(t, qt.Equals(moved.Section, "stock"))

	calls := d.recorded()
	qt.Check(t, qt.HasLen(callsTo(calls, http.MethodPost, "/config/preview"), 0))
	qt.Check(t, qt.HasLen(callsTo(calls, http.MethodPost, "/config/apply"), 0))

	p := problemOf(t, err)
	qt.Check(t, qt.Equals(p.Code, document.CodeVersionMoved))
	qt.Check(t, qt.Equals(p.Status, http.StatusPreconditionFailed))
}

// A version is compared as the ETag with its quotes removed and nothing else, so
// a weak tag never matches the version a dry run answered.
func TestAWeakTagIsNeverTheVersionADryRunAnswered(t *testing.T) {
	d := newDeployment(t)
	d.orders.etag = `W/"sha256:orders"`

	_, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection}, map[string]string{"orders": "sha256:orders"}), false)
	var moved *transfer.VersionMoved
	qt.Check(t, qt.ErrorAs(err, &moved))
}

// An owner refusing the read an apply starts with ends the apply with its own
// refusal, before anything is previewed or written.
func TestAnApplyWhoseReadIsRefusedRelaysTheOwnersRefusal(t *testing.T) {
	d := newDeployment(t)
	d.stock.readRefused = true

	_, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection},
		map[string]string{"orders": "sha256:orders", "stock": "sha256:stock"}), false)
	var refused *transfer.OwnerRefused
	qt.Assert(t, qt.ErrorAs(err, &refused))
	qt.Check(t, qt.Equals(refused.Section, "stock"))
	qt.Check(t, qt.HasLen(callsTo(d.recorded(), http.MethodPost, "/config/apply"), 0))

	p := problemOf(t, err)
	qt.Check(t, qt.Equals(p.Code, "err:owner:forbidden"))
	qt.Check(t, qt.Equals(p.Status, http.StatusForbidden))
}

// One refused section and the document writes nothing: the refused one is
// reported refused, the clean ones withheld.
func TestOneRefusedSectionWritesNothing(t *testing.T) {
	d := newDeployment(t)
	d.stock.refuse = true

	imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection},
		map[string]string{"orders": "sha256:orders", "stock": "sha256:stock"}), false)
	qt.Assert(t, qt.IsNil(err))

	a := imp.Answer
	qt.Check(t, qt.Equals(a.Outcome, document.Refused))
	qt.Check(t, qt.Equals(a.Sections["stock"].Status, document.SectionRefused))
	qt.Check(t, qt.Equals(a.Sections["orders"].Status, document.SectionWithheld))
	qt.Check(t, qt.HasLen(callsTo(d.recorded(), http.MethodPost, "/config/apply"), 0))
}

// A clean document lands whole: every section written in the owners' order —
// the role register last — each under the version its dry run answered and
// naming the document it came in, so each owner's record of the apply names
// the file.
func TestACleanDocumentLandsInOrderNamingTheDocument(t *testing.T) {
	d := newDeployment(t)

	sections := map[string]string{"rolebyte": rolesSection, "stock": stockSection, "orders": ordersSection}
	raw := map[string]json.RawMessage{}
	for k, v := range sections {
		raw[k] = json.RawMessage(v)
	}
	documentHash := document.ContentHash(raw)

	imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t, sections,
		map[string]string{"orders": "sha256:orders", "stock": "sha256:stock", "rolebyte": "sha256:roles"}), false)
	qt.Assert(t, qt.IsNil(err))

	a := imp.Answer
	qt.Check(t, qt.Equals(a.Outcome, document.Applied))
	qt.Check(t, qt.IsFalse(a.DryRun))
	qt.Check(t, qt.Equals(a.Sections["orders"].Version, "sha256:orders+"))
	qt.Check(t, qt.Equals(imp.Hash, documentHash))

	applies := callsTo(d.recorded(), http.MethodPost, "/config/apply")
	qt.Assert(t, qt.HasLen(applies, 3))
	for i, section := range []string{"orders", "stock", "rolebyte"} {
		qt.Check(t, qt.Equals(applies[i].section, section))
		qt.Check(t, qt.Equals(applies[i].header.Get("Config-Document"), documentHash))
	}
	qt.Check(t, qt.Equals(applies[0].header.Get("If-Match"), `"sha256:orders"`))
	qt.Check(t, qt.Equals(applies[2].header.Get("If-Match"), `"sha256:roles"`))
	qt.Check(t, qt.IsTrue(strings.HasPrefix(documentHash, "sha256:")))
}

// A later section failing after earlier ones were written restores each
// written one to what the import read before touching it, under the version
// its own apply answered, naming the same document; the outcome is rolledBack.
func TestAFailedLaterSectionRestoresTheWrittenOnes(t *testing.T) {
	d := newDeployment(t)
	d.roles.failApply = 1

	edited := `{"schema":"orders-config/1","priorities":[]}`
	imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": edited, "stock": stockSection, "rolebyte": rolesSection},
		map[string]string{"orders": "sha256:orders", "stock": "sha256:stock", "rolebyte": "sha256:roles"}), false)
	qt.Assert(t, qt.IsNil(err))

	a := imp.Answer
	qt.Check(t, qt.Equals(a.Outcome, document.RolledBack))
	qt.Check(t, qt.Equals(a.Sections["orders"].Status, document.SectionRolledBack))
	qt.Check(t, qt.Equals(a.Sections["stock"].Status, document.SectionRolledBack))
	qt.Check(t, qt.Equals(a.Sections["rolebyte"].Status, document.SectionFailed))

	// Written, then written back: the section ends as it began.
	qt.Check(t, qt.DeepEquals(d.orders.written, []string{edited, ordersSection}))
	qt.Check(t, qt.Equals(d.orders.section, ordersSection))

	applies := callsTo(d.recorded(), http.MethodPost, "/config/apply")
	// orders, stock, rolebyte (failed), then stock and orders restored, newest first.
	qt.Assert(t, qt.HasLen(applies, 5))
	qt.Check(t, qt.Equals(applies[3].section, "stock"))
	qt.Check(t, qt.Equals(applies[3].header.Get("If-Match"), `"sha256:stock+"`))
	qt.Check(t, qt.Equals(applies[4].section, "orders"))
	qt.Check(t, qt.Equals(applies[4].header.Get("Config-Document"), applies[0].header.Get("Config-Document")))

	qt.Check(t, qt.DeepEquals(d.warns, []string{"configuration import compensated after a failed section (tenant " + testTenant + ", rolledBack)"}))
}

// A restore that itself fails leaves the document partial, and the answer says
// which section holds what.
func TestARestoreThatFailsLeavesTheDocumentPartial(t *testing.T) {
	d := newDeployment(t)
	d.stock.failApply = 1
	d.orders.failApply = 2 // the restore

	imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection},
		map[string]string{"orders": "sha256:orders", "stock": "sha256:stock"}), false)
	qt.Assert(t, qt.IsNil(err))

	a := imp.Answer
	qt.Check(t, qt.Equals(a.Outcome, document.Partial))
	qt.Check(t, qt.IsTrue(strings.Contains(a.Sections["orders"].Detail, "could not be restored")))
	qt.Check(t, qt.JSONEquals([]byte(a.Sections["orders"].Problem), map[string]any{"code": "err:owner:internal", "title": "Internal", "status": 500}))
}

// The first section failing leaves nothing to restore: the outcome is failed,
// and every later section is withheld, never written.
func TestTheFirstSectionFailingWritesNothingElse(t *testing.T) {
	d := newDeployment(t)
	d.orders.failApply = 1

	imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection},
		map[string]string{"orders": "sha256:orders", "stock": "sha256:stock"}), false)
	qt.Assert(t, qt.IsNil(err))

	a := imp.Answer
	qt.Check(t, qt.Equals(a.Outcome, document.Failed))
	qt.Check(t, qt.Equals(a.Sections["stock"].Status, document.SectionWithheld))
	qt.Check(t, qt.HasLen(d.stock.written, 0))
}

// An envelope that cannot honestly be interpreted is refused as a bad document
// before any section reaches an owner.
func TestABadEnvelopeIsRefusedBeforeAnyOwner(t *testing.T) {
	d := newDeployment(t)

	for _, doc := range []string{`not json`, `{"gmbConfig":"9.0","sections":{}}`, `{"gmbConfig":"1.0"}`} {
		_, err := d.transfer().Import(context.Background(), everyone(), d, []byte(doc), true)
		var invalid *document.Invalid
		qt.Check(t, qt.ErrorAs(err, &invalid), qt.Commentf("%s", doc))

		p := problemOf(t, err)
		qt.Check(t, qt.Equals(p.Code, document.CodeBadDocument))
		qt.Check(t, qt.Equals(p.Status, http.StatusUnprocessableEntity))
	}
	qt.Check(t, qt.HasLen(d.recorded(), 0))
}

// The edited flag says honestly whether the sections are the exact export the
// file names, and never stops the import.
func TestTheEditedFlagIsHonestAndNeverAGate(t *testing.T) {
	d := newDeployment(t)

	doc := map[string]any{"gmbConfig": "1.0", "contentHash": document.ContentHash(map[string]json.RawMessage{"orders": json.RawMessage(ordersSection)}),
		"sections": map[string]json.RawMessage{"orders": json.RawMessage(`{"schema":"orders-config/1","priorities":[]}`)}}
	b, err := json.Marshal(doc)
	qt.Assert(t, qt.IsNil(err))

	imp, err := d.transfer().Import(context.Background(), everyone(), d, b, true)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsNotNil(imp.Answer.DocumentEdited))
	qt.Check(t, qt.IsTrue(*imp.Answer.DocumentEdited))
	qt.Check(t, qt.Equals(imp.Answer.Outcome, document.Previewed))
}

// The order the transfer writes in is the one the owners were arranged in — a
// section after the sections it refers to — not the order of the document.
func TestTheImportFollowsTheReferencesTheOwnersWereArrangedBy(t *testing.T) {
	d := newDeployment(t)
	set, err := owners.Arrange([]*owners.Owner{
		{Address: "http://orders:8080/api/v1", Section: "orders", Audience: "svc:orders", ScopeKeys: []string{"orders"}, RefersTo: []string{"stock"}},
		{Address: "http://stock:8080/api/v1", Section: "stock", Audience: "svc:stock", ScopeKeys: []string{"stock", "warehouse"}},
	})
	qt.Assert(t, qt.IsNil(err))
	d.set = set

	_, err = d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection},
		map[string]string{"orders": "sha256:orders", "stock": "sha256:stock"}), false)
	qt.Assert(t, qt.IsNil(err))

	applies := callsTo(d.recorded(), http.MethodPost, "/config/apply")
	qt.Assert(t, qt.HasLen(applies, 2))
	qt.Check(t, qt.Equals(applies[0].section, "stock"))
	qt.Check(t, qt.Equals(applies[1].section, "orders"))
}

// A call that produced no owner's answer ends the export. The authority
// refusing the token is an answer somebody decided, relayed with its own code;
// a hop that failed is the gateway failure. Each is told to Warn in its own
// words.
func TestAnUnreachedOwnerTellsTheAuthoritysRefusalFromAFailedHop(t *testing.T) {
	d := newDeployment(t)
	d.stock.unreached = &authclient.Error{Status: http.StatusForbidden,
		Body: `{"code":"err:auth:scope_not_granted","title":"Forbidden","status":403}`}

	_, err := d.transfer().Export(context.Background(), everyone(), d, exportDay)
	var unreached *transfer.Unreached
	qt.Assert(t, qt.ErrorAs(err, &unreached))
	qt.Check(t, qt.Equals(unreached.Section, "stock"))
	p := problemOf(t, err)
	qt.Check(t, qt.Equals(p.Code, "err:auth:scope_not_granted"))
	qt.Check(t, qt.Equals(p.Status, http.StatusForbidden))
	qt.Check(t, qt.DeepEquals(d.warns, []string{"configuration export from the stock owner refused by the authority"}))

	d = newDeployment(t)
	d.stock.unreached = errors.New("dial tcp: connection refused")
	_, err = d.transfer().Export(context.Background(), everyone(), d, exportDay)
	p = problemOf(t, err)
	qt.Check(t, qt.Equals(p.Code, "err:upstream:unavailable"))
	qt.Check(t, qt.Equals(p.Status, http.StatusBadGateway))
	qt.Check(t, qt.DeepEquals(d.warns, []string{"configuration export from the stock owner failed: dial tcp: connection refused"}))

	// The same holds for the read an apply starts with.
	d = newDeployment(t)
	d.stock.unreached = errors.New("context deadline exceeded")
	_, err = d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"stock": stockSection}, map[string]string{"stock": "sha256:stock"}), false)
	qt.Assert(t, qt.ErrorAs(err, &unreached))
	qt.Check(t, qt.Equals(unreached.What, "configuration read from the stock owner"))
}

// A refusal whose body is no problem keeps its status and says only that the
// request was refused; a server error without one is the gateway failure.
func TestARefusalWithoutAProblemBodyKeepsOnlyItsStatus(t *testing.T) {
	p := problemOf(t, &transfer.OwnerRefused{Section: "stock", Status: http.StatusConflict, Body: []byte("<html>no</html>")})
	qt.Check(t, qt.Equals(p.Code, "err:upstream:refused"))
	qt.Check(t, qt.Equals(p.Status, http.StatusConflict))

	p = problemOf(t, &transfer.OwnerRefused{Section: "stock", Status: http.StatusServiceUnavailable})
	qt.Check(t, qt.Equals(p.Code, "err:upstream:unavailable"))
	qt.Check(t, qt.Equals(p.Status, http.StatusBadGateway))
}

// An error that is none of this package's is answered as it came, and an
// owner's refusal keeps its body out of the error's message.
func TestProblemLeavesOtherErrorsAsTheyCame(t *testing.T) {
	other := errors.New("something else")
	qt.Check(t, qt.Equals(transfer.Problem(other, "host"), other))

	refused := &transfer.OwnerRefused{Section: "stock", Status: http.StatusForbidden, Body: []byte(`{"detail":"a secret"}`)}
	qt.Check(t, qt.IsFalse(strings.Contains(refused.Error(), "secret")))
}

// The sections are listed in the order an import writes them, each with what
// its owner said at start, and a section that refers to nothing says so with
// an empty list, never null.
func TestTheSectionsAreListedInImportOrder(t *testing.T) {
	d := newDeployment(t)

	b, err := json.Marshal(d.transfer().Sections())
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.JSONEquals(b, []map[string]any{
		{"section": "orders", "schema": "orders-config/1", "refersTo": []any{}},
		{"section": "stock", "schema": "stock-config/1", "refersTo": []any{}},
		{"section": "rolebyte", "schema": "rolebyte-config/1", "refersTo": []any{}},
	}))
}

var allVersions = map[string]string{"orders": "sha256:orders", "stock": "sha256:stock", "rolebyte": "sha256:roles"}

// Every call the transfer makes — the export's reads, the apply's reads,
// previews, writes and restores — asks with the person's own scopes under that
// owner's keys and nothing else, never with an empty scope, and hands the
// caller a header.
func TestEveryCallCarriesOnlyTheScopesUnderItsOwnersKeys(t *testing.T) {
	d := newDeployment(t)
	d.roles.failApply = 1
	p := transfer.Person{Tenant: testTenant, Scopes: []string{"orders/setup:import", "stock/setup:import", "warehouse:admin",
		"membership:admin", "billing/invoice:view", "authority/order:view", "openid"}}

	_, err := d.transfer().Export(context.Background(), p, d, exportDay)
	qt.Assert(t, qt.IsNil(err))
	imp, err := d.transfer().Import(context.Background(), p, d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection, "rolebyte": rolesSection}, allVersions), false)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.Equals(imp.Answer.Outcome, document.RolledBack))

	keys := map[string][]string{"orders": {"orders"}, "stock": {"stock", "warehouse"}, "rolebyte": {"membership"}}
	calls := d.recorded()
	qt.Assert(t, qt.HasLen(calls, 15), qt.Commentf("3 export reads, 3 reads, 3 previews, 3 applies, 1 read-back of the failed one, 2 restores"))
	for _, c := range calls {
		qt.Check(t, qt.IsFalse(c.nilHeader), qt.Commentf("%s %s to %s", c.method, c.path, c.section))
		qt.Assert(t, qt.Not(qt.Equals(strings.TrimSpace(c.scope), "")))
		for _, s := range strings.Fields(c.scope) {
			key := s[:strings.IndexAny(s, "/:")]
			qt.Check(t, qt.IsTrue(slices.Contains(keys[c.section], key)), qt.Commentf("%s travelled to %s", s, c.section))
		}
	}
}

// A caller that answers neither a response nor an error is a call that produced
// no owner's answer: the export ends, a dry run's section fails, an apply is
// refused before anything is written — never a panic.
func TestACallerThatAnswersNothingIsAnUnansweredCall(t *testing.T) {
	d := newDeployment(t)
	d.answerNothing = true
	three := map[string]string{"orders": ordersSection, "stock": stockSection, "rolebyte": rolesSection}
	var unreached *transfer.Unreached

	_, err := d.transfer().Export(context.Background(), everyone(), d, exportDay)
	qt.Check(t, qt.ErrorAs(err, &unreached))

	imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t, three, nil), true)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(imp.Answer.Outcome, document.Failed))

	_, err = d.transfer().Import(context.Background(), everyone(), d, importDoc(t, three, allVersions), false)
	qt.Check(t, qt.ErrorAs(err, &unreached))
	qt.Check(t, qt.HasLen(callsTo(d.recorded(), http.MethodPost, "/config/apply"), 0))
}

// A transfer with no owners set answers an error rather than panicking, and
// lists no sections.
func TestATransferWithNoOwnersAnswersAnError(t *testing.T) {
	d := newDeployment(t)
	tr := &transfer.Transfer{}

	_, err := tr.Export(context.Background(), everyone(), d, exportDay)
	qt.Check(t, qt.IsNotNil(err))
	_, err = tr.Import(context.Background(), everyone(), d, importDoc(t, map[string]string{"orders": ordersSection}, nil), true)
	qt.Check(t, qt.IsNotNil(err))
	qt.Check(t, qt.HasLen(tr.Sections(), 0))
	qt.Check(t, qt.HasLen(d.recorded(), 0))
}

// When the request ends while the import is writing — the person gone, its
// deadline passed — the sections already written are still restored: the
// restore does not stop with the request.
func TestTheRestoreRunsAfterTheRequestEnded(t *testing.T) {
	d := newDeployment(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.roles.failApply = 1
	d.roles.onFail = cancel

	edited := `{"schema":"orders-config/1","priorities":[]}`
	imp, err := d.transfer().Import(ctx, everyone(), d, importDoc(t,
		map[string]string{"orders": edited, "stock": stockSection, "rolebyte": rolesSection}, allVersions), false)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(imp.Answer.Outcome, document.RolledBack))
	qt.Check(t, qt.Equals(d.orders.section, ordersSection))
	qt.Check(t, qt.DeepEquals(d.orders.written, []string{edited, ordersSection}))
}

// An owner's refusal or failure reaches the section's line only as its public
// part — code, status, title, trace id — never the services it passed through
// or its internal detail: the line travels inside an answer of 200, past the
// host's own projection.
func TestASectionsLineCarriesOnlyThePublicPartOfTheOwnersProblem(t *testing.T) {
	d := newDeployment(t)
	d.orders.failApply = 1
	d.orders.failBody = `{"type":"about:blank","title":"Internal","status":500,"code":"err:owner:internal",` +
		`"detail":"pq: relation \"cfg_orders\" does not exist","source":"orders","trace_id":"01TRACE",` +
		`"chain":[{"service":"orders-db","code":"err:db:missing","status":500}]}`

	imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection}, map[string]string{"orders": "sha256:orders"}), false)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(imp.Answer.Outcome, document.Failed))
	qt.Check(t, qt.JSONEquals([]byte(imp.Answer.Sections["orders"].Problem), map[string]any{
		"type": "about:blank", "title": "Internal", "status": 500, "code": "err:owner:internal", "trace_id": "01TRACE",
	}))

	// A preview's refusal alike.
	d = newDeployment(t)
	stockFails := `{"title":"Conflict","status":409,"code":"err:stock:busy","detail":"lock held by job 7","source":"stock"}`
	call := transfer.CallerFunc(func(ctx context.Context, o *owners.Owner, scope, method, path string, header http.Header, body []byte) (*configclient.Response, error) {
		if o.Section == "stock" && path == "/config/preview" {
			return &configclient.Response{Status: http.StatusConflict, Header: http.Header{}, Body: []byte(stockFails)}, nil
		}

		return d.Call(ctx, o, scope, method, path, header, body)
	})
	imp, err = d.transfer().Import(context.Background(), everyone(), call, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection}, nil), true)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.JSONEquals([]byte(imp.Answer.Sections["stock"].Problem), map[string]any{
		"title": "Conflict", "status": 409, "code": "err:stock:busy",
	}))
}

// A refusal or failure whose body is no problem keeps only its status in the
// section's line: refused for a 4xx, unavailable otherwise — never the body.
func TestASectionsLineWithoutAProblemBodyKeepsOnlyItsStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   map[string]any
	}{
		{http.StatusConflict, map[string]any{"code": "err:upstream:refused", "status": 409}},
		{http.StatusInternalServerError, map[string]any{"code": "err:upstream:unavailable", "status": 502}},
	} {
		d := newDeployment(t)
		d.orders.failApply = 1
		d.orders.failStatus = tc.status
		d.orders.failBody = "<html>a proxy's page naming its host</html>"

		imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
			map[string]string{"orders": ordersSection}, map[string]string{"orders": "sha256:orders"}), false)
		qt.Assert(t, qt.IsNil(err))
		qt.Check(t, qt.JSONEquals([]byte(imp.Answer.Sections["orders"].Problem), tc.want), qt.Commentf("status %d", tc.status))
	}
}

// A write whose answer was lost after the owner wrote it leaves its section
// holding the document: the import is partial, never rolled back. The sections
// before it are restored; that one is not written back over, because a move
// cannot be told from someone else's change made in between. The read-back runs
// even when the request ended with the lost answer.
func TestAWriteWhoseAnswerWasLostAfterItLandedMakesTheImportPartial(t *testing.T) {
	d := newDeployment(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.roles.loseApply = 1
	d.roles.onLose = cancel
	edited := `{"schema":"rolebyte-config/1","roles":[]}`

	imp, err := d.transfer().Import(ctx, everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection, "rolebyte": edited}, allVersions), false)
	qt.Assert(t, qt.IsNil(err))

	a := imp.Answer
	qt.Check(t, qt.Equals(a.Outcome, document.Partial))
	qt.Check(t, qt.Equals(a.Sections["orders"].Status, document.SectionRolledBack))
	qt.Check(t, qt.Equals(a.Sections["stock"].Status, document.SectionRolledBack))
	qt.Check(t, qt.Equals(a.Sections["rolebyte"].Status, document.SectionFailed))
	qt.Check(t, qt.Equals(a.Sections["rolebyte"].Version, "sha256:roles+"))
	qt.Check(t, qt.IsTrue(strings.Contains(a.Sections["rolebyte"].Detail, "changed after the preview")), qt.Commentf("%s", a.Sections["rolebyte"].Detail))
	qt.Check(t, qt.DeepEquals(d.roles.written, []string{edited}), qt.Commentf("written once, never written back over"))
	qt.Check(t, qt.HasLen(d.warns, 3), qt.Commentf("%v", d.warns))
	qt.Check(t, qt.Equals(d.warns[2], "configuration import compensated after a failed section (tenant "+testTenant+", partial)"))
}

// The first section's write lost after it landed: nothing to restore, and the
// import is partial, not failed.
func TestTheFirstWriteLostAfterItLandedIsPartialNotFailed(t *testing.T) {
	d := newDeployment(t)
	d.orders.loseApply = 1

	imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection}, allVersions), false)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(imp.Answer.Outcome, document.Partial))
	qt.Check(t, qt.Equals(imp.Answer.Sections["stock"].Status, document.SectionWithheld))
}

// A write whose answer was lost before anything was written leaves its section
// as it was: the read-back finds the version the apply named, and the import
// rolls back as for any failed section.
func TestAWriteLostBeforeItLandedStillRollsBack(t *testing.T) {
	d := newDeployment(t)
	d.roles.dropApply = 1

	imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection, "rolebyte": rolesSection}, allVersions), false)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(imp.Answer.Outcome, document.RolledBack))
	qt.Check(t, qt.HasLen(d.roles.written, 0))
	qt.Check(t, qt.Equals(imp.Answer.Sections["rolebyte"].Detail, ""))
	// It was read back: once before the writes, once after its write was lost.
	var reads int
	for _, c := range callsTo(d.recorded(), http.MethodGet, "/config") {
		if c.section == "rolebyte" {
			reads++
		}
	}
	qt.Check(t, qt.Equals(reads, 2))
}

// A server error after the owner wrote — a proxy's timeout in front of it, say —
// is no clear answer either: the section is read back, and the import is
// partial, not rolled back. So is a 200 whose body is no report.
func TestAServerErrorOrAnUnreadableAnswerAfterTheWriteIsReadBack(t *testing.T) {
	for name, tune := range map[string]func(s *stub){
		"a 504 after the write":            func(s *stub) { s.gatewayAfterWrite = 1 },
		"a 200 that is no report":          func(s *stub) { s.garbleAfterWrite = 1 },
		"no answer at all after the write": func(s *stub) { s.loseApply = 1 },
	} {
		d := newDeployment(t)
		tune(d.roles)

		imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
			map[string]string{"orders": ordersSection, "rolebyte": `{"schema":"rolebyte-config/1","roles":[]}`}, allVersions), false)
		qt.Assert(t, qt.IsNil(err))
		qt.Check(t, qt.Equals(imp.Answer.Outcome, document.Partial), qt.Commentf("%s", name))
		qt.Check(t, qt.IsTrue(strings.Contains(imp.Answer.Sections["rolebyte"].Detail, "changed after the preview")), qt.Commentf("%s", name))
		qt.Check(t, qt.Equals(imp.Answer.Sections["orders"].Status, document.SectionRolledBack), qt.Commentf("%s", name))
	}
}

// An apply the token authority refused was never sent: that is a clear answer,
// so nothing is read back and the import rolls back as for any refusal.
func TestAnApplyTheAuthorityRefusedIsNotReadBack(t *testing.T) {
	d := newDeployment(t)
	d.roles.authorityRefuses = 1

	imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "rolebyte": rolesSection}, allVersions), false)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(imp.Answer.Outcome, document.RolledBack))
	var reads int
	for _, c := range callsTo(d.recorded(), http.MethodGet, "/config") {
		if c.section == "rolebyte" {
			reads++
		}
	}
	qt.Check(t, qt.Equals(reads, 1), qt.Commentf("only the read before the writes"))
}

// A restore whose own answer is unclear says the section may hold either
// version; a restore its owner refused (someone changed it since) says it holds
// the document's version or that change.
func TestARestoreWithoutAClearAnswerSaysMay(t *testing.T) {
	d := newDeployment(t)
	d.stock.failApply = 1
	d.orders.loseApply = 2 // the restore

	imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection}, allVersions), false)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(imp.Answer.Outcome, document.Partial))
	qt.Check(t, qt.IsTrue(strings.Contains(imp.Answer.Sections["orders"].Detail, "may hold the document's version or what it held before")))

	d = newDeployment(t)
	d.stock.failApply = 1
	d.stock.failStatus = http.StatusConflict
	d.orders.failApply = 2
	d.orders.failStatus = http.StatusPreconditionFailed
	d.orders.failBody = `{"code":"err:owner:config_version_moved","title":"Changed","status":412}`
	imp, err = d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "stock": stockSection}, allVersions), false)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(imp.Answer.Outcome, document.Partial))
	qt.Check(t, qt.IsTrue(strings.HasSuffix(imp.Answer.Sections["orders"].Detail, "the document's version, or a change made since")))
}

// A read-back after the request ended is bounded by the transfer's own limit:
// an owner that never answers it cannot hold the import for ever.
func TestAReadBackIsBoundedAfterTheRequestEnded(t *testing.T) {
	d := newDeployment(t)
	d.roles.loseApply = 1
	hang := transfer.CallerFunc(func(ctx context.Context, o *owners.Owner, scope, method, path string, header http.Header, body []byte) (*configclient.Response, error) {
		if o.Section == "rolebyte" && method == http.MethodGet && d.roles.lost {
			<-ctx.Done()

			return nil, ctx.Err()
		}

		return d.Call(ctx, o, scope, method, path, header, body)
	})
	tr := d.transfer()
	tr.AfterRequest = 50 * time.Millisecond

	done := make(chan *transfer.Imported, 1)
	go func() {
		imp, err := tr.Import(context.Background(), everyone(), hang, importDoc(t,
			map[string]string{"orders": ordersSection, "rolebyte": rolesSection}, allVersions), false)
		qt.Check(t, qt.IsNil(err))
		done <- imp
	}()
	select {
	case imp := <-done:
		qt.Check(t, qt.Equals(imp.Answer.Outcome, document.Partial))
		qt.Check(t, qt.IsTrue(strings.Contains(imp.Answer.Sections["rolebyte"].Detail, "could not be read back")))
	case <-time.After(5 * time.Second):
		t.Fatal("the read-back after the request ended was not bounded")
	}
}

// A body the kit reads as a problem but that carries no code of the fleet's
// shape is no problem: only its status is kept. One that does keeps the status
// the owner answered with, not the one in its body, and a title of one line.
func TestAProblemWithoutACodeOrWithALongTitleIsKeptToItsPublicShape(t *testing.T) {
	long := strings.Repeat("ā", 300)
	for _, tc := range []struct {
		status int
		body   string
		want   map[string]any
	}{
		{http.StatusInternalServerError, `{"status":500}`, map[string]any{"code": "err:upstream:unavailable", "status": 502}},
		{http.StatusConflict, `{"title":"x"}`, map[string]any{"code": "err:upstream:refused", "status": 409}},
		{http.StatusConflict, `{"code":"err:owner:busy","title":"Busy","status":200}`, map[string]any{"code": "err:owner:busy", "title": "Busy", "status": 409}},
		{http.StatusConflict, `{"code":"err:owner:busy","title":"` + long + `","status":409}`,
			map[string]any{"code": "err:owner:busy", "title": strings.Repeat("ā", 200), "status": 409}},
	} {
		d := newDeployment(t)
		d.orders.failApply = 1
		d.orders.failStatus = tc.status
		d.orders.failBody = tc.body

		imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
			map[string]string{"orders": ordersSection}, map[string]string{"orders": "sha256:orders"}), false)
		qt.Assert(t, qt.IsNil(err))
		qt.Check(t, qt.JSONEquals([]byte(imp.Answer.Sections["orders"].Problem), tc.want), qt.Commentf("%d %.40s", tc.status, tc.body))
	}
}

// A write whose answer was lost, on a section that then cannot be read either:
// nobody can tell whether it landed, so the import is partial and the line says
// it may hold the document.
func TestALostWriteWhoseSectionCannotBeReadBackIsPartial(t *testing.T) {
	d := newDeployment(t)
	d.roles.loseApply = 1
	d.roles.unreadableAfterLoss = true

	imp, err := d.transfer().Import(context.Background(), everyone(), d, importDoc(t,
		map[string]string{"orders": ordersSection, "rolebyte": rolesSection}, allVersions), false)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(imp.Answer.Outcome, document.Partial))
	qt.Check(t, qt.IsTrue(strings.Contains(imp.Answer.Sections["rolebyte"].Detail, "could not be read back")))
	qt.Check(t, qt.Equals(imp.Answer.Sections["orders"].Status, document.SectionRolledBack))
}
