package match_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/go-quicktest/qt"

	"github.com/gmb-lib/go-configbyte/contract"
	"github.com/gmb-lib/go-configbyte/match"
)

// priorities is a kind the way an owner describes it: a label that must be
// there, a look that defaults to normal and must be one of four, both of which
// may change.
var priorities = match.Kind{
	Word:     "priority",
	Required: []string{"label"},
	Compared: []string{"label", "look"},
	Defaults: map[string]string{"look": "normal"},
	Check: func(e match.Entry) (string, string) {
		switch look := match.Text(e["look"]); look {
		case "", "low", "normal", "attention", "urgent":
			return "", ""
		default:
			return contract.ReasonBadDocument, "priority " + e.Key() + ": look must be one of low, normal, attention, urgent"
		}
	},
}

// fields is a kind whose type and unit say what an item is.
var fields = match.Kind{
	Word:     "field",
	Required: []string{"label", "fieldType"},
	Fixed:    []string{"fieldType", "unit"},
	Compared: []string{"label", "warnDays"},
}

func list(t *testing.T, doc string) []json.RawMessage {
	t.Helper()
	var l []json.RawMessage
	qt.Assert(t, qt.IsNil(json.Unmarshal([]byte(doc), &l)))

	return l
}

func held(t *testing.T, doc string) map[string]match.Entry {
	t.Helper()
	out := map[string]match.Entry{}
	_, judged := match.Kind{}.Match(list(t, doc), nil)
	for _, j := range judged {
		out[j.Key] = j.Entry
	}

	return out
}

func TestAnItemIsAddedUnchangedOrChangedByKey(t *testing.T) {
	have := held(t, `[{"key":"high","label":"High","look":"urgent"},{"key":"low","label":"Low"}]`)
	items, judged := priorities.Match(list(t, `[
		{"key":"high","label":"High","look":"urgent"},
		{"key":"low","label":"Lowest"},
		{"key":"rush","label":"Rush","look":"urgent"}
	]`), have)

	qt.Check(t, qt.DeepEquals(items, []contract.Item{
		{Key: "high", Status: contract.Unchanged},
		{Key: "low", Status: contract.Changed, Detail: "label"},
		{Key: "rush", Status: contract.Added},
	}))
	qt.Assert(t, qt.HasLen(judged, 3))
	qt.Check(t, qt.DeepEquals(judged[1].Changed, []string{"label"}))
	qt.Check(t, qt.Equals(judged[2].Status, contract.Added))
}

// A document never removes: an item it leaves out is not in the report and not
// among the entries to act on, so it stays as it is held.
func TestADocumentNeverRemovesAnItemItLeavesOut(t *testing.T) {
	have := held(t, `[{"key":"high","label":"High"},{"key":"low","label":"Low"}]`)
	items, judged := priorities.Match(list(t, `[{"key":"high","label":"High"}]`), have)
	qt.Check(t, qt.DeepEquals(items, []contract.Item{{Key: "high", Status: contract.Unchanged}}))
	qt.Check(t, qt.HasLen(judged, 1))
}

// Retiring is a change of state, reported as the status changing: the item stays
// in the section, valid for everything that already names it. Offering it again
// is the same change back.
func TestRetiringIsAChangeOfStatusNeverARemoval(t *testing.T) {
	have := held(t, `[{"key":"high","label":"High"},{"key":"old","label":"Old","status":"retired"}]`)
	items, judged := priorities.Match(list(t, `[
		{"key":"high","label":"Highest","status":"retired"},
		{"key":"old","label":"Old","status":"active"}
	]`), have)
	qt.Check(t, qt.DeepEquals(items, []contract.Item{
		{Key: "high", Status: contract.Changed, Detail: "label, status"},
		{Key: "old", Status: contract.Changed, Detail: "status"},
	}))
	qt.Check(t, qt.Equals(judged[0].Entry.Status(), match.Retired))
	qt.Check(t, qt.Equals(judged[1].Entry.Status(), match.Active))
}

