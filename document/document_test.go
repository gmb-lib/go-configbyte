package document_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-quicktest/qt"

	"github.com/gmb-lib/go-configbyte/contract"
	"github.com/gmb-lib/go-configbyte/document"
)

var sections = map[string]json.RawMessage{
	"orders": json.RawMessage(`{"schema": "orders-config/1", "priorities": [{"key": "high", "label": "High"}]}`),
	"stock":  json.RawMessage(`{"schema":"stock-config/1","kinds":[]}`),
}

// The hash is over the sections object with every section compacted and the
// keys in order, so a document re-indented on its way through an editor is
// still recognised as the export it came from.
func TestTheContentHashIsOverTheCanonicalSections(t *testing.T) {
	canonical := `{"orders":{"schema":"orders-config/1","priorities":[{"key":"high","label":"High"}]},"stock":{"schema":"stock-config/1","kinds":[]}}`
	sum := sha256.Sum256([]byte(canonical))
	qt.Check(t, qt.Equals(document.ContentHash(sections), "sha256:"+hex.EncodeToString(sum[:])))

	reindented := map[string]json.RawMessage{}
	for k, v := range sections {
		var buf bytes.Buffer
		qt.Assert(t, qt.IsNil(json.Indent(&buf, v, "\t", "    ")))
		reindented[k] = json.RawMessage(buf.String())
	}
	qt.Check(t, qt.Equals(document.ContentHash(reindented), document.ContentHash(sections)), qt.Commentf("only the whitespace moved"))

	edited := map[string]json.RawMessage{"orders": json.RawMessage(strings.Replace(string(sections["orders"]), "High", "Urgent", 1)), "stock": sections["stock"]}
	qt.Check(t, qt.Not(qt.Equals(document.ContentHash(edited), document.ContentHash(sections))))
}

// Each section sits inside the content hash in the form its part hash is taken
// over, so the hashes the owners record and the document's own are made of the
// same bytes: a section holding characters an encoder escapes hashes alike on
// both sides.
func TestTheContentHashIsMadeOfThePartsAsTheirOwnersHashThem(t *testing.T) {
	escaped := map[string]json.RawMessage{
		"orders": json.RawMessage(`{"schema": "orders-config/1", "label": "R&D <north>"}`),
		"stock":  json.RawMessage(`{"schema":"stock-config/1","kinds":[]}`),
	}
	var canonical bytes.Buffer
	canonical.WriteString(`{"orders":`)
	part, err := json.Marshal(escaped["orders"])
	qt.Assert(t, qt.IsNil(err))
	canonical.Write(part)
	canonical.WriteString(`,"stock":`)
	part, err = json.Marshal(escaped["stock"])
	qt.Assert(t, qt.IsNil(err))
	canonical.Write(part)
	canonical.WriteString(`}`)
	sum := sha256.Sum256(canonical.Bytes())
	qt.Check(t, qt.Equals(document.ContentHash(escaped), "sha256:"+hex.EncodeToString(sum[:])))

	for name, raw := range escaped {
		inside, err := json.Marshal(raw)
		qt.Assert(t, qt.IsNil(err))
		one := sha256.Sum256(inside)
		qt.Check(t, qt.Equals(contract.PartHash(raw), "sha256:"+hex.EncodeToString(one[:])), qt.Commentf("%s", name))
	}
}

func TestAnExportIsTheCurrentFormatTheTenantAndTheHash(t *testing.T) {
	at := time.Date(2026, 1, 31, 9, 0, 0, 0, time.FixedZone("x", 3600))
	d := document.New(sections, "tenant-a", at)
	qt.Check(t, qt.Equals(d.GmbConfig, document.Version))
	qt.Check(t, qt.Equals(d.ExportedAt, "2026-01-31T08:00:00Z"))
	qt.Check(t, qt.DeepEquals(d.Tenant, &document.Tenant{ID: "tenant-a"}))
	qt.Check(t, qt.Equals(d.ContentHash, document.ContentHash(sections)))
	qt.Check(t, qt.IsNil(d.Expect), qt.Commentf("an export never names a version"))

	raw, err := d.Encode()
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.StringContains(string(raw), "\n  \"gmbConfig\": \"1.0\""), qt.Commentf("indented, for a person to edit"))
	qt.Check(t, qt.Not(qt.StringContains(string(raw), "expect")))

	back, err := document.Parse(raw)
	qt.Assert(t, qt.IsNil(err))
	edited := back.Edited()
	qt.Assert(t, qt.IsNotNil(edited))
	qt.Check(t, qt.IsFalse(*edited), qt.Commentf("the export, unedited"))
}

