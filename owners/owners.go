// Package owners finds the configuration owners a deployment names and works
// out the order an import writes their sections in.
//
// A deployment names its owners by address and nothing else. Each one is asked
// which section it holds, which sections that section refers to, and what a
// token for it carries; the answers decide everything the service asking needs
// to know about an owner, so a new owner is a new address and never a new
// release.
package owners

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gmb-lib/go-configbyte/configclient"
	"github.com/gmb-lib/go-configbyte/contract"
)

// WidensAccess is the one section whose items grant access: the roles. Roles
// belong to the role register and no other section may name one, so its
// section is written after every other section that does not refer to it. The
// moment between two writes then never holds wider access than the document
// asks for.
const WidensAccess = "rolebyte"

// Owner is one configuration owner as it described itself at start.
type Owner struct {
	// Address is the owner's API root, the one the contract's five operations
	// sit under, such as "http://orders:8080/api/v1".
	Address string
	// Section is the section the owner holds; Schema the name its payload
	// carries.
	Section string
	Schema  string
	// RefersTo are the sections whose keys this section names. It is written
	// after each of them.
	RefersTo []string
	// Audience is what a token for this owner must name, and ScopeKeys the keys
	// of every scope its routes check.
	Audience  string
	ScopeKeys []string
}

// Scopes answers which of a person's scopes belong to this owner: each one whose
// key is one of the owner's scope keys. A call to the owner asks for exactly
// these, so the owner judges the person's own grant; a scope under any other
// key never travels to it.
func (o *Owner) Scopes(held []string) []string { return Under(o.ScopeKeys, held) }

// Under answers which of a person's scopes sit under one of the keys given —
// a scope's key is the part before its first "/" or ":" — each once, in the
// order they are held. A held entry with a space inside it is not one scope
// and is left out: scopes travel joined by spaces, so it would carry a scope
// under another key with it.
func Under(keys, held []string) []string {
	var out []string
	for _, s := range held {
		s = strings.TrimSpace(s)
		if strings.IndexFunc(s, unicode.IsSpace) >= 0 {
			continue
		}
		i := strings.IndexAny(s, "/:")
		if i <= 0 || i == len(s)-1 {
			continue
		}
		if slices.Contains(keys, s[:i]) && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}

	return out
}

// Set is every owner of a deployment, in the order an import writes them.
type Set struct {
	order  []*Owner
	byName map[string]*Owner
}

// Order answers the owners in the order an import writes their sections.
func (s *Set) Order() []*Owner { return slices.Clone(s.order) }

// Lookup answers the owner of a section.
func (s *Set) Lookup(section string) (*Owner, bool) {
	o, ok := s.byName[section]

	return o, ok
}

// Checks reports whether an owner of this deployment checks the scopes under a
// key.
func (s *Set) Checks(key string) bool {
	for _, o := range s.order {
		if slices.Contains(o.ScopeKeys, key) {
			return true
		}
	}

	return false
}

// Prober asks one address which section it holds.
type Prober func(ctx context.Context, address string) (contract.SectionAnswer, error)

// Discover asks every address which section it holds and arranges the answers.
//
// An owner may start after the service asking, so an address that does not answer —
// nothing listening yet, or an answer of 500 or above — is asked again until
// the window closes. An address that answers with anything else and is not a
// configuration owner fails at once: waiting would not change the answer.
func Discover(ctx context.Context, addresses []string, probe Prober, window, every time.Duration) (*Set, error) {
	if len(addresses) == 0 {
		return nil, errors.New("owners: no configuration owner is named; name each owner's API root")
	}

	deadline := time.Now().Add(window)
	found := make([]*Owner, 0, len(addresses))
	for _, address := range addresses {
		answer, err := ask(ctx, address, probe, deadline, every)
		if err != nil {
			return nil, err
		}
		found = append(found, &Owner{
			Address: address, Section: answer.Section, Schema: answer.Schema,
			RefersTo: slices.Clone(answer.RefersTo), Audience: answer.Audience,
			ScopeKeys: slices.Clone(answer.ScopeKeys),
		})
	}

	return Arrange(found)
}