// A change to what an item is, on an existing key, is refused with the key
// named: a new meaning is a new key. A member that says what an item is and is
// left out reads as empty, so dropping a unit is a new meaning too.
func TestANewMeaningOnAnExistingKeyIsRefused(t *testing.T) {
	have := held(t, `[{"key":"weight","label":"Weight","fieldType":"number","unit":"kg"}]`)
	for name, doc := range map[string]string{
		"another type":    `[{"key":"weight","label":"Weight","fieldType":"text","unit":"kg"}]`,
		"another unit":    `[{"key":"weight","label":"Weight","fieldType":"number","unit":"t"}]`,
		"the unit gone":   `[{"key":"weight","label":"Weight","fieldType":"number"}]`,
		"both, and label": `[{"key":"weight","label":"Mass","fieldType":"text"}]`,
	} {
		items, judged := fields.Match(list(t, doc), have)
		qt.Assert(t, qt.HasLen(items, 1), qt.Commentf("%s", name))
		qt.Check(t, qt.Equals(items[0].Key, "weight"), qt.Commentf("%s", name))
		qt.Check(t, qt.Equals(items[0].Status, contract.Refused), qt.Commentf("%s", name))
		qt.Check(t, qt.Equals(items[0].Reason, contract.ReasonKeyConflict), qt.Commentf("%s", name))
		qt.Check(t, qt.StringContains(items[0].Detail, "field weight: "), qt.Commentf("%s", name))
		qt.Check(t, qt.StringContains(items[0].Detail, "a new meaning is a new key"), qt.Commentf("%s", name))
		qt.Check(t, qt.HasLen(judged, 0), qt.Commentf("%s: a refused entry is not acted on", name))
	}
	items, _ := fields.Match(list(t, `[{"key":"weight","label":"Mass","fieldType":"number","unit":"kg"}]`), have)
	qt.Check(t, qt.DeepEquals(items, []contract.Item{{Key: "weight", Status: contract.Changed, Detail: "label"}}),
		qt.Commentf("a new label on the same meaning applies"))
}

// A member that may change and is left out, with no default, is left as held;
// one with a default takes it.
func TestALeftOutMemberKeepsItsValueOrTakesItsDefault(t *testing.T) {
	have := held(t, `[{"key":"weight","label":"Weight","fieldType":"number","unit":"kg","warnDays":60}]`)
	items, _ := fields.Match(list(t, `[{"key":"weight","label":"Weight","fieldType":"number","unit":"kg"}]`), have)
	qt.Check(t, qt.DeepEquals(items, []contract.Item{{Key: "weight", Status: contract.Unchanged}}))
	items, _ = fields.Match(list(t, `[{"key":"weight","label":"Weight","fieldType":"number","unit":"kg","warnDays":"60"}]`), have)
	qt.Check(t, qt.DeepEquals(items, []contract.Item{{Key: "weight", Status: contract.Unchanged}}), qt.Commentf("a value compares by its text"))
	items, _ = fields.Match(list(t, `[{"key":"weight","label":"Weight","fieldType":"number","unit":"kg","warnDays":30}]`), have)
	qt.Check(t, qt.DeepEquals(items, []contract.Item{{Key: "weight", Status: contract.Changed, Detail: "warnDays"}}))

	urgent := held(t, `[{"key":"high","label":"High","look":"urgent"}]`)
	items, _ = priorities.Match(list(t, `[{"key":"high","label":"High"}]`), urgent)
	qt.Check(t, qt.DeepEquals(items, []contract.Item{{Key: "high", Status: contract.Changed, Detail: "look"}}))
}

// Every entry out of shape is refused by itself, named as closely as it can be,
// and the entries beside it are still judged.
func TestAnEntryOutOfShapeIsRefusedAndNamed(t *testing.T) {
	items, judged := priorities.Match(list(t, `[
		"high",
		{"label":"No key"},
		{"key":"  ","label":"Blank key"},
		{"key":"high","label":"High"},
		{"key":"high","label":"High again"},
		{"key":"mid"},
		{"key":"low","label":"Low","status":"deleted"},
		{"key":"odd","label":"Odd","look":"loud"}
	]`), nil)
	qt.Check(t, qt.DeepEquals(items, []contract.Item{
		{Key: "", Status: contract.Refused, Reason: contract.ReasonBadDocument, Detail: "entry 1 is not an object"},
		{Key: "", Status: contract.Refused, Reason: contract.ReasonBadDocument, Detail: "entry 2 is missing its key"},
		{Key: "", Status: contract.Refused, Reason: contract.ReasonBadDocument, Detail: "entry 3 is missing its key"},
		{Key: "high", Status: contract.Added},
		{Key: "high", Status: contract.Refused, Reason: contract.ReasonBadDocument, Detail: "priority high appears twice"},
		{Key: "mid", Status: contract.Refused, Reason: contract.ReasonBadDocument, Detail: "priority mid has no label"},
		{Key: "low", Status: contract.Refused, Reason: contract.ReasonBadDocument, Detail: "priority low: status must be active or retired"},
		{Key: "odd", Status: contract.Refused, Reason: contract.ReasonBadDocument, Detail: "priority odd: look must be one of low, normal, attention, urgent"},
	}))
	qt.Check(t, qt.HasLen(judged, 1))
	qt.Check(t, qt.IsTrue(contract.AnyRefused(items)))
}