// A document a reader cannot honestly interpret is refused before any owner is
// asked, with a sentence for the person who sent it.
func TestADocumentNobodyCanInterpretIsRefused(t *testing.T) {
	for name, want := range map[string]string{
		`not json`:                   "the document is not valid JSON",
		`{"sections":{"orders":{}}}`: "the document does not declare a gmbConfig 1.x version",
		`{"gmbConfig":"2.0","sections":{"orders":{}}}`: "the document does not declare a gmbConfig 1.x version",
		`{"gmbConfig":"1.0","sections":{}}`:            "the document carries no sections",
		`{"gmbConfig":"1.0"}`:                          "the document carries no sections",
	} {
		_, err := document.Parse([]byte(name))
		var invalid *document.Invalid
		qt.Assert(t, qt.IsTrue(errors.As(err, &invalid)), qt.Commentf("%s", name))
		qt.Check(t, qt.Equals(invalid.Why, want), qt.Commentf("%s", name))
	}

	d, err := document.Parse([]byte(`{"gmbConfig":"1.1","sections":{"unknown":{"schema":"unknown-config/1"}}}`))
	qt.Assert(t, qt.IsNil(err), qt.Commentf("a later 1.x and a section nobody here owns are read; the owners judge"))
	qt.Check(t, qt.HasLen(d.Sections, 1))
}

// Whether a document was edited is an honest flag: absent without a hash to
// compare against, true when the sections are not the ones hashed.
func TestWhetherADocumentWasEditedIsAFlagNeverAGate(t *testing.T) {
	d, err := document.Parse([]byte(`{"gmbConfig":"1.0","sections":{"orders":{}}}`))
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.IsNil(d.Edited()))

	d, err = document.Parse([]byte(`{"gmbConfig":"1.0","contentHash":"sha256:not-the-hash","sections":{"orders":{}}}`))
	qt.Assert(t, qt.IsNil(err), qt.Commentf("an edited document is still read"))
	edited := d.Edited()
	qt.Assert(t, qt.IsNotNil(edited))
	qt.Check(t, qt.IsTrue(*edited))
}

func TestAnImportAnswerIsOneOutcomeAndOneLinePerSection(t *testing.T) {
	no := false
	raw, err := json.Marshal(document.Answer{
		DryRun: true, DocumentEdited: &no, Outcome: document.Previewed,
		Sections: map[string]document.SectionResult{
			"orders":  {Status: document.SectionPreviewed, Version: "sha256:v", Report: json.RawMessage(`{"applied":false}`)},
			"unknown": {Status: document.SectionSkipped, Detail: "no service in this deployment owns this section"},
		},
	})
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(string(raw), `{"documentEdited":false,"dryRun":true,"outcome":"previewed","sections":{`+
		`"orders":{"status":"previewed","version":"sha256:v","report":{"applied":false}},`+
		`"unknown":{"status":"skipped","detail":"no service in this deployment owns this section"}}}`))

	raw, _ = json.Marshal(document.Answer{Outcome: document.Applied, Sections: map[string]document.SectionResult{}})
	qt.Check(t, qt.Equals(string(raw), `{"dryRun":false,"outcome":"applied","sections":{}}`), qt.Commentf("no hash, no flag"))
}

func TestTheOutcomeAndStatusWordsAreTheContractsOwn(t *testing.T) {
	outcomes := []document.Outcome{document.Previewed, document.Applied, document.Refused, document.RolledBack, document.Partial, document.Failed}
	qt.Check(t, qt.DeepEquals(outcomes, []document.Outcome{"previewed", "applied", "refused", "rolledBack", "partial", "failed"}))
	statuses := []document.Status{document.SectionPreviewed, document.SectionApplied, document.SectionRefused,
		document.SectionWithheld, document.SectionRolledBack, document.SectionSkipped, document.SectionFailed}
	qt.Check(t, qt.DeepEquals(statuses, []document.Status{"previewed", "applied", "refused", "withheld", "rolledBack", "skipped", "failed"}))
}

func FuzzParseNeverPanicsAndRefusesWithASentence(f *testing.F) {
	for _, s := range []string{`not json`, `{}`, `{"gmbConfig":"1.0","sections":{"a":{}}}`, `{"gmbConfig":1}`, `[]`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		d, err := document.Parse(body)
		if err != nil {
			var invalid *document.Invalid
			if !errors.As(err, &invalid) || invalid.Why == "" {
				t.Fatalf("Parse(%q) refused without a sentence: %v", body, err)
			}

			return
		}
		if !strings.HasPrefix(d.GmbConfig, "1.") || len(d.Sections) == 0 {
			t.Fatalf("Parse(%q) accepted %+v", body, d)
		}
	})
}

// A section that is not JSON cannot be hashed honestly, so it hashes to
// nothing, and an import cannot claim the document is an unedited export.
func TestASectionThatIsNotJSONHashesToNothing(t *testing.T) {
	qt.Check(t, qt.Equals(document.ContentHash(map[string]json.RawMessage{"orders": json.RawMessage(`{not json`)}), ""))
	d := document.New(map[string]json.RawMessage{"orders": json.RawMessage(`{not json`)}, "t", time.Now())
	qt.Check(t, qt.IsNil(d.Edited()))
	qt.Check(t, qt.Equals((&document.Invalid{Why: "the document carries no sections"}).Error(),
		"configuration document: the document carries no sections"))
}
