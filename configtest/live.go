package configtest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/go-quicktest/qt"
	"github.com/valyala/fasthttp"

	"github.com/gmb-lib/go-configbyte/contract"
)

// Live walks the contract end to end against a real store. The tenant's section
// must hold at least the items the Refusals and the Change edit, authored
// through the service's ordinary acts before Run.
type Live struct {
	Service
	// Reader reads the section; Admin previews and applies; NonAdmin reads and
	// is refused the apply; OtherTenant reads another tenant's section. All but
	// OtherTenant are of one tenant.
	Reader, Admin, NonAdmin, OtherTenant Caller
	// Parts are the lists the report carries, compared item by item between a
	// preview and an apply of the same document.
	Parts []string
	// Refusals are documents the owner's own rules refuse, each an edit of the
	// section as read.
	Refusals []Refusal
	// Beside adds one clean item to a document, and Landed reports whether that
	// item was written: a refused document writes nothing, not even it.
	Beside func(doc map[string]any)
	Landed func(t testing.TB) bool
	// Change is one real change.
	Change Change

	// Exports answers the part hashes of the export lines the owner has
	// recorded in its history for Admin's tenant, oldest first, and Imports the
	// hashes on its import lines. Both are required: every owner records each
	// transport of its part.
	Exports func(t testing.TB) []string
	Imports func(t testing.TB) []Import
}

// Import is what one import line in the owner's history carries.
type Import struct {
	// PartHash is the hash of the part as it arrived, Document the content hash
	// of the document it arrived in, or "" when the apply named none.
	PartHash, Document string
}

// Refusal is one document the owner must refuse.
type Refusal struct {
	// Name says what the document gets wrong, for the failure message.
	Name string
	// Edit makes the document from the section as read.
	Edit func(doc map[string]any)
	// Part, Key and Reason are the report line the refusal must be: the item
	// named by key, refused with the reason, saying why.
	Part, Key, Reason string
}

// Change is one real change to the section.
type Change struct {
	// Edit makes the changed document from the section as read.
	Edit func(doc map[string]any)
	// Part, Index and Detail are the report line the change must be: changed,
	// naming the members that change.
	Part   string
	Index  int
	Detail string
	// Landed reports whether the change is where the service's screens read it.
	Landed func(t testing.TB) bool
}

