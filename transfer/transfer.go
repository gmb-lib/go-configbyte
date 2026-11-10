// Package transfer carries a tenant's whole configuration between
// installations as one document: an export of every owner's section, and an
// import that previews the document and then applies it whole or not at all.
//
// It only orchestrates. Each owner answers and writes its own section, on the
// authority of the person the transfer acts for, applies its own rules to it,
// and records the transport of its own part in its own history; this package
// keeps nothing.
//
// The service hosting it brings two things: the owners of its deployment
// ([owners.Set]) and a [Caller], which makes one call to an owner on the
// person's behalf — a coordinator with its session's delegated token, a
// backend-for-frontend with its own token exchange. The package never sees a
// session, a cookie or a request; the routes, the session and the error
// rendering stay with the host, and [Problem] turns this package's errors into
// the problem answers every host gives alike.
//
// The document is not a security boundary (the permission to import one is): a
// hand-edited file is the feature working, and the content hash only answers,
// honestly, whether the document is still the exact export.
package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gmb-lib/go-configbyte/configclient"
	"github.com/gmb-lib/go-configbyte/contract"
	"github.com/gmb-lib/go-configbyte/document"
	"github.com/gmb-lib/go-configbyte/owners"
)

// Caller makes one call to an owner, below its API root (o.Address), on the
// person's behalf and asking for exactly the scope given: the person's own
// scopes under the owner's keys, space-separated. It answers every answer the
// owner gave, a refusal included; an error is only for a call that produced no
// owner's answer — the authority refusing the token, or the hop failing.
//
// The host builds it per request, bound to the person; the person's subject and
// token never pass through this package. The header it is handed is never nil.
//
// The token authority's refusal to act for the person is told from a failed
// hop by its type: go-authbyte's *authclient.Error with a 4xx status, which is
// what go-authbyte's clients answer. A host that exchanges tokens another way
// answers its authority's refusal as one, or [Problem] answers it as a failed
// hop (502).
type Caller interface {
	Call(ctx context.Context, o *owners.Owner, scope, method, path string, header http.Header, body []byte) (*configclient.Response, error)
}

// CallerFunc is a function that is a [Caller].
type CallerFunc func(ctx context.Context, o *owners.Owner, scope, method, path string, header http.Header, body []byte) (*configclient.Response, error)

// Call calls f.
func (f CallerFunc) Call(ctx context.Context, o *owners.Owner, scope, method, path string, header http.Header, body []byte) (*configclient.Response, error) {
	return f(ctx, o, scope, method, path, header, body)
}

// Person is who a transfer acts for.
type Person struct {
	// Tenant is the organisation the document belongs to.
	Tenant string
	// Scopes is every scope the person holds. Each owner is asked with those
	// under its own keys, and an owner the person holds nothing of is not asked.
	Scopes []string
}

// Transfer exports and imports the configuration of one deployment's owners.
type Transfer struct {
	// Owners is every owner of the deployment, in the order an import writes
	// them. Required: without it an export and an import answer an error.
	Owners *owners.Set
	// Warn is told of a call that produced no owner's answer and of an import
	// that had to restore what it wrote. Nil tells nobody.
	Warn func(msg string)
	// AfterRequest bounds each call made after the request may have ended — a
	// restore, a read-back — which no longer runs under the request's deadline.
	// Zero means 30 seconds.
	AfterRequest time.Duration
}

// detached is the context for one call that must finish even when the request
// has ended, bounded by the transfer's own limit instead of the request's.
func (t *Transfer) detached(ctx context.Context) (context.Context, context.CancelFunc) {
	limit := t.AfterRequest
	if limit <= 0 {
		limit = 30 * time.Second
	}

	return context.WithTimeout(context.WithoutCancel(ctx), limit)
}

// File is an exported document, ready to answer as a download.
type File struct {
	// Name is the file name a download offers, such as
	// "configuration-2026-11-10.json".
	Name string
	// Body is the document, indented on purpose: it is written to be read and
	// hand-edited by the administrator preparing a tenant.
	Body []byte
	// Hash is the document's content hash.
	Hash string
}

// Imported is the outcome of an import: the answer for the whole document, and
// the content hash of the sections that arrived, which each owner recorded
// beside its own part.
type Imported struct {
	Answer document.Answer
	Hash   string
}

// Section is one section of the deployment as an administration application
// lists it.
type Section struct {
	Section  string   `json:"section"`
	Schema   string   `json:"schema"`
	RefersTo []string `json:"refersTo"`
}

