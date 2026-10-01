package contract

import (
	"encoding/json"
	"testing"

	"github.com/go-quicktest/qt"
)

// A report is one object: the outcome's members and one list per part beside
// them, an empty part written as [] and an item's empty reason and detail left
// out.
func TestAReportIsTheOutcomeAndItsListsInOneObject(t *testing.T) {
	r := NewReport("orders-config/1", true, "sha256:v", map[string][]Item{
		"priorities": {{Key: "high", Status: Unchanged}, {Key: "rush", Status: Added}},
		"stages":     nil,
	})
	raw, err := json.Marshal(r)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(string(raw),
		`{"schema":"orders-config/1","dryRun":true,"applied":false,"refused":false,"version":"sha256:v",`+
			`"priorities":[{"key":"high","status":"unchanged"},{"key":"rush","status":"added"}],"stages":[]}`))

	var back Report
	qt.Assert(t, qt.IsNil(json.Unmarshal(raw, &back)))
	qt.Check(t, qt.DeepEquals(back.Outcome, r.Outcome))
	qt.Check(t, qt.DeepEquals(back.Lists["priorities"], r.Lists["priorities"]))
	qt.Check(t, qt.HasLen(back.Lists["stages"], 0))
	_, has := back.Lists["stages"]
	qt.Check(t, qt.IsTrue(has), qt.Commentf("an empty part is still a part"))
}

// One refused item refuses the document: it is not applied, and says so.
func TestOneRefusedItemMeansTheDocumentIsNotApplied(t *testing.T) {
	lists := map[string][]Item{
		"priorities": {{Key: "high", Status: Changed, Detail: "label"}},
		"fields":     {{Key: "weight", Status: Refused, Reason: ReasonKeyConflict, Detail: "a new meaning is a new key"}},
	}
	r := NewReport("orders-config/1", false, "sha256:v", lists)
	qt.Check(t, qt.IsTrue(r.Refused))
	qt.Check(t, qt.IsFalse(r.Applied))

	clean := NewReport("orders-config/1", false, "sha256:v", map[string][]Item{"priorities": lists["priorities"]})
	qt.Check(t, qt.IsFalse(clean.Refused))
	qt.Check(t, qt.IsTrue(clean.Applied))

	dry := NewReport("orders-config/1", true, "sha256:v", map[string][]Item{"priorities": lists["priorities"]})
	qt.Check(t, qt.IsFalse(dry.Applied), qt.Commentf("a preview is never applied"))
}

func TestAListMayNotTakeAnOutcomeMembersName(t *testing.T) {
	for _, name := range []string{"schema", "dryRun", "applied", "refused", "version"} {
		_, err := json.Marshal(Report{Lists: map[string][]Item{name: {}}})
		qt.Check(t, qt.IsNotNil(err), qt.Commentf("%s", name))
	}
}

// A member beside the lists that is not a list is left alone, so an owner may
// carry more than its lists.
func TestReadingAReportKeepsOnlyItsLists(t *testing.T) {
	var r Report
	qt.Assert(t, qt.IsNil(json.Unmarshal([]byte(
		`{"schema":"s-config/1","applied":true,"version":"v","note":"x","counts":{"a":1},"items":[{"key":"k","status":"added"}]}`), &r)))
	qt.Check(t, qt.IsTrue(r.Applied))
	qt.Check(t, qt.HasLen(r.Lists, 1))
	qt.Check(t, qt.DeepEquals(r.Lists["items"], []Item{{Key: "k", Status: Added}}))
	qt.Check(t, qt.IsNotNil(json.Unmarshal([]byte(`{"items":[1]}`), &r)), qt.Commentf("a list that is not items"))
}

func TestCountingAListByStatus(t *testing.T) {
	c := Count([]Item{{Status: Added}, {Status: Added}, {Status: Changed}, {Status: Unchanged}, {Status: Refused}})
	qt.Check(t, qt.DeepEquals(c, Counts{Added: 2, Changed: 1, Unchanged: 1, Refused: 1}))
	qt.Check(t, qt.IsTrue(AnyRefused(nil, []Item{{Status: Added}}, []Item{{Status: Refused}})))
	qt.Check(t, qt.IsFalse(AnyRefused([]Item{{Status: Added}, {Status: Changed}})))
}
