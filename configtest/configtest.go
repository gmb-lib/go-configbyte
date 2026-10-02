// Package configtest is the proof ladder for a service that owns a
// configuration section: every promise the contract makes, checked against the
// service's own routes, from the service's own tests, with its own section.
//
// [Gates] runs with no database behind the service. It proves each operation is
// where the contract puts it and guarded as the contract says: nothing but the
// section answer answers without a token, the section answer needs none and
// names what a token for the service carries, readers reach the store, writers
// reach it and non-writers do not, only a writer may read the section as an
// export, an apply naming no version is refused before the store is asked, and
// the reasons render as themselves.
//
// [Live] runs against a real store. It walks the contract end to end: the section
// read with its version as the ETag, a document that changes nothing previewed
// and applied without moving the version, the refusals the service's own rules
// make — each reported alike by the preview and the apply, each writing nothing,
// not even the clean item beside it — a real change previewed without landing
// and applied landing, an apply against a moved version refused, another tenant
// holding another section, and a document that is not this section refused —
// and every transport recorded in the owner's own history: one line for each
// export and each apply that was not refused, carrying the part's hash and, for
// an import, the document's.
//
// Both take the callers as the service's own test client presents them, so the
// ladder never needs to know how the service authenticates.
package configtest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"azugo.io/azugo"
	"github.com/go-quicktest/qt"
	"github.com/valyala/fasthttp"

	"github.com/gmb-lib/go-configbyte/contract"
)

// Caller is one principal, as the service's test client presents it: the
// options a request is sent with.
type Caller []azugo.TestClientOption

// Service is the owning service under test.
type Service struct {
	// Client is the test client of the service, started.
	Client *azugo.TestClient
	// Root is the service's versioned API root, such as "/api/v1".
	Root string
	// Section is the section's name; its payload is the name plus "-config/1".
	Section string
	// Domain is the error domain the service's refusals carry.
	Domain string
	// RefersTo names the sections this section refers to.
	RefersTo []string
	// Audience and ScopeKeys are what the section answer says a token for the
	// service carries.
	Audience  string
	ScopeKeys []string
}

func (s Service) purpose(p string) azugo.TestClientOption {
	return s.Client.WithHeader(contract.HeaderPurpose, p)
}

// reply is one answer, read whole.
type reply struct {
	status int
	raw    string
	body   map[string]any
	etag   string
}

func (s Service) call(t testing.TB, method, path string, body []byte, who Caller, extra ...azugo.TestClientOption) reply {
	t.Helper()
	opts := append(append([]azugo.TestClientOption{}, who...), extra...)
	var (
		resp *fasthttp.Response
		err  error
	)
	if method == fasthttp.MethodGet {
		resp, err = s.Client.Get(s.Root+path, opts...)
	} else {
		resp, err = s.Client.Post(s.Root+path, body, opts...)
	}
	qt.Assert(t, qt.IsNil(err), qt.Commentf("%s %s", method, path))
	defer fasthttp.ReleaseResponse(resp)
	raw, _ := resp.BodyUncompressed()
	r := reply{status: resp.StatusCode(), raw: string(raw), etag: string(resp.Header.Peek("ETag"))}
	_ = json.Unmarshal(raw, &r.body)

	return r
}

func (s Service) ifMatch(version string) azugo.TestClientOption {
	return s.Client.WithHeader("If-Match", contract.ETag(version))
}

// Gates proves, with no store behind the service, that each operation is
// guarded as the contract says.
type Gates struct {
	Service
	// Anyone is a caller with a valid token, no tenant and nothing of this
	// service: the section answer is theirs, as it is a caller's with no token.
	Anyone Caller
	// Readers must each pass the gate of both reads.
	Readers []Caller
	// Unreadable must each be refused both reads, such as a service acting as
	// itself with nothing of this service.
	Unreadable []Caller
	// Writers must each pass the gate of the preview and of the apply.
	Writers []Caller
	// Refused must each be refused the preview and the apply: a member who does
	// not administer, a person holding the ladder's rungs, one module's
	// administrator where a document reaches into two.
	Refused []Caller
	// Unready is what the service answers a request that passed its gate and
	// found no store. Zero means 503.
	Unready int
}