// Sections answers every section of the deployment in the order an import
// writes them — a section after the ones it refers to, the role register's
// last — which is also the order an application draws its parts in. It names
// what each owner said at start and nothing of how it is reached.
func (t *Transfer) Sections() []Section {
	lines := []Section{}
	if t.Owners == nil {
		return lines
	}
	for _, o := range t.Owners.Order() {
		refers := slices.Clone(o.RefersTo)
		if refers == nil {
			refers = []string{}
		}
		lines = append(lines, Section{Section: o.Section, Schema: o.Schema, RefersTo: refers})
	}

	return lines
}

// Export composes the tenant's configuration document: the section of every
// owner the person holds anything of, each read as an export so the owner
// records who took its part out, and the content hash. An owner the person
// holds nothing of is left out; an owner that refuses the export, or cannot be
// reached, ends it, because half a document would be worse than none.
//
// It answers [ErrNothingHeld] before any call when the person holds nothing any
// owner accepts, [*OwnerRefused] when an owner refused its part, and
// [*Unreached] when a call produced no owner's answer.
func (t *Transfer) Export(ctx context.Context, p Person, c Caller, now time.Time) (*File, error) {
	if t.Owners == nil {
		return nil, errNoOwners
	}
	if !t.heldUnderAnyOwner(p) {
		return nil, ErrNothingHeld
	}

	sections := map[string]json.RawMessage{}
	for _, o := range t.Owners.Order() {
		scope := strings.Join(o.Scopes(p.Scopes), " ")
		if scope == "" {
			continue
		}
		body, err := t.exportSection(ctx, c, o, scope)
		if err != nil {
			return nil, err
		}
		sections[o.Section] = body
	}

	now = now.UTC()
	env := document.New(sections, p.Tenant, now)
	doc, err := env.Encode()
	if err != nil {
		return nil, err
	}

	return &File{Name: "configuration-" + now.Format("2006-01-02") + ".json", Body: doc, Hash: env.ContentHash}, nil
}

// exportSection reads one owner's section as part of an export. The owner
// answers it only to someone who may apply the section, and only once it has
// recorded the export.
func (t *Transfer) exportSection(ctx context.Context, c Caller, o *owners.Owner, scope string) (json.RawMessage, error) {
	header := http.Header{}
	header.Set(contract.HeaderPurpose, contract.PurposeExport)

	var unanswered error
	fetch := func(ctx context.Context, path string) (*configclient.Response, error) {
		resp, err := call(ctx, c, o, scope, http.MethodGet, path, header, nil)
		if err != nil {
			unanswered = err

			return nil, err
		}

		return resp, nil
	}

	body, _, err := configclient.Read(ctx, fetch)
	if err != nil {
		var refused *configclient.Refused
		if errors.As(err, &refused) {
			return nil, &OwnerRefused{Section: o.Section, Status: refused.Status, Body: refused.Body}
		}
		if unanswered != nil {
			err = unanswered
		}

		return nil, t.unreached(o.Section, "configuration export from the "+o.Section+" owner", err)
	}

	return json.RawMessage(body), nil
}

// ownedSection is one section of a document that an owner here will judge.
type ownedSection struct {
	owner *owners.Owner
	raw   json.RawMessage
	// scope is what every call about this section asks for: the person's scopes
	// under the owner's keys.
	scope string
	// held is the section as its owner had it just before the apply wrote
	// anything — what a restore writes back.
	held json.RawMessage
}

// Import previews (dryRun) or applies a configuration document, and answers one
// outcome for the whole document:
//
//   - a dry run fans every section out to its owner's preview and answers each
//     report with the version the owner holds, which is what an apply names;
//   - an apply names, in the document's `expect`, the version each section's
//     dry run answered. It reads every section first and stops, writing
//     nothing, when one has moved; it previews every section and writes none
//     when one is refused; it then writes them in the owners' order, each under
//     its expected version and naming the document it came in; and when a later
//     write fails it restores the sections already written to what it read
//     before, so a document lands whole or not at all.
//
// A section no owner here holds, or whose owner the person holds nothing of, is
// reported skipped, never dropped. A refusal inside the document is an
// outcome, not an error.
//
// It answers [ErrNothingHeld] before any call when the person holds nothing any
// owner accepts; [*document.Invalid] for a document that cannot honestly be
// interpreted; and, for an apply, [*VersionRequired], [*VersionMoved],
// [*OwnerRefused] or [*Unreached] when the request is refused before anything
// is written.
func (t *Transfer) Import(ctx context.Context, p Person, c Caller, doc []byte, dryRun bool) (*Imported, error) {
	if t.Owners == nil {
		return nil, errNoOwners
	}
	if !t.heldUnderAnyOwner(p) {
		return nil, ErrNothingHeld
	}

	env, err := document.Parse(doc)
	if err != nil {
		return nil, err
	}

	// The honest edited flag: present only when the document carries a hash to
	// compare against. Never a gate — an edited document is the feature working.
	documentEdited := env.Edited()

	// The hash is of the sections that arrived, not the one the file claims, so
	// an unedited document names the export it came from and an edited one does
	// not pretend to. Each owner records it beside its own part.
	documentHash := document.ContentHash(env.Sections)

	results := map[string]document.SectionResult{}
	owned := t.ownedSections(p, env.Sections, results)

	var outcome document.Outcome
	if dryRun {
		outcome = t.previewDocument(ctx, c, owned, results)
	} else {
		outcome, err = t.applyDocument(ctx, p, c, owned, env.Expect, documentHash, results)
		if err != nil {
			return nil, err
		}
	}

	return &Imported{
		Answer: document.Answer{
			DocumentEdited: documentEdited,
			DryRun:         dryRun,
			Outcome:        outcome,
			Sections:       results,
		},
		Hash: documentHash,
	}, nil
}

