// Package configserver answers the tenant configuration contract for a service
// that owns a section: the five operations, mounted at the service's own
// versioned API root, each behind the service's own gate.
//
// The service keeps what is its own — the section's shape, its domain rules and
// the store that reads and writes it. This package keeps what the contract says
// about all of them: the section read carrying its version as the ETag, the
// version read, the preview that writes nothing, the apply that names the
// version it was previewed against and is refused without one, the answer
// naming the section that needs no token, the gate that lets members read and
// only the administering permission write, the record of every transport of the
// section in the owner's own history, and the refusals, each in the service's
// own error domain.
//
//	cfg := &configserver.Owner{
//		Section:   "orders",
//		Audience:  "svc:orders",
//		ScopeKeys: []string{"orders"},
//		Domain:    "orders",
//		Gate:    gate, // the service's permissions.Gate
//		Read:    permissions.Levels("orders", "read"),
//		Write:   configserver.ImportRule(set, permissions.Levels("orders", "admin")),
//		Tenant:  tenantOf,
//		Actor:   actorOf,
//		Store:   storeOf,
//	}
//	if err := cfg.Mount(v1, open); err != nil { … } // open: the same root, no authentication
package configserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"azugo.io/azugo"
	"github.com/valyala/fasthttp"

	"github.com/gmb-lib/go-authbyte/claims"
	"github.com/gmb-lib/go-authbyte/permissions"
	pkerrors "github.com/gmb-lib/go-platform-kit/errors"

	"github.com/gmb-lib/go-configbyte/contract"
)

// Store is the owner's own data, as the contract reads and writes it. The
// section and its version come from one snapshot, so the two never disagree.
type Store interface {
	// ConfigRead answers the tenant's section, verbatim as it travels, and the
	// version derived from exactly those bytes.
	ConfigRead(ctx context.Context, tenant string) (Read, error)
	// ConfigVersion answers the tenant's version alone.
	ConfigVersion(ctx context.Context, tenant string) (string, error)
	// ConfigApply previews (DryRun) or applies a document and answers the
	// report, verbatim as it travels ([contract.Report] is its shape). A
	// document with a refused item writes nothing; a preview never writes. A
	// writing apply compares Expected — never empty for one — with the version
	// the section has when it is about to be written, under a lock held per
	// tenant, and refuses with [contract.ReasonVersionMoved] when they differ.
	ConfigApply(ctx context.Context, a Apply) (json.RawMessage, error)
	// ConfigExported records, in the owner's own history, that its section was
	// read as part of an export: who took it out and the part's hash. The read
	// is answered only once the record is written, so an export is never made
	// unrecorded.
	ConfigExported(ctx context.Context, e Export) error
}

// Export is one read of the section as part of an export.
type Export struct {
	Tenant string
	// Actor is who took the section out, as the owner's history names people.
	Actor string
	// PartHash is the hash of the section as it was answered
	// ([contract.PartHash]), which is the part the exported file carries.
	PartHash string
}

// Read is a section and the version computed from exactly its bytes.
type Read struct {
	Section json.RawMessage
	Version string
}

// Apply is one preview or apply of a document.
type Apply struct {
	Tenant string
	// Actor is who the change is attributed to in the owner's history.
	Actor   string
	Section json.RawMessage
	DryRun  bool
	// Expected is the version the document was previewed against. Empty for a
	// preview, never for a writing apply.
	Expected string
	// PartHash is the hash of the section as it arrived ([contract.PartHash]),
	// and Document the content hash of the document it arrived in, as the
	// caller named it in [contract.HeaderDocument] — empty when none was named.
	// A writing apply records both on the owner's line for it, so the history
	// names the file an import came from.
	PartHash string
	Document string
}

// Rule is who may preview and apply a document. A service acting as itself
// passes by Level or by the permissions; a person never holds a level, so a
// person passes by the permissions alone.
type Rule struct {
	// Level is the rung of the service's own ladder a service acting as itself
	// passes by. The zero Level for a service with no ladder.
	Level permissions.Level
	// Perms are the permissions that let a caller write: one of them, or with
	// All set, every one.
	Perms []permissions.Perm
	All   bool
}

// Routes is where the operations are mounted: the service's versioned API root.
// An azugo.Router satisfies it.
type Routes interface {
	Get(path string, handler azugo.RequestHandler)
	Post(path string, handler azugo.RequestHandler)
}