// Run walks every rung, in order; later rungs build on earlier ones.
func (l Live) Run(t testing.TB) {
	t.Helper()
	get, post := fasthttp.MethodGet, fasthttp.MethodPost
	schema := contract.Schema(l.Section)
	if l.Exports == nil || l.Imports == nil {
		t.Fatal("configtest.Live needs Exports and Imports: every owner records each transport of its part in its own history")

		return
	}

	version := func() string {
		t.Helper()
		got := l.call(t, get, contract.PathVersion, nil, l.Reader)
		qt.Assert(t, qt.Equals(got.status, fasthttp.StatusOK), qt.Commentf("the version read: %s", got.raw))
		v, _ := got.body["version"].(string)
		qt.Assert(t, qt.IsTrue(strings.HasPrefix(v, "sha256:")), qt.Commentf("version %q", v))

		return v
	}

	// ---- The section, read by a member, with its version as the ETag ----
	read := l.call(t, get, contract.PathConfig, nil, l.Reader)
	qt.Assert(t, qt.Equals(read.status, fasthttp.StatusOK), qt.Commentf("the section read: %s", read.raw))
	qt.Assert(t, qt.Equals(fmt.Sprint(read.body["schema"]), schema))
	qt.Assert(t, qt.IsTrue(strings.HasPrefix(read.etag, `"sha256:`) && strings.HasSuffix(read.etag, `"`)),
		qt.Commentf("the version rides as the ETag, quoted: %q", read.etag))
	v0 := contract.EntityTag(read.etag)
	qt.Assert(t, qt.Equals(version(), v0), qt.Commentf("the version read answers the token the section read carried"))
	qt.Check(t, qt.IsFalse(strings.Contains(read.raw, `"id"`)), qt.Commentf("the section carries keys, never identifiers"))
	qt.Check(t, qt.IsFalse(strings.Contains(read.raw, `"createdAt"`)), qt.Commentf("the section carries no timestamps"))
	section := read.raw

	// ---- An export: answered as the read is, and recorded once ----
	exports := l.Exports(t)
	plain := l.call(t, get, contract.PathConfig, nil, l.Admin)
	qt.Assert(t, qt.Equals(plain.status, fasthttp.StatusOK), qt.Commentf("the administrator's read: %s", plain.raw))
	qt.Check(t, qt.HasLen(l.Exports(t), len(exports)), qt.Commentf("a plain read recorded an export"))
	export := l.call(t, get, contract.PathConfig, nil, l.Admin, l.purpose(contract.PurposeExport))
	qt.Assert(t, qt.Equals(export.status, fasthttp.StatusOK), qt.Commentf("the export read: %s", export.raw))
	qt.Check(t, qt.Equals(export.raw, plain.raw), qt.Commentf("an export answers what the read answers"))
	qt.Check(t, qt.Equals(export.etag, plain.etag))
	recorded := l.Exports(t)
	qt.Assert(t, qt.HasLen(recorded, len(exports)+1), qt.Commentf("an export leaves exactly one line"))
	qt.Check(t, qt.Equals(recorded[len(recorded)-1], contract.PartHash([]byte(export.raw))),
		qt.Commentf("the line carries the hash of the part the exporter received"))
	refusedExport := l.call(t, get, contract.PathConfig, nil, l.NonAdmin, l.purpose(contract.PurposeExport))
	qt.Check(t, qt.Equals(refusedExport.status, fasthttp.StatusForbidden), qt.Commentf("a member who does not administer exported: %s", refusedExport.raw))
	qt.Check(t, qt.HasLen(l.Exports(t), len(recorded)), qt.Commentf("a refused export left a line"))

	imports := func() int {
		t.Helper()

		return len(l.Imports(t))
	}
	lastImport := func() Import {
		t.Helper()
		all := l.Imports(t)
		qt.Assert(t, qt.Not(qt.HasLen(all, 0)), qt.Commentf("no import line"))

		return all[len(all)-1]
	}

	edit := func(f func(doc map[string]any)) []byte {
		t.Helper()
		var doc map[string]any
		qt.Assert(t, qt.IsNil(json.Unmarshal([]byte(section), &doc)))
		if f != nil {
			f(doc)
		}
		out, err := json.Marshal(doc)
		qt.Assert(t, qt.IsNil(err))

		return out
	}

	// ---- A document that changes nothing: previewed, applied, unmoved ----
	preview := l.call(t, post, contract.PathPreview, []byte(section), l.Admin)
	qt.Assert(t, qt.Equals(preview.status, fasthttp.StatusOK), qt.Commentf("the unchanged preview: %s", preview.raw))
	qt.Check(t, qt.Equals(fmt.Sprint(preview.body["dryRun"]), "true"))
	qt.Check(t, qt.Equals(fmt.Sprint(preview.body["applied"]), "false"))
	qt.Check(t, qt.Equals(fmt.Sprint(preview.body["version"]), v0), qt.Commentf("a preview answers the version an apply names"))
	for _, part := range l.Parts {
		for _, it := range items(preview.body, part) {
			qt.Check(t, qt.Equals(fmt.Sprint(it["status"]), string(contract.Unchanged)), qt.Commentf("%s: %v", part, it))
		}
	}
	before := imports()
	applied := l.call(t, post, contract.PathApply, []byte(section), l.Admin, l.ifMatch(version()))
	qt.Assert(t, qt.Equals(applied.status, fasthttp.StatusOK), qt.Commentf("the unchanged apply: %s", applied.raw))
	qt.Check(t, qt.Equals(imports(), before+1), qt.Commentf("an apply that was not refused leaves exactly one line, even one that changed nothing"))
	qt.Check(t, qt.Equals(lastImport(), Import{PartHash: contract.PartHash([]byte(section))}),
		qt.Commentf("the line carries the part's hash, and no document when the apply named none"))
	qt.Check(t, qt.Equals(fmt.Sprint(applied.body["applied"]), "true"))
	qt.Check(t, qt.Equals(fmt.Sprint(applied.body["version"]), v0), qt.Commentf("an unchanged idempotent apply never moves the token"))
	qt.Check(t, qt.Equals(version(), v0))

	// ---- A member who does not administer reads, and may not apply ----
	qt.Check(t, qt.Equals(l.call(t, get, contract.PathConfig, nil, l.NonAdmin).status, fasthttp.StatusOK))
	qt.Check(t, qt.Equals(l.call(t, post, contract.PathApply, []byte(section), l.NonAdmin, l.ifMatch(v0)).status, fasthttp.StatusForbidden))

	// ---- The owner's refusals: preview and apply agree, and nothing moves ----
	before = imports()
	for _, row := range l.Refusals {
		doc := edit(func(d map[string]any) {
			row.Edit(d)
			if l.Beside != nil {
				l.Beside(d)
			}
		})
		pre := l.call(t, post, contract.PathPreview, doc, l.Admin)
		qt.Assert(t, qt.Equals(pre.status, fasthttp.StatusOK), qt.Commentf("%s: a refusal is a report: %s", row.Name, pre.raw))
		qt.Check(t, qt.Equals(fmt.Sprint(pre.body["refused"]), "true"), qt.Commentf("%s", row.Name))
		// The line that refuses: the key, refused, with the reason, saying why. A
		// document naming a key twice has a second line for it beside the first.
		named := false
		for _, it := range items(pre.body, row.Part) {
			detail, _ := it["detail"].(string)
			if it["key"] == row.Key && it["status"] == string(contract.Refused) && it["reason"] == row.Reason && detail != "" {
				named = true
			}
		}
		qt.Check(t, qt.IsTrue(named), qt.Commentf("%s: the preview refuses %s among %s with %s, saying why: %s",
			row.Name, row.Key, row.Part, row.Reason, pre.raw))

		app := l.call(t, post, contract.PathApply, doc, l.Admin, l.ifMatch(version()))
		qt.Assert(t, qt.Equals(app.status, fasthttp.StatusOK), qt.Commentf("%s: a refusal is a report: %s", row.Name, app.raw))
		qt.Check(t, qt.Equals(fmt.Sprint(app.body["applied"]), "false"), qt.Commentf("%s", row.Name))
		for _, part := range l.Parts {
			pj, _ := json.Marshal(pre.body[part])
			aj, _ := json.Marshal(app.body[part])
			qt.Check(t, qt.Equals(string(aj), string(pj)), qt.Commentf("%s: preview and apply disagree on %s", row.Name, part))
		}
		qt.Check(t, qt.Equals(version(), v0), qt.Commentf("%s: a refused document moved the configuration", row.Name))
		if l.Landed != nil {
			qt.Check(t, qt.IsFalse(l.Landed(t)), qt.Commentf("%s: the clean item beside the refusal was written", row.Name))
		}
	}
	qt.Check(t, qt.Equals(imports(), before), qt.Commentf("a refused document left an import line"))

	// ---- A real change: previewed without landing, then applied ----
	changed := edit(l.Change.Edit)
	pre := l.call(t, post, contract.PathPreview, changed, l.Admin)
	qt.Assert(t, qt.Equals(pre.status, fasthttp.StatusOK), qt.Commentf("the change previewed: %s", pre.raw))
	qt.Check(t, qt.Equals(fmt.Sprint(pre.body["applied"]), "false"))
	qt.Check(t, qt.Equals(fmt.Sprint(item(t, pre.body, l.Change.Part, l.Change.Index)["status"]), string(contract.Changed)))
	qt.Check(t, qt.Equals(version(), v0), qt.Commentf("a preview wrote the change"))
	if l.Change.Landed != nil {
		qt.Check(t, qt.IsFalse(l.Change.Landed(t)), qt.Commentf("a preview wrote the change where the screens read"))
	}

	file := contract.PartHash([]byte("the document " + l.Section + " arrived in"))
	before = imports()
	app := l.call(t, post, contract.PathApply, changed, l.Admin, l.ifMatch(version()), l.Client.WithHeader(contract.HeaderDocument, file))
	qt.Assert(t, qt.Equals(app.status, fasthttp.StatusOK), qt.Commentf("the change applied: %s", app.raw))
	qt.Check(t, qt.Equals(imports(), before+1), qt.Commentf("the change left exactly one import line"))
	qt.Check(t, qt.Equals(lastImport(), Import{PartHash: contract.PartHash(changed), Document: file}),
		qt.Commentf("the import line names the part and the document it arrived in"))
	qt.Assert(t, qt.Equals(fmt.Sprint(app.body["applied"]), "true"), qt.Commentf("%s", app.raw))
	line := item(t, app.body, l.Change.Part, l.Change.Index)
	qt.Check(t, qt.Equals(fmt.Sprint(line["status"]), string(contract.Changed)))
	qt.Check(t, qt.Equals(fmt.Sprint(line["detail"]), l.Change.Detail))
	v1 := fmt.Sprint(app.body["version"])
	qt.Assert(t, qt.Not(qt.Equals(v1, v0)), qt.Commentf("a real change moves the token"))
	qt.Check(t, qt.Equals(version(), v1), qt.Commentf("the token the apply answers is the token the next read answers"))
	after := l.call(t, get, contract.PathConfig, nil, l.Reader)
	qt.Check(t, qt.Equals(contract.EntityTag(after.etag), v1))
	if l.Change.Landed != nil {
		qt.Check(t, qt.IsTrue(l.Change.Landed(t)), qt.Commentf("the change landed where the screens read"))
	}

	// ---- An apply lands only on the configuration it was previewed against ----
	before = imports()
	none := l.call(t, post, contract.PathApply, []byte(section), l.Admin)
	qt.Check(t, qt.Equals(none.status, fasthttp.StatusPreconditionRequired), qt.Commentf("%s", none.raw))
	qt.Check(t, qt.Equals(fmt.Sprint(none.body["code"]), contract.Code(l.Domain, contract.ReasonVersionRequired)))
	stale := l.call(t, post, contract.PathApply, []byte(section), l.Admin, l.ifMatch(v0))
	qt.Check(t, qt.Equals(stale.status, fasthttp.StatusPreconditionFailed), qt.Commentf("%s", stale.raw))
	qt.Check(t, qt.Equals(fmt.Sprint(stale.body["code"]), contract.Code(l.Domain, contract.ReasonVersionMoved)))
	qt.Check(t, qt.Not(qt.Equals(fmt.Sprint(stale.body["title"]), "")), qt.Commentf("the refusal carries a public title: %s", stale.raw))
	qt.Check(t, qt.Equals(version(), v1), qt.Commentf("a stale apply moved the configuration"))
	qt.Check(t, qt.Equals(imports(), before), qt.Commentf("an apply refused for its version left an import line"))
	if l.Change.Landed != nil {
		qt.Check(t, qt.IsTrue(l.Change.Landed(t)), qt.Commentf("a stale apply undid the change"))
	}

	// ---- Another tenant's configuration is another section and another token ----
	other := l.call(t, get, contract.PathConfig, nil, l.OtherTenant)
	qt.Check(t, qt.Equals(other.status, fasthttp.StatusOK), qt.Commentf("%s", other.raw))
	qt.Check(t, qt.Not(qt.Equals(contract.EntityTag(other.etag), v1)))
	qt.Check(t, qt.Not(qt.Equals(other.raw, after.raw)), qt.Commentf("another tenant is handed this tenant's section"))

	// ---- A document that is not this section is a request fault ----
	foreign := contract.Schema("another-" + l.Section)
	for _, body := range []string{`{"schema":"` + foreign + `"}`, `not json`} {
		got := l.call(t, post, contract.PathPreview, []byte(body), l.Admin)
		qt.Check(t, qt.Equals(got.status, fasthttp.StatusUnprocessableEntity), qt.Commentf("%s: %s", body, got.raw))
		qt.Check(t, qt.Equals(fmt.Sprint(got.body["code"]), contract.Code(l.Domain, contract.ReasonBadDocument)), qt.Commentf("%s", body))
		qt.Check(t, qt.Not(qt.Equals(fmt.Sprint(got.body["title"]), "")), qt.Commentf("the refusal carries a public title: %s", got.raw))
	}
}

// items reads one list of a report.
func items(report map[string]any, part string) []map[string]any {
	list, _ := report[part].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, it := range list {
		m, _ := it.(map[string]any)
		out = append(out, m)
	}

	return out
}

// item reads one line of a report, failing when it is not there.
func item(t testing.TB, report map[string]any, part string, index int) map[string]any {
	t.Helper()
	list := items(report, part)
	qt.Assert(t, qt.IsTrue(len(list) > index), qt.Commentf("report %s has no item %d: %v", part, index, report))

	return list[index]
}