// heldUnderAnyOwner reports whether the person holds a scope under any owner's
// keys. Someone who holds nothing any owner accepts is refused before anything
// is called, rather than answered as if the deployment had nothing for them.
func (t *Transfer) heldUnderAnyOwner(p Person) bool {
	for _, o := range t.Owners.Order() {
		if len(o.Scopes(p.Scopes)) > 0 {
			return true
		}
	}

	return false
}

// ownedSections picks, in the owners' order, the sections an owner here will
// judge, and reports every other one skipped: a section no owner here holds,
// and one whose owner the person holds nothing of.
func (t *Transfer) ownedSections(p Person, sections map[string]json.RawMessage, results map[string]document.SectionResult) []*ownedSection {
	for name := range sections {
		o, ok := t.Owners.Lookup(name)
		switch {
		case !ok:
			// Another deployment's section, or one nothing owns yet.
			results[name] = document.SectionResult{Status: document.SectionSkipped,
				Detail: "no service in this deployment owns this section"}
		case len(o.Scopes(p.Scopes)) == 0:
			results[name] = document.SectionResult{Status: document.SectionSkipped,
				Detail: "this account holds nothing the owner of this section accepts, so it was not applied"}
		}
	}

	var owned []*ownedSection
	for _, o := range t.Owners.Order() {
		raw, ok := sections[o.Section]
		if !ok {
			continue
		}
		if scope := strings.Join(o.Scopes(p.Scopes), " "); scope != "" {
			owned = append(owned, &ownedSection{owner: o, raw: raw, scope: scope})
		}
	}

	return owned
}

// previewDocument asks every owner what its section would do and answers the
// document's outcome: previewed, refused when any owner refused an item, or
// failed when an owner could not be asked.
func (t *Transfer) previewDocument(ctx context.Context, c Caller, owned []*ownedSection, results map[string]document.SectionResult) document.Outcome {
	outcome := document.Previewed
	for _, s := range owned {
		res := t.previewSection(ctx, c, s)
		results[s.owner.Section] = res
		switch {
		case res.Status == document.SectionFailed:
			outcome = document.Failed
		case res.Status == document.SectionRefused && outcome == document.Previewed:
			outcome = document.Refused
		}
	}

	return outcome
}

