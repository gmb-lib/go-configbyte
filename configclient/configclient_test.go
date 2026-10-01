package configclient_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/go-quicktest/qt"

	"github.com/gmb-lib/go-configbyte/configclient"
	"github.com/gmb-lib/go-configbyte/contract"
)

// owner stands in for one owner of a section, per organisation: its version
// read answers the version, its section read answers the section with that
// version as the ETag. Every call is recorded.
type owner struct {
	mu       sync.Mutex
	sections map[string]string // tenant → section
	versions map[string]string // tenant → version
	refuse   map[string]int    // path → status answered instead
	down     bool
	calls    []string
}

func newOwner() *owner {
	return &owner{sections: map[string]string{}, versions: map[string]string{}, refuse: map[string]int{}}
}

func (o *owner) set(tenant, section, version string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sections[tenant], o.versions[tenant] = section, version
}

// fetch is the owner as one organisation's member reaches it.
func (o *owner) fetch(tenant string) configclient.Fetch {
	return func(_ context.Context, path string) (*configclient.Response, error) {
		o.mu.Lock()
		defer o.mu.Unlock()
		o.calls = append(o.calls, tenant+" "+path)
		if o.down {
			return nil, errors.New("connection refused")
		}
		if status, ok := o.refuse[path]; ok {
			return &configclient.Response{Status: status, Header: http.Header{"Content-Type": {"application/problem+json"}},
				Body: []byte(`{"code":"err:orders:forbidden","status":403}`)}, nil
		}
		switch path {
		case contract.PathVersion:
			return &configclient.Response{Status: 200, Body: []byte(`{"version":"` + o.versions[tenant] + `"}`)}, nil
		case contract.PathConfig:
			return &configclient.Response{Status: 200, Header: http.Header{"Etag": {contract.ETag(o.versions[tenant])}},
				Body: []byte(o.sections[tenant])}, nil
		case contract.PathSection:
			return &configclient.Response{Status: 200, Body: []byte(`{"section":"orders","schema":"orders-config/1","refersTo":[]}`)}, nil
		}

		return &configclient.Response{Status: 404}, nil
	}
}

func (o *owner) count(path string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, c := range o.calls {
		if len(c) >= len(path) && c[len(c)-len(path):] == path {
			n++
		}
	}

	return n
}

// The first read carries the section across; an unchanged version is answered
// from the copy, and the owner is asked only for its version.
func TestAnUnchangedVersionIsAnsweredFromTheCopy(t *testing.T) {
	o := newOwner()
	o.set("t1", `{"schema":"orders-config/1","v":1}`, "sha256:1")
	c := configclient.NewCache(10)

	body, fromCopy, err := c.Current(context.Background(), "t1", "orders", o.fetch("t1"))
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.IsFalse(fromCopy))
	qt.Check(t, qt.Equals(string(body), `{"schema":"orders-config/1","v":1}`))

	body, fromCopy, err = c.Current(context.Background(), "t1", "orders", o.fetch("t1"))
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.IsTrue(fromCopy))
	qt.Check(t, qt.Equals(string(body), `{"schema":"orders-config/1","v":1}`))
	qt.Check(t, qt.Equals(o.count(contract.PathVersion), 2), qt.Commentf("the version is asked every time"))
	qt.Check(t, qt.Equals(o.count(contract.PathConfig), 1), qt.Commentf("the section crossed once"))
}

// A moved version reads the section afresh and answers the new content.
func TestAMovedVersionReadsTheSectionAfresh(t *testing.T) {
	o := newOwner()
	o.set("t1", `{"v":1}`, "sha256:1")
	c := configclient.NewCache(10)
	_, _, err := c.Current(context.Background(), "t1", "orders", o.fetch("t1"))
	qt.Assert(t, qt.IsNil(err))

	o.set("t1", `{"v":2}`, "sha256:2")
	body, fromCopy, err := c.Current(context.Background(), "t1", "orders", o.fetch("t1"))
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.IsFalse(fromCopy))
	qt.Check(t, qt.Equals(string(body), `{"v":2}`))
}

// The copy is kept under the version stamped on the bytes it came with, not the
// one asked a moment earlier: a change landing between the two calls must not
// file new content under an old version.
func TestTheCopyIsKeptUnderTheVersionOnItsOwnBytes(t *testing.T) {
	o := newOwner()
	o.set("t1", `{"v":1}`, "sha256:1")
	c := configclient.NewCache(10)

	raced := func(ctx context.Context, path string) (*configclient.Response, error) {
		if path == contract.PathVersion {
			resp, err := o.fetch("t1")(ctx, path)
			o.set("t1", `{"v":2}`, "sha256:2") // a write lands between the two calls

			return resp, err
		}

		return o.fetch("t1")(ctx, path)
	}
	body, _, err := c.Current(context.Background(), "t1", "orders", raced)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(string(body), `{"v":2}`))

	_, ok := c.Get("t1", "orders", "sha256:1")
	qt.Check(t, qt.IsFalse(ok), qt.Commentf("new content filed under the old version"))
	held, ok := c.Get("t1", "orders", "sha256:2")
	qt.Check(t, qt.IsTrue(ok))
	qt.Check(t, qt.Equals(string(held), `{"v":2}`))
}