// Owner is one service's section and how its operations are answered.
type Owner struct {
	// Section is the section's name, such as "orders". Its payload travels as
	// the name plus "-config/1".
	Section string
	// RefersTo names the sections whose keys this section names. Nil when it
	// names none.
	RefersTo []string
	// Audience is the audience a token for this service carries, such as
	// "svc:orders", and ScopeKeys the keys of every scope its routes check: the
	// groups of its ladder and its permissions' service key. The section answer
	// names both, so a coordinator can call this service on a person's behalf
	// with exactly the person's own scopes, knowing nothing but its address.
	Audience  string
	ScopeKeys []string
	// Domain is the error domain the service's refusals carry:
	// "err:<domain>:config_version_moved".
	Domain string

	// Gate is the service's permissions gate. The routes are checked through it,
	// so the permissions they accept are recorded with the service's own.
	Gate *permissions.Gate
	// Read is the rung a service acting as itself reads the section by. It also
	// reads it holding any permission the service declares. A person of the
	// tenant always may: the section is the vocabulary everybody works in, the
	// same for every member.
	Read permissions.Level
	// Write is who may preview and apply a document.
	Write Rule

	// Tenant answers the caller's tenant, or writes the refusal and answers
	// false.
	Tenant func(ctx *azugo.Context) (string, bool)
	// Actor answers who an apply is attributed to.
	Actor func(ctx *azugo.Context) string
	// Store answers the owner's store, or writes the refusal (not ready yet)
	// and answers false.
	Store func(ctx *azugo.Context) (Store, bool)
	// Fail renders a store failure. Nil hands it to ctx.Error.
	Fail func(ctx *azugo.Context, err error)
	// Applied is told of every apply that wrote, after it did — the place to
	// record it as a security event. Nil for none.
	Applied func(ctx *azugo.Context, tenant string)
	// IsMachine tells a service acting as itself from a person. Nil reads a
	// subject carrying the service prefix ("svc:") as a service.
	IsMachine func(ctx *azugo.Context) bool
}

// Mount checks the owner and registers the five operations: four on r, the
// service's versioned API root behind its authentication, and the section
// answer on open, the same root with no authentication — it is asked before the
// asker holds any credential for this service, and it says nothing a token
// would protect.
func (o *Owner) Mount(r, open Routes) error {
	answer, err := o.check()
	if err != nil {
		return err
	}
	if r == nil || open == nil {
		return fmt.Errorf("configuration: section %q is mounted on two routers: the authenticated root and the open one", o.Section)
	}

	read, export := o.reader(o.config), o.writer(o.export)
	r.Get(contract.PathConfig, func(ctx *azugo.Context) {
		switch strings.TrimSpace(string(ctx.Request().Header.Peek(contract.HeaderPurpose))) {
		case "":
			read(ctx)
		case contract.PurposeExport:
			export(ctx)
		default:
			ctx.Error(pkerrors.NewProblem("err:request:invalid",
				pkerrors.WithStatus(fasthttp.StatusBadRequest),
				pkerrors.WithPublicDetail(contract.HeaderPurpose+" names a purpose this service does not know; the one purpose is "+contract.PurposeExport)))
		}
	})
	r.Get(contract.PathVersion, o.reader(o.version))
	r.Post(contract.PathPreview, o.writer(o.preview))
	r.Post(contract.PathApply, o.writer(o.apply))
	open.Get(contract.PathSection, func(ctx *azugo.Context) { ctx.JSON(answer) })

	return nil
}

func (o *Owner) check() (contract.SectionAnswer, error) {
	refs := o.RefersTo
	if refs == nil {
		refs = []string{}
	}
	answer, err := contract.NewSectionAnswer(o.Section, o.Audience, o.ScopeKeys, refs)
	errs := []error{err}
	if strings.TrimSpace(o.Domain) == "" || strings.ContainsAny(o.Domain, ": ") {
		errs = append(errs, fmt.Errorf("configuration: section %q needs an error domain of one word", o.Section))
	}
	if o.Gate == nil {
		errs = append(errs, fmt.Errorf("configuration: section %q has no gate", o.Section))
	}
	if o.Write.Level.Holds == nil && len(o.Write.Perms) == 0 {
		errs = append(errs, fmt.Errorf("configuration: nobody could apply section %q: its write rule has no level and no permission", o.Section))
	}
	if o.Tenant == nil || o.Actor == nil || o.Store == nil {
		errs = append(errs, fmt.Errorf("configuration: section %q needs Tenant, Actor and Store", o.Section))
	}

	return answer, errors.Join(errs...)
}

func (o *Owner) machine(ctx *azugo.Context) bool {
	if o.IsMachine != nil {
		return o.IsMachine(ctx)
	}

	return strings.HasPrefix(strings.TrimSpace(ctx.User().ID()), claims.ServiceSubjectPrefix)
}

// reader guards the two reads: a person always reaches the handler, which asks
// for their tenant; a service acting as itself needs the reading rung or any
// permission the service declares.
func (o *Owner) reader(h azugo.RequestHandler) azugo.RequestHandler {
	machine := o.Gate.Member(o.Read, h)

	return func(ctx *azugo.Context) {
		if o.machine(ctx) {
			machine(ctx)

			return
		}
		h(ctx)
	}
}

