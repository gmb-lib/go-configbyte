package owners_test

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-quicktest/qt"

	"github.com/gmb-lib/go-configbyte/configclient"
	"github.com/gmb-lib/go-configbyte/contract"
	"github.com/gmb-lib/go-configbyte/owners"
)

// answer builds the section answer an owner gives, failing the test when the
// answer is not one an owner could give.
func answer(t *testing.T, section string, scopeKeys []string, refersTo ...string) contract.SectionAnswer {
	t.Helper()

	a, err := contract.NewSectionAnswer(section, "svc:"+section, scopeKeys, refersTo)
	qt.Assert(t, qt.IsNil(err))

	return a
}

// fixed answers each address with the answer given for it, and an address it
// does not know as a transport that reached nothing.
func fixed(answers map[string]contract.SectionAnswer) owners.Prober {
	return func(_ context.Context, address string) (contract.SectionAnswer, error) {
		a, ok := answers[address]
		if !ok {
			return contract.SectionAnswer{}, errors.New("connection refused")
		}

		return a, nil
	}
}

func sections(set *owners.Set) []string {
	var out []string
	for _, o := range set.Order() {
		out = append(out, o.Section)
	}

	return out
}

func discover(t *testing.T, answers map[string]contract.SectionAnswer, addresses ...string) (*owners.Set, error) {
	t.Helper()

	return owners.Discover(context.Background(), addresses, fixed(answers), 50*time.Millisecond, 10*time.Millisecond)
}

// The order a deployment with four owners and no reference between them
// writes in: by section name, the role register's last — after "stock", which
// its name alone would put it before.
func TestTheRoleRegisterIsWrittenLast(t *testing.T) {
	set, err := discover(t, map[string]contract.SectionAnswer{
		"http://rolebyte/api/v1": answer(t, "rolebyte", []string{"membership"}),
		"http://people/api/v1":   answer(t, "people", []string{"people", "staff", "equipment"}),
		"http://orders/api/v1":   answer(t, "orders", []string{"orders"}),
		"http://stock/api/v1":    answer(t, "stock", []string{"stock"}),
	}, "http://rolebyte/api/v1", "http://people/api/v1", "http://orders/api/v1", "http://stock/api/v1")
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.DeepEquals(sections(set), []string{"orders", "people", "stock", "rolebyte"}))

	o, ok := set.Lookup("orders")
	qt.Assert(t, qt.IsTrue(ok))
	qt.Check(t, qt.Equals(o.Address, "http://orders/api/v1"))
	qt.Check(t, qt.Equals(o.Audience, "svc:orders"))
	qt.Check(t, qt.DeepEquals(o.ScopeKeys, []string{"orders"}))
}

// A section is written after every section it refers to, whatever its name
// and whichever address named it first.
func TestASectionIsWrittenAfterTheSectionsItRefersTo(t *testing.T) {
	set, err := owners.Arrange([]*owners.Owner{
		{Address: "a", Section: "alpha", RefersTo: []string{"omega"}},
		{Address: "o", Section: "omega", RefersTo: []string{"zeta"}},
		{Address: "z", Section: "zeta"},
		{Address: "b", Section: "beta"},
	})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.DeepEquals(sections(set), []string{"beta", "zeta", "omega", "alpha"}))
}

// A reference outranks the role register's place: a section that refers to
// the roles is written after them.
func TestAReferenceToTheRolesOutranksTheirPlaceAtTheEnd(t *testing.T) {
	set, err := owners.Arrange([]*owners.Owner{
		{Address: "r", Section: owners.WidensAccess},
		{Address: "p", Section: "people", RefersTo: []string{owners.WidensAccess}},
		{Address: "a", Section: "assets"},
	})
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.DeepEquals(sections(set), []string{"assets", owners.WidensAccess, "people"}))
}

// The order does not depend on the order the addresses were named in.
func TestTheOrderIsTheSameOnEveryStart(t *testing.T) {
	list := []*owners.Owner{
		{Address: "c", Section: "gamma"}, {Address: "r", Section: owners.WidensAccess},
		{Address: "a", Section: "alpha", RefersTo: []string{"gamma"}}, {Address: "b", Section: "beta"},
	}
	first, err := owners.Arrange(list)
	qt.Assert(t, qt.IsNil(err))
	reversed := []*owners.Owner{list[3], list[2], list[1], list[0]}
	second, err := owners.Arrange(reversed)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.DeepEquals(sections(first), sections(second)))
	qt.Assert(t, qt.DeepEquals(sections(first), []string{"beta", "gamma", "alpha", owners.WidensAccess}))
}

// Every way a deployment's owners cannot be arranged fails the start, naming
// the cause.
func TestAnUnarrangeableDeploymentFailsNamingTheCause(t *testing.T) {
	for _, tc := range []struct {
		name  string
		found []*owners.Owner
		want  string
	}{
		{"two owners of one section", []*owners.Owner{
			{Address: "http://one", Section: "orders"}, {Address: "http://two", Section: "orders"},
		}, `http://one and http://two both hold the section "orders"`},
		{"a reference nobody holds", []*owners.Owner{
			{Address: "http://one", Section: "orders", RefersTo: []string{"stock"}},
		}, `"orders" refers to "stock", which no owner holds`},
		{"a circle", []*owners.Owner{
			{Address: "a", Section: "alpha", RefersTo: []string{"beta"}},
			{Address: "b", Section: "beta", RefersTo: []string{"alpha"}},
			{Address: "c", Section: "gamma"},
		}, "the sections alpha, beta refer to each other in a circle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := owners.Arrange(tc.found)
			qt.Assert(t, qt.ErrorMatches(err, ".*"+regexp.QuoteMeta(tc.want)+".*"))
		})
	}
}