// Two organisations never share a copy — not even when their owners answer the
// same version string. Each is read on its own member's behalf.
func TestOneOrganisationIsNeverHandedAnothersCopy(t *testing.T) {
	o := newOwner()
	o.set("t1", `{"tenant":"one"}`, "sha256:same")
	o.set("t2", `{"tenant":"two"}`, "sha256:same")
	c := configclient.NewCache(10)

	_, _, err := c.Current(context.Background(), "t1", "orders", o.fetch("t1"))
	qt.Assert(t, qt.IsNil(err))
	body, fromCopy, err := c.Current(context.Background(), "t2", "orders", o.fetch("t2"))
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.IsFalse(fromCopy))
	qt.Check(t, qt.Equals(string(body), `{"tenant":"two"}`))
}

// An owner's refusal ends the read with the owner's own answer, which the caller
// relays as it came; a refused read leaves no copy behind.
func TestAnOwnersRefusalEndsTheReadAndLeavesNoCopy(t *testing.T) {
	for _, path := range []string{contract.PathVersion, contract.PathConfig} {
		o := newOwner()
		o.set("t1", `{"v":1}`, "sha256:1")
		o.refuse[path] = 403
		c := configclient.NewCache(10)

		_, _, err := c.Current(context.Background(), "t1", "orders", o.fetch("t1"))
		var refused *configclient.Refused
		qt.Assert(t, qt.ErrorAs(err, &refused), qt.Commentf("%s", path))
		qt.Check(t, qt.Equals(refused.Path, path))
		qt.Check(t, qt.Equals(refused.Status, 403))
		qt.Check(t, qt.Equals(string(refused.Body), `{"code":"err:orders:forbidden","status":403}`))
		qt.Check(t, qt.Equals(refused.Header.Get("Content-Type"), "application/problem+json"))
		qt.Check(t, qt.Not(qt.StringContains(err.Error(), "forbidden")), qt.Commentf("the owner's words stay in their field"))
		qt.Check(t, qt.Equals(c.Len(), 0))
	}
}

// A request that was never answered is the transport's own error, not a
// refusal, so a caller can tell "unreachable" from "refused".
func TestAnOwnerThatCannotBeReachedIsNotARefusal(t *testing.T) {
	o := newOwner()
	o.down = true
	c := configclient.NewCache(10)
	_, _, err := c.Current(context.Background(), "t1", "orders", o.fetch("t1"))
	qt.Assert(t, qt.IsNotNil(err))
	var refused *configclient.Refused
	qt.Check(t, qt.IsFalse(errors.As(err, &refused)))
	qt.Check(t, qt.StringContains(err.Error(), "connection refused"))
}

// An owner that answers no version, or no ETag, is never answered from a copy
// and never fills one: an empty version matches nothing.
func TestNoVersionNeverFillsOrMatchesACopy(t *testing.T) {
	o := newOwner()
	o.set("t1", `{"v":1}`, "")
	c := configclient.NewCache(10)
	for range 2 {
		_, fromCopy, err := c.Current(context.Background(), "t1", "orders", o.fetch("t1"))
		qt.Assert(t, qt.IsNil(err))
		qt.Check(t, qt.IsFalse(fromCopy))
	}
	qt.Check(t, qt.Equals(c.Len(), 0))
}

func TestTheSectionAnswerIsReadAndChecked(t *testing.T) {
	o := newOwner()
	a, err := configclient.Section(context.Background(), o.fetch(""))
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.DeepEquals(a, contract.SectionAnswer{Section: "orders", Schema: "orders-config/1", RefersTo: []string{}}))

	for name, body := range map[string]string{
		"not an answer":            `[]`,
		"another section's schema": `{"section":"orders","schema":"stock-config/1","refersTo":[]}`,
		"references not said":      `{"section":"orders","schema":"orders-config/1"}`,
	} {
		wrong := func(context.Context, string) (*configclient.Response, error) {
			return &configclient.Response{Status: 200, Body: []byte(body)}, nil
		}
		_, err := configclient.Section(context.Background(), wrong)
		qt.Check(t, qt.IsNotNil(err), qt.Commentf("%s", name))
	}

	missing := func(context.Context, string) (*configclient.Response, error) {
		return &configclient.Response{Status: 404}, nil
	}
	_, err = configclient.Section(context.Background(), missing)
	var refused *configclient.Refused
	qt.Check(t, qt.ErrorAs(err, &refused), qt.Commentf("an address with no section answer is a refusal to relay"))
}

func TestReadAndVersionAnswerWhatTheOwnerSaid(t *testing.T) {
	o := newOwner()
	o.set("t1", `{"v":1}`, "sha256:1")
	v, err := configclient.Version(context.Background(), o.fetch("t1"))
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(v, "sha256:1"))
	body, stamped, err := configclient.Read(context.Background(), o.fetch("t1"))
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(string(body), `{"v":1}`))
	qt.Check(t, qt.Equals(stamped, "sha256:1"))

	none := func(context.Context, string) (*configclient.Response, error) { return nil, nil }
	_, err = configclient.Version(context.Background(), none)
	qt.Check(t, qt.IsNotNil(err))
}