// Judging writes nothing and depends on nothing but its inputs, so the dry pass
// and the writing pass of the same document answer the same report.
func TestJudgingTheSameDocumentTwiceAnswersTheSameReport(t *testing.T) {
	have := held(t, `[{"key":"high","label":"High"}]`)
	doc := list(t, `[{"key":"high","label":"Higher"},{"key":"new","label":"New"},{"key":"new","label":"Twice"}]`)
	first, _ := priorities.Match(doc, have)
	second, _ := priorities.Match(doc, have)
	qt.Check(t, qt.DeepEquals(first, second))
	qt.Check(t, qt.Equals(match.Text(have["high"]["label"]), "High"), qt.Commentf("what is held is not touched"))
}

func TestAMemberComparesByItsText(t *testing.T) {
	for v, want := range map[any]string{nil: "", "  x ": "x", json.Number("60"): "60", true: "true", false: "false"} {
		qt.Check(t, qt.Equals(match.Text(v), want), qt.Commentf("%v", v))
	}
	qt.Check(t, qt.Equals(match.Text([]any{"a", 1.0}), `["a",1]`))
	qt.Check(t, qt.Equals(match.Entry{}.Status(), match.Active))
}

// A document is read as the section before any item is judged: an object, its
// own schema, and every part a list. Anything else is refused whole.
func TestADocumentIsOpenedAsTheSectionOrRefusedWhole(t *testing.T) {
	lists, err := match.Open([]byte(`{"schema":"orders-config/1","priorities":[{"key":"a"}],"settings":{}}`), "orders-config/1", "priorities", "stages")
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.HasLen(lists["priorities"], 1))
	stages, has := lists["stages"]
	qt.Check(t, qt.IsTrue(has))
	qt.Check(t, qt.HasLen(stages, 0), qt.Commentf("a part the document leaves out is empty"))

	for doc, want := range map[string]string{
		`[]`:                          "the section must be an object",
		`not json`:                    "the section must be an object",
		`{"priorities":[]}`:           "the section is not orders-config/1",
		`{"schema":"stock-config/1"}`: "the section is not orders-config/1 (it says stock-config/1)",
		`{"schema":null}`:             "the section is not orders-config/1 (it says null)",
		`{"schema":"orders-config/1","priorities":{}}`: "priorities must be a list",
		`{"schema":"orders-config/1","stages":null}`:   "stages must be a list",
	} {
		_, err := match.Open([]byte(doc), "orders-config/1", "priorities", "stages")
		var refusal *match.Refusal
		qt.Assert(t, qt.IsTrue(errors.As(err, &refusal)), qt.Commentf("%s", doc))
		qt.Check(t, qt.Equals(refusal.Detail, want), qt.Commentf("%s", doc))
		qt.Check(t, qt.Equals(refusal.Reason(), contract.ReasonBadDocument))
	}
}

func TestARefusalSaysWhyAndAKindWithoutAWordNamesTheKey(t *testing.T) {
	_, err := match.Open([]byte(`[]`), "orders-config/1")
	qt.Check(t, qt.ErrorMatches(err, "configuration: the section must be an object"))

	items, _ := match.Kind{}.Match(list(t, `[{"key":"a"},{"key":"a"}]`), nil)
	qt.Check(t, qt.Equals(items[1].Detail, "a appears twice"))
	items, _ = match.Kind{}.Match([]json.RawMessage{json.RawMessage(`{"key":`)}, nil)
	qt.Check(t, qt.Equals(items[0].Detail, "entry 1 is not an object"))
}
