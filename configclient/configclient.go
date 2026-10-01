// Package configclient reads other owners' configuration sections: for a
// sign-in read that hands an application its organisation's vocabulary in one
// answer, for an export, and for a coordinator checking at start that every
// address it was given answers the section it expects.
//
// The caller brings the transport. A [Fetch] reads one path below an owner's
// versioned API root on the caller's own authority — for a person, a delegated
// token for that person — so the owner answers exactly what it would answer the
// person directly. Nothing here widens what anybody may read.
//
// [Cache] keeps one copy of each section per organisation and hands a copy out
// only against the version its owner has just confirmed:
//
//	body, fromCopy, err := cache.Current(ctx, tenant, "orders", fetch)
//
// asks the owner for the version, answers the copy when it was stored under
// that version, and otherwise reads the section afresh and keeps it under the
// version its owner stamped on those very bytes.
package configclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gmb-lib/go-configbyte/contract"
)

// Response is an owner's answer, as the caller's transport received it.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Fetch reads one path below an owner's versioned API root, such as
// contract.PathVersion, on the caller's own authority. It answers every answer
// the owner gave, a refusal included; an error is only for a request that was
// never answered.
type Fetch func(ctx context.Context, path string) (*Response, error)

// Refused is an owner's answer at or above 400: the owner was reached, it
// understood the request, and it refused or failed it. That is a decision about
// the request, so a caller relays it as the owner gave it — Body is the owner's
// own problem document, unparsed.
//
// A request that was never answered is not this type; it is the transport's
// own error, wrapped.
type Refused struct {
	// Path is the operation that was refused.
	Path   string
	Status int
	Header http.Header
	Body   []byte
}

// Error states the fact and nothing else: the body is the owner's own wording,
// and an error message ends up in logs kept by callers who never chose to
// publish it, so it stays in its field.
func (e *Refused) Error() string {
	return fmt.Sprintf("configuration: %s answered %d", e.Path, e.Status)
}

func call(ctx context.Context, fetch Fetch, path string) (*Response, error) {
	resp, err := fetch(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("configuration: %s: %w", path, err)
	}
	if resp == nil {
		return nil, fmt.Errorf("configuration: %s: no answer", path)
	}
	if resp.Status >= http.StatusBadRequest {
		return nil, &Refused{Path: path, Status: resp.Status, Header: resp.Header, Body: resp.Body}
	}

	return resp, nil
}

// Version asks an owner for its version. An answer that carries none reads as
// "", which matches nothing a cache holds.
func Version(ctx context.Context, fetch Fetch) (string, error) {
	resp, err := call(ctx, fetch, contract.PathVersion)
	if err != nil {
		return "", err
	}
	var v contract.VersionAnswer
	_ = json.Unmarshal(resp.Body, &v)

	return v.Version, nil
}

// Read reads an owner's section and the version its owner stamped on those very
// bytes, from the ETag.
func Read(ctx context.Context, fetch Fetch) (section []byte, version string, err error) {
	resp, err := call(ctx, fetch, contract.PathConfig)
	if err != nil {
		return nil, "", err
	}

	return resp.Body, contract.EntityTag(resp.Header.Get("ETag")), nil
}

// Section asks an owner which section it holds and which sections it refers to,
// and checks the answer is one a configuration owner gives. It needs no tenant
// and no person, so a coordinator can ask it at start.
func Section(ctx context.Context, fetch Fetch) (contract.SectionAnswer, error) {
	resp, err := call(ctx, fetch, contract.PathSection)
	if err != nil {
		return contract.SectionAnswer{}, err
	}
	var a contract.SectionAnswer
	if err := json.Unmarshal(resp.Body, &a); err != nil {
		return contract.SectionAnswer{}, fmt.Errorf("configuration: %s: the answer is not a section answer: %w", contract.PathSection, err)
	}
	if err := a.Check(); err != nil {
		return contract.SectionAnswer{}, err
	}

	return a, nil
}

// Current answers one organisation's section as its owner holds it now: the
// copy kept here when the owner's current version is the one it was stored
// under, otherwise the section read afresh and kept under the version that came
// with those bytes — not the one asked a moment earlier, since a change landing
// between the two calls would otherwise file new content under an old version.
// fromCopy says which.
//
// The copy saves the transfer, never the question: the owner is asked for its
// version every time, on the caller's authority. Whether this caller may be
// answered the section at all is decided before Current is asked. A refusal or
// failure at either call ends the read, leaving no copy behind.
func (c *Cache) Current(ctx context.Context, tenant, section string, fetch Fetch) (body []byte, fromCopy bool, err error) {
	version, err := Version(ctx, fetch)
	if err != nil {
		return nil, false, err
	}
	if held, ok := c.Get(tenant, section, version); ok {
		return held, true, nil
	}

	body, stamped, err := Read(ctx, fetch)
	if err != nil {
		return nil, false, err
	}
	c.Put(tenant, section, stamped, body)

	return body, false, nil
}