// applyDocument writes a document whole or not at all. It answers the outcome,
// or an error when the request itself is refused before anything is written:
// no expected version, a section that moved, or a section that could not be
// read.
func (t *Transfer) applyDocument(ctx context.Context, p Person, c Caller, owned []*ownedSection, expect map[string]string, documentHash string, results map[string]document.SectionResult) (document.Outcome, error) {
	for _, s := range owned {
		if expect[s.owner.Section] == "" {
			return "", &VersionRequired{Section: s.owner.Section}
		}
	}

	// Read what every section holds now: what a restore would write back, and
	// the proof it is still the configuration that was previewed. The version is
	// the ETag with its quotes removed and nothing else, so a weak tag never
	// matches the version a dry run answered.
	for _, s := range owned {
		resp, err := call(ctx, c, s.owner, s.scope, http.MethodGet, contract.PathConfig, nil, nil)
		if err != nil {
			return "", t.unreached(s.owner.Section, "configuration read from the "+s.owner.Section+" owner", err)
		}
		if resp.Status >= http.StatusBadRequest {
			return "", &OwnerRefused{Section: s.owner.Section, Status: resp.Status, Body: resp.Body}
		}
		if strings.Trim(resp.Header.Get("ETag"), `"`) != expect[s.owner.Section] {
			return "", &VersionMoved{Section: s.owner.Section}
		}
		s.held = json.RawMessage(resp.Body)
	}

	// Judge the whole document before writing any of it.
	blocked := false
	for _, s := range owned {
		res := t.previewSection(ctx, c, s)
		results[s.owner.Section] = res
		if res.Status != document.SectionPreviewed {
			blocked = true
		}
	}
	if blocked {
		outcome := document.Refused
		for _, s := range owned {
			switch results[s.owner.Section].Status {
			case document.SectionPreviewed:
				res := results[s.owner.Section]
				res.Status = document.SectionWithheld
				results[s.owner.Section] = res
			case document.SectionFailed:
				outcome = document.Failed
			}
		}

		return outcome, nil
	}

	// Write, in order, each under the version it was previewed at.
	for i, s := range owned {
		res, answered := t.applySection(ctx, c, s, s.raw, expect[s.owner.Section], documentHash)
		results[s.owner.Section] = res
		if res.Status == document.SectionApplied {
			continue
		}
		for _, later := range owned[i+1:] {
			withheld := results[later.owner.Section]
			withheld.Status = document.SectionWithheld
			results[later.owner.Section] = withheld
		}

		// A write without a clear answer may have landed all the same.
		unsure := !answered && t.mayHaveLanded(ctx, p, c, s, expect[s.owner.Section], results)

		outcome := t.restore(ctx, c, owned[:i], documentHash, results)
		if unsure {
			outcome = document.Partial
		}
		if i > 0 {
			t.warn("configuration import compensated after a failed section (tenant " + p.Tenant + ", " + string(outcome) + ")")
		}

		return outcome, nil
	}

	return document.Applied, nil
}

// mayHaveLanded reads back a section whose write had no clear answer — no
// answer at all, a server error, or an answer that could not be read: a timeout
// after the owner had already written it, say. When its version is still the
// one the apply named, the write has not landed and nothing more is needed;
// a write still held up somewhere could land later, which only the owner
// could rule out. Otherwise the section may hold this document's version: it
// moved (and a move cannot be told from someone else's change made in between,
// so it is never written back over), or it cannot be read at all. Its line
// says so, and the import is partial rather than rolled back.
func (t *Transfer) mayHaveLanded(ctx context.Context, p Person, c Caller, s *ownedSection, version string, results map[string]document.SectionResult) bool {
	ctx, cancel := t.detached(ctx)
	defer cancel()
	res := results[s.owner.Section]

	const check = " Preview the document again: a section with nothing left to change already holds it."
	resp, err := call(ctx, c, s.owner, s.scope, http.MethodGet, contract.PathConfig, nil, nil)
	switch {
	case err == nil && resp.Status < http.StatusBadRequest && strings.Trim(resp.Header.Get("ETag"), `"`) == version:
		return false
	case err == nil && resp.Status < http.StatusBadRequest:
		res.Version = strings.Trim(resp.Header.Get("ETag"), `"`)
		res.Detail = "its write had no clear answer, and it changed after the preview: it may hold this document's version, and was not written back over." + check
	default:
		res.Detail = "its write had no clear answer, and it could not be read back: it may hold this document's version." + check
	}
	results[s.owner.Section] = res
	t.warn("configuration import: the " + s.owner.Section + " section's write had no clear answer and it may hold the document (tenant " + p.Tenant + ")")

	return true
}

// restore writes back what each already-written section held before the apply,
// newest first, each under the version its own apply answered — so a restore
// never overwrites a change someone else made in between. A restore is an
// apply, so each owner records it as one, naming the same document. It answers
// rolledBack when every section is back, failed when nothing had been written,
// and partial when a restore itself failed.
//
// The restore runs even when the request's context has ended — the person
// gone, or its deadline passed — because sections already written must not be
// left half applied; each call is bounded by AfterRequest instead.
func (t *Transfer) restore(ctx context.Context, c Caller, written []*ownedSection, documentHash string, results map[string]document.SectionResult) document.Outcome {
	if len(written) == 0 {
		return document.Failed
	}

	outcome := document.RolledBack
	for i := len(written) - 1; i >= 0; i-- {
		s := written[i]
		res := results[s.owner.Section]
		cctx, cancel := t.detached(ctx)
		back, answered := t.applySection(cctx, c, s, s.held, res.Version, documentHash)
		cancel()
		switch {
		case back.Status == document.SectionApplied:
			res.Status = document.SectionRolledBack
			res.Version = back.Version
			res.Detail = "applied, then restored to what it held before this import because a later section failed"
		case answered:
			outcome = document.Partial
			res.Detail = "applied, and could not be restored after a later section failed — this section holds the document's version, or a change made since"
			res.Problem = back.Problem
		default:
			outcome = document.Partial
			res.Detail = "applied, and could not be restored after a later section failed: its restore had no clear answer, so it may hold the document's version or what it held before"
			res.Problem = back.Problem
		}
		results[s.owner.Section] = res
	}

	return outcome
}