// ask asks one address until it answers or the deadline passes.
func ask(ctx context.Context, address string, probe Prober, deadline time.Time, every time.Duration) (contract.SectionAnswer, error) {
	for {
		answer, err := probe(ctx, address)
		if err == nil {
			return answer, nil
		}
		if !retryable(err) {
			return contract.SectionAnswer{}, fmt.Errorf("owners: %s is not a configuration owner: %w", address, err)
		}
		if !time.Now().Add(every).Before(deadline) {
			return contract.SectionAnswer{}, fmt.Errorf("owners: %s did not answer in time: %w", address, err)
		}

		select {
		case <-ctx.Done():
			return contract.SectionAnswer{}, fmt.Errorf("owners: %s: %w", address, ctx.Err())
		case <-time.After(every):
		}
	}
}

// retryable reports whether a failed probe may succeed later: nothing answered,
// or the owner answered that it is not able to.
func retryable(err error) bool {
	var refused *configclient.Refused
	if errors.As(err, &refused) {
		return refused.Status >= 500
	}
	var notOwner *notAnOwner

	return !errors.As(err, &notOwner)
}

// notAnOwner is an answer that was given and is not a section answer.
type notAnOwner struct{ err error }

func (e *notAnOwner) Error() string { return e.err.Error() }
func (e *notAnOwner) Unwrap() error { return e.err }

// Arrange checks that the answers describe one deployment and works out the
// order an import writes them in: a section after every section it refers to;
// among sections with no reference between them the role register's last; the
// rest by section name, so every start arrives at the same order.
//
// It refuses two owners of one section, a reference to a section no owner
// holds, and sections that refer to each other in a circle.
func Arrange(found []*Owner) (*Set, error) {
	byName := make(map[string]*Owner, len(found))
	for _, o := range found {
		if prev, ok := byName[o.Section]; ok {
			return nil, fmt.Errorf("owners: %s and %s both hold the section %q", prev.Address, o.Address, o.Section)
		}
		byName[o.Section] = o
	}

	waiting := make(map[string]int, len(found))
	referredBy := make(map[string][]string, len(found))
	for _, o := range found {
		for _, ref := range o.RefersTo {
			if _, ok := byName[ref]; !ok {
				return nil, fmt.Errorf("owners: the section %q refers to %q, which no owner holds", o.Section, ref)
			}
			waiting[o.Section]++
			referredBy[ref] = append(referredBy[ref], o.Section)
		}
	}

	ready := make([]string, 0, len(found))
	for _, o := range found {
		if waiting[o.Section] == 0 {
			ready = append(ready, o.Section)
		}
	}

	order := make([]*Owner, 0, len(found))
	for len(ready) > 0 {
		next := pick(ready)
		ready = slices.DeleteFunc(ready, func(s string) bool { return s == next })
		order = append(order, byName[next])
		for _, s := range referredBy[next] {
			waiting[s]--
			if waiting[s] == 0 {
				ready = append(ready, s)
			}
		}
	}

	if len(order) < len(found) {
		var circle []string
		for name, n := range waiting {
			if n > 0 {
				circle = append(circle, name)
			}
		}
		sort.Strings(circle)

		return nil, fmt.Errorf("owners: the sections %s refer to each other in a circle", strings.Join(circle, ", "))
	}

	return &Set{order: order, byName: byName}, nil
}

// pick chooses the next section to write among those whose references are all
// written: the first by name, the role register's only when nothing else is
// ready.
func pick(ready []string) string {
	sorted := slices.Clone(ready)
	sort.Strings(sorted)
	for _, s := range sorted {
		if s != WidensAccess {
			return s
		}
	}

	return sorted[0]
}

// HTTPProber asks an address over the transport given, with no credential: the
// section answer is given before the asker holds one.
func HTTPProber(get func(ctx context.Context, url string) (*configclient.Response, error)) Prober {
	return func(ctx context.Context, address string) (contract.SectionAnswer, error) {
		answered := false
		fetch := func(ctx context.Context, path string) (*configclient.Response, error) {
			resp, err := get(ctx, strings.TrimRight(address, "/")+path)
			if err == nil {
				answered = true
			}

			return resp, err
		}

		answer, err := configclient.Section(ctx, fetch)
		if err != nil && answered {
			var refused *configclient.Refused
			if !errors.As(err, &refused) {
				return contract.SectionAnswer{}, &notAnOwner{err: err}
			}
		}

		return answer, err
	}
}