// A deployment that names no owner has nothing to coordinate.
func TestNoAddressFailsTheStart(t *testing.T) {
	_, err := discover(t, nil)
	qt.Assert(t, qt.ErrorMatches(err, ".*no configuration owner is named.*"))
}

// An address nothing answers at is asked again until the window closes, then
// fails the start naming the address.
func TestAnAddressThatNeverAnswersFailsTheStartAfterTheWindow(t *testing.T) {
	asked := 0
	probe := func(context.Context, string) (contract.SectionAnswer, error) {
		asked++

		return contract.SectionAnswer{}, errors.New("connection refused")
	}

	started := time.Now()
	_, err := owners.Discover(context.Background(), []string{"http://nowhere/api/v1"}, probe, 60*time.Millisecond, 10*time.Millisecond)
	qt.Assert(t, qt.ErrorMatches(err, `owners: http://nowhere/api/v1 did not answer in time: connection refused`))
	qt.Check(t, qt.IsTrue(asked > 1), qt.Commentf("asked %d times", asked))
	qt.Check(t, qt.IsTrue(time.Since(started) < time.Second))
}

// An owner that starts after the service asking is found once it answers.
func TestAnOwnerThatStartsLateIsFound(t *testing.T) {
	asked := 0
	late := answer(t, "orders", []string{"orders"})
	probe := func(context.Context, string) (contract.SectionAnswer, error) {
		asked++
		if asked < 3 {
			return contract.SectionAnswer{}, &configclient.Refused{Path: contract.PathSection, Status: http.StatusServiceUnavailable}
		}

		return late, nil
	}

	set, err := owners.Discover(context.Background(), []string{"http://orders/api/v1"}, probe, time.Second, 5*time.Millisecond)
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.DeepEquals(sections(set), []string{"orders"}))
	qt.Check(t, qt.Equals(asked, 3))
}

// An address that answers and is not a configuration owner fails at once:
// asking again would not change the answer.
func TestAnAddressThatIsNotAnOwnerFailsAtOnce(t *testing.T) {
	asked := 0
	probe := func(context.Context, string) (contract.SectionAnswer, error) {
		asked++

		return contract.SectionAnswer{}, &configclient.Refused{Path: contract.PathSection, Status: http.StatusNotFound}
	}

	_, err := owners.Discover(context.Background(), []string{"http://web/api/v1"}, probe, time.Second, 5*time.Millisecond)
	qt.Assert(t, qt.ErrorMatches(err, `owners: http://web/api/v1 is not a configuration owner: .*`))
	qt.Check(t, qt.Equals(asked, 1))
}

// The prober over a real transport: the answer an owner gives is read, an
// answer that is not a section answer is no owner, and a refusal keeps its
// status so a 500 is retried and a 404 is not.
func TestTheProberReadsTheAnswerWithoutACredential(t *testing.T) {
	good := `{"section":"orders","schema":"orders-config/1","refersTo":[],"audience":"svc:orders","scopeKeys":["orders"]}`
	var asked []string
	get := func(_ context.Context, url string) (*configclient.Response, error) {
		asked = append(asked, url)
		switch {
		case strings.HasPrefix(url, "http://orders/"):
			return &configclient.Response{Status: http.StatusOK, Body: []byte(good)}, nil
		case strings.HasPrefix(url, "http://web/"):
			return &configclient.Response{Status: http.StatusOK, Body: []byte(`<html>welcome</html>`)}, nil
		case strings.HasPrefix(url, "http://halfway/"):
			return &configclient.Response{Status: http.StatusOK, Body: []byte(`{"section":"orders"}`)}, nil
		}

		return nil, errors.New("connection refused")
	}
	probe := owners.HTTPProber(get)

	a, err := probe(context.Background(), "http://orders/api/v1/")
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(a.Audience, "svc:orders"))
	qt.Check(t, qt.Equals(asked[0], "http://orders/api/v1/config/section"))

	for _, address := range []string{"http://web/api/v1", "http://halfway/api/v1"} {
		_, err = owners.Discover(context.Background(), []string{address}, probe, time.Second, 5*time.Millisecond)
		qt.Check(t, qt.ErrorMatches(err, `(?s)owners: `+address+` is not a configuration owner: .*`))
	}
}

// Only the scopes under the owner's keys travel to it; a chart box, another
// service's permission and a scope with no key never do.
func TestOnlyTheScopesUnderTheOwnersKeysTravel(t *testing.T) {
	o := &owners.Owner{Section: "people", ScopeKeys: []string{"people", "staff", "equipment"}}
	held := []string{
		"orders/setup:import", "people/setup:import", "staff:admin",
		"authority/person:view", "openid", "equipment/", "people/equipment/item:add",
		"people/setup:import", " equipment:read ", "peopleX/setup:import",
	}
	qt.Assert(t, qt.DeepEquals(o.Scopes(held), []string{
		"people/setup:import", "staff:admin", "people/equipment/item:add", "equipment:read",
	}))
	qt.Assert(t, qt.IsNil(o.Scopes([]string{"orders/setup:import"})))
}

// A held entry with a space inside it is not one scope: joined with the others
// it would carry a scope under another key to this owner, so it never travels.
func TestAnEntryHoldingTwoScopesNeverTravels(t *testing.T) {
	o := &owners.Owner{Section: "people", ScopeKeys: []string{"people"}}
	held := []string{"people:read membership:admin", "people:read\tmembership:admin", "people/setup:import"}

	qt.Assert(t, qt.DeepEquals(o.Scopes(held), []string{"people/setup:import"}))
}