// previewSection asks one owner what its section would do. A refusal inside
// the document is a 200 with the report saying `refused: true`; a refused or
// failed call keeps the public part of the owner's problem.
func (t *Transfer) previewSection(ctx context.Context, c Caller, s *ownedSection) document.SectionResult {
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	resp, err := call(ctx, c, s.owner, s.scope, http.MethodPost, contract.PathPreview, header, s.raw)
	if err != nil {
		t.warn("configuration import: section preview failed: " + err.Error())

		return document.SectionResult{Status: document.SectionFailed, Problem: unavailable()}
	}
	if resp.Status >= http.StatusBadRequest {
		return document.SectionResult{Status: document.SectionFailed, Problem: publicProblem(resp.Status, resp.Body)}
	}

	var rep contract.Outcome
	_ = json.Unmarshal(resp.Body, &rep)
	status := document.SectionPreviewed
	if rep.Refused {
		status = document.SectionRefused
	}

	return document.SectionResult{Status: status, Version: rep.Version, Report: json.RawMessage(resp.Body)}
}

// applySection writes one section under the version it names, naming the
// document it came in so the owner's record of the apply names the file. An
// owner that declines the document answers a 200 with `applied: false`; one
// whose configuration moved past the version answers 412 — both are a section
// that did not land, which the caller answers for the whole document. It also
// answers whether the answer is clear: an owner's decision (a 2xx it could be
// read from, a 4xx), or the token authority refusing a call never sent. No
// answer, a server error or a 2xx that cannot be read is not clear: such a
// write may have landed.
func (t *Transfer) applySection(ctx context.Context, c Caller, s *ownedSection, body json.RawMessage, version, documentHash string) (document.SectionResult, bool) {
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("If-Match", contract.ETag(version))
	header.Set(contract.HeaderDocument, documentHash)
	resp, err := call(ctx, c, s.owner, s.scope, http.MethodPost, contract.PathApply, header, body)
	if err != nil {
		t.warn("configuration import: section apply failed: " + err.Error())
		_, refusedByAuthority := authorityRefusal(err)

		return document.SectionResult{Status: document.SectionFailed, Problem: unavailable()}, refusedByAuthority
	}
	if resp.Status >= http.StatusBadRequest {
		return document.SectionResult{Status: document.SectionFailed, Problem: publicProblem(resp.Status, resp.Body)},
			resp.Status < http.StatusInternalServerError
	}

	var rep contract.Outcome
	if err := json.Unmarshal(resp.Body, &rep); err != nil {
		return document.SectionResult{Status: document.SectionFailed, Problem: unavailable()}, false
	}
	status := document.SectionApplied
	if !rep.Applied {
		status = document.SectionRefused
	}

	return document.SectionResult{Status: status, Version: rep.Version, Report: json.RawMessage(resp.Body)}, true
}

var (
	errNoOwners = errors.New("transfer: no owners are set")
	errNoScope  = errors.New("transfer: a call to an owner names no scope")
	errNoAnswer = errors.New("transfer: the caller answered neither an answer nor an error")
)

// call is every call the transfer makes to an owner. It never asks with an
// empty scope — a token exchange asked for no scope answers the caller's whole
// grant — hands the caller a header that is never nil, and treats a caller
// that answers nothing as a call that produced no owner's answer.
func call(ctx context.Context, c Caller, o *owners.Owner, scope, method, path string, header http.Header, body []byte) (*configclient.Response, error) {
	if strings.TrimSpace(scope) == "" {
		return nil, errNoScope
	}
	if header == nil {
		header = http.Header{}
	}
	resp, err := c.Call(ctx, o, scope, method, path, header, body)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errNoAnswer
	}
	if resp.Header == nil {
		resp.Header = http.Header{}
	}

	return resp, nil
}

// unavailable is the problem a section's line carries when its owner could not
// be asked at all.
func unavailable() json.RawMessage {
	return json.RawMessage(`{"code":"err:upstream:unavailable","status":502}`)
}

func (t *Transfer) warn(msg string) {
	if t.Warn != nil {
		t.Warn(msg)
	}
}