// writer guards the preview and the apply with the write rule.
func (o *Owner) writer(h azugo.RequestHandler) azugo.RequestHandler {
	guard := o.Gate.OneOf
	if o.Write.All {
		guard = o.Gate.AllOf
	}
	machine := guard(o.Write.Level, h, o.Write.Perms...)
	person := guard(permissions.Level{}, h, o.Write.Perms...)

	return func(ctx *azugo.Context) {
		if o.machine(ctx) {
			machine(ctx)

			return
		}
		person(ctx)
	}
}

func (o *Owner) fail(ctx *azugo.Context, err error) {
	if o.Fail != nil {
		o.Fail(ctx, err)

		return
	}
	ctx.Error(err)
}

// config answers the tenant's section, verbatim as it travels, with its version
// in the ETag header.
func (o *Owner) config(ctx *azugo.Context) {
	tenant, ok := o.Tenant(ctx)
	if !ok {
		return
	}
	s, ok := o.Store(ctx)
	if !ok {
		return
	}

	res, err := s.ConfigRead(ctx, tenant)
	if err != nil {
		o.fail(ctx, err)

		return
	}

	ctx.Header.Set("ETag", contract.ETag(res.Version))
	ctx.JSON(res.Section)
}

// export answers the section exactly as the read does, once the owner has
// recorded who took it out and the hash of the part they took.
func (o *Owner) export(ctx *azugo.Context) {
	tenant, ok := o.Tenant(ctx)
	if !ok {
		return
	}
	s, ok := o.Store(ctx)
	if !ok {
		return
	}

	res, err := s.ConfigRead(ctx, tenant)
	if err != nil {
		o.fail(ctx, err)

		return
	}
	if err := s.ConfigExported(ctx, Export{Tenant: tenant, Actor: o.Actor(ctx), PartHash: contract.PartHash(res.Section)}); err != nil {
		o.fail(ctx, err)

		return
	}

	ctx.Header.Set("ETag", contract.ETag(res.Version))
	ctx.JSON(res.Section)
}

// version answers the version alone.
func (o *Owner) version(ctx *azugo.Context) {
	tenant, ok := o.Tenant(ctx)
	if !ok {
		return
	}
	s, ok := o.Store(ctx)
	if !ok {
		return
	}

	v, err := s.ConfigVersion(ctx, tenant)
	if err != nil {
		o.fail(ctx, err)

		return
	}

	ctx.JSON(contract.VersionAnswer{Version: v})
}

// preview answers exactly what applying the document would do, item by item,
// refusals included, and writes nothing. It is the apply's own dry pass, so the
// two can never disagree about a document.
func (o *Owner) preview(ctx *azugo.Context) { o.run(ctx, true) }

// apply applies the document atomically — one refused item and nothing is
// written — and idempotently: a document that changes nothing records nothing
// and leaves the version where it was.
func (o *Owner) apply(ctx *azugo.Context) { o.run(ctx, false) }

func (o *Owner) run(ctx *azugo.Context, dryRun bool) {
	tenant, ok := o.Tenant(ctx)
	if !ok {
		return
	}

	// A writing apply names the version its preview answered, as If-Match: the
	// ETag the section read carries. Refused here without one, before the store
	// is asked; the store compares it, under a lock, with the version the
	// section has when the document is about to be written.
	expected := ""
	if !dryRun {
		// Copied: the request's header bytes are reused once it is answered.
		expected = strings.Clone(contract.EntityTag(ctx.Header.Get("If-Match")))
		if expected == "" {
			ctx.Error(contract.Refusal(o.Domain, contract.ReasonVersionRequired,
				"an apply names the configuration version its preview answered, as If-Match; preview the document first"))

			return
		}
	}

	// The document the section arrived in, named by its content hash: recorded
	// on the owner's line for the apply, so the history names the file.
	document := strings.Clone(strings.TrimSpace(string(ctx.Request().Header.Peek(contract.HeaderDocument))))
	if document != "" && !contract.ValidHash(document) {
		ctx.Error(contract.Refusal(o.Domain, contract.ReasonBadDocument,
			contract.HeaderDocument+" names no content hash: it is sha256: and 64 lower-case hex digits"))

		return
	}

	s, ok := o.Store(ctx)
	if !ok {
		return
	}

	// Copied for the same reason, so a store may keep the document it is handed.
	body := bytes.Clone(ctx.Body.Bytes())
	if !json.Valid(body) {
		// The same reason the store answers a document it cannot read, so a
		// request fault renders exactly like a document fault.
		ctx.Error(contract.Refusal(o.Domain, contract.ReasonBadDocument, "the section is not valid JSON"))

		return
	}

	report, err := s.ConfigApply(ctx, Apply{
		Tenant: tenant, Actor: o.Actor(ctx), Section: json.RawMessage(body), DryRun: dryRun, Expected: expected,
		PartHash: contract.PartHash(body), Document: document,
	})
	if err != nil {
		o.fail(ctx, err)

		return
	}

	if !dryRun && o.Applied != nil {
		var out contract.Outcome
		if json.Unmarshal(report, &out) == nil && out.Applied {
			o.Applied(ctx, tenant)
		}
	}

	ctx.JSON(report)
}