// Run runs every rung.
func (g Gates) Run(t testing.TB) {
	t.Helper()
	unready := g.Unready
	if unready == 0 {
		unready = fasthttp.StatusServiceUnavailable
	}
	doc := []byte(`{"schema":"` + contract.Schema(g.Section) + `"}`)
	get, post := fasthttp.MethodGet, fasthttp.MethodPost

	// Nothing but the section answer answers without a token.
	for _, path := range []string{contract.PathConfig, contract.PathVersion} {
		qt.Check(t, qt.Equals(g.call(t, get, path, nil, nil).status, fasthttp.StatusUnauthorized), qt.Commentf("GET %s without a token", path))
	}
	for _, path := range []string{contract.PathPreview, contract.PathApply} {
		qt.Check(t, qt.Equals(g.call(t, post, path, doc, nil).status, fasthttp.StatusUnauthorized), qt.Commentf("POST %s without a token", path))
	}

	// Which section answers here needs no token, no tenant and no person, and
	// names what a token for the service carries.
	refs := g.RefersTo
	if refs == nil {
		refs = []string{}
	}
	want, err := json.Marshal(contract.SectionAnswer{Section: g.Section, Schema: contract.Schema(g.Section), RefersTo: refs,
		Audience: g.Audience, ScopeKeys: g.ScopeKeys})
	qt.Assert(t, qt.IsNil(err))
	for name, who := range map[string]Caller{"no token": nil, "anyone": g.Anyone} {
		got := g.call(t, get, contract.PathSection, nil, who)
		qt.Check(t, qt.Equals(got.status, fasthttp.StatusOK), qt.Commentf("the section answer, %s: %s", name, got.raw))
		qt.Check(t, qt.Equals(got.raw, string(want)),
			qt.Commentf("the section answer, %s, names the section, its schema, its references, its audience and its scope keys", name))
	}

	for i, c := range g.Readers {
		for _, path := range []string{contract.PathConfig, contract.PathVersion} {
			qt.Check(t, qt.Equals(g.call(t, get, path, nil, c).status, unready), qt.Commentf("reader %d: GET %s passes the gate", i, path))
		}
	}
	for i, c := range g.Unreadable {
		for _, path := range []string{contract.PathConfig, contract.PathVersion} {
			qt.Check(t, qt.Equals(g.call(t, get, path, nil, c).status, fasthttp.StatusForbidden), qt.Commentf("unreadable %d: GET %s", i, path))
		}
	}
	for i, c := range g.Refused {
		for _, path := range []string{contract.PathPreview, contract.PathApply} {
			got := g.call(t, post, path, doc, c, g.ifMatch("sha256:x"))
			qt.Check(t, qt.Equals(got.status, fasthttp.StatusForbidden), qt.Commentf("refused %d: POST %s: %s", i, path, got.raw))
		}
		// Taking the section out as an export is the administrator's too: it
		// writes a line in the owner's history.
		got := g.call(t, get, contract.PathConfig, nil, c, g.purpose(contract.PurposeExport))
		qt.Check(t, qt.Equals(got.status, fasthttp.StatusForbidden), qt.Commentf("refused %d: an export read: %s", i, got.raw))
	}
	for i, c := range g.Writers {
		// A preview needs no version; an apply naming one — strong or weak —
		// reaches the store; an apply naming none is refused before it.
		qt.Check(t, qt.Equals(g.call(t, post, contract.PathPreview, doc, c).status, unready), qt.Commentf("writer %d: a preview needs no version", i))
		for _, tag := range []string{`"sha256:x"`, `W/"sha256:x"`} {
			got := g.call(t, post, contract.PathApply, doc, c, g.Client.WithHeader("If-Match", tag))
			qt.Check(t, qt.Equals(got.status, unready), qt.Commentf("writer %d: an apply naming %s reaches the store", i, tag))
		}
		got := g.call(t, post, contract.PathApply, doc, c)
		qt.Check(t, qt.Equals(got.status, fasthttp.StatusPreconditionRequired), qt.Commentf("writer %d: an apply naming no version: %s", i, got.raw))
		qt.Check(t, qt.StringContains(got.raw, contract.Code(g.Domain, contract.ReasonVersionRequired)), qt.Commentf("writer %d", i))

		// A writer may read the section as an export; a purpose the contract
		// does not know is refused before the store, never read as a plain read.
		got = g.call(t, get, contract.PathConfig, nil, c, g.purpose(contract.PurposeExport))
		qt.Check(t, qt.Equals(got.status, unready), qt.Commentf("writer %d: an export read reaches the store: %s", i, got.raw))
		got = g.call(t, get, contract.PathConfig, nil, c, g.purpose("backup"))
		qt.Check(t, qt.Equals(got.status, fasthttp.StatusBadRequest), qt.Commentf("writer %d: an unknown purpose: %s", i, got.raw))
	}

	Reasons(t, g.Domain)
}

// Reasons proves every reason the contract refuses with renders, in the
// service's domain, with its status, its code and a public title.
func Reasons(t testing.TB, domain string) {
	t.Helper()
	for reason, spec := range contract.Reasons {
		err := contract.Refusal(domain, reason, "the key is named here")
		var status interface{ StatusCode() int }
		qt.Check(t, qt.IsTrue(errors.As(err, &status)), qt.Commentf("%s renders with a status", reason))
		if status != nil {
			qt.Check(t, qt.Equals(status.StatusCode(), spec.Status), qt.Commentf("%s", reason))
		}
		var code interface{ ErrorCode() string }
		qt.Check(t, qt.IsTrue(errors.As(err, &code)), qt.Commentf("%s keeps its code", reason))
		if code != nil {
			qt.Check(t, qt.Equals(code.ErrorCode(), contract.Code(domain, reason)), qt.Commentf("%s", reason))
		}
		qt.Check(t, qt.Not(qt.Equals(strings.TrimSpace(spec.Title), "")), qt.Commentf("%s carries a public title", reason))
	}
}
