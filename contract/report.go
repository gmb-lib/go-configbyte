package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Status is what happened, or would happen, to one item of a document.
type Status string

const (
	// Added is an item the owner does not hold yet.
	Added Status = "added"
	// Changed is an item the owner holds under the same key with another value
	// in a member that may change, such as its label or whether it is retired.
	// The item's detail names the members, comma-separated.
	Changed Status = "changed"
	// Unchanged is an item the owner already holds exactly as the document says.
	Unchanged Status = "unchanged"
	// Refused is an item the owner will not take. The item names the reason and
	// says why. A document with one refused item writes nothing.
	Refused Status = "refused"
)

// Item is one line of a report: one item of the document, by key.
type Item struct {
	// Key is the item's key. It is empty only for an entry that could not be
	// read far enough to have one.
	Key    string `json:"key"`
	Status Status `json:"status"`
	// Reason is a refusal's reason, such as ReasonKeyConflict.
	Reason string `json:"reason,omitempty"`
	// Detail is a refusal's sentence, or the members a change touches.
	Detail string `json:"detail,omitempty"`
}

// AnyRefused reports whether any item in the lists was refused.
func AnyRefused(lists ...[]Item) bool {
	for _, list := range lists {
		for _, it := range list {
			if it.Status == Refused {
				return true
			}
		}
	}

	return false
}

// Counts is how many items of a list ended in each status: the summary an
// applied document's history line carries.
type Counts struct {
	Added     int `json:"added"`
	Changed   int `json:"changed"`
	Unchanged int `json:"unchanged"`
	Refused   int `json:"refused"`
}

// Count counts a list's items by status.
func Count(items []Item) Counts {
	var c Counts
	for _, it := range items {
		switch it.Status {
		case Added:
			c.Added++
		case Changed:
			c.Changed++
		case Unchanged:
			c.Unchanged++
		case Refused:
			c.Refused++
		}
	}

	return c
}

// Outcome is what every owner's preview and apply answer about the whole
// section, beside the report's lists.
type Outcome struct {
	// Schema is the section's payload name.
	Schema string `json:"schema"`
	// DryRun is true for a preview.
	DryRun bool `json:"dryRun"`
	// Applied is true when the document was written. A preview never is; an
	// apply of a document with a refused item is not either.
	Applied bool `json:"applied"`
	// Refused is true when any item was refused.
	Refused bool `json:"refused"`
	// Version is the section's version as the answer leaves it. After a preview
	// it is the version an apply of the same document names in If-Match.
	Version string `json:"version"`
}

// outcomeMembers are the report members the outcome owns; a list may not use
// one of their names.
var outcomeMembers = map[string]bool{"schema": true, "dryRun": true, "applied": true, "refused": true, "version": true}

// Report is a preview's or an apply's whole answer: the outcome, and one list
// of items per part of the section, such as "priorities". On the wire the lists
// sit beside the outcome's members in one object.
type Report struct {
	Outcome
	Lists map[string][]Item
}

// NewReport builds the report for a section's lists, setting Refused from them.
// A dry run is never applied, and neither is a document with a refused item.
func NewReport(schema string, dryRun bool, version string, lists map[string][]Item) Report {
	refused := false
	for _, items := range lists {
		refused = refused || AnyRefused(items)
	}

	return Report{
		Outcome: Outcome{Schema: schema, DryRun: dryRun, Applied: !dryRun && !refused, Refused: refused, Version: version},
		Lists:   lists,
	}
}

// MarshalJSON writes the outcome's members and every list in one object. An
// empty list is written as [] so a reader can tell it from a list the section
// does not have.
func (r Report) MarshalJSON() ([]byte, error) {
	names := make([]string, 0, len(r.Lists))
	for name := range r.Lists {
		if outcomeMembers[name] {
			return nil, fmt.Errorf("configuration: a report list may not be called %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	head, err := json.Marshal(r.Outcome)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.Write(head[:len(head)-1])
	for _, name := range names {
		items := r.Lists[name]
		if items == nil {
			items = []Item{}
		}
		key, _ := json.Marshal(name)
		list, err := json.Marshal(items)
		if err != nil {
			return nil, err
		}
		buf.WriteByte(',')
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(list)
	}
	buf.WriteByte('}')

	return buf.Bytes(), nil
}

// UnmarshalJSON reads the outcome's members, and every other member that is a
// list as one of the report's lists. A member that is neither is left alone, so
// an owner may carry more beside its lists.
func (r *Report) UnmarshalJSON(b []byte) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil {
		return err
	}
	var out Report
	if err := json.Unmarshal(b, &out.Outcome); err != nil {
		return err
	}
	for name, raw := range members {
		if outcomeMembers[name] || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			continue
		}
		var items []Item
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("configuration: report list %q: %w", name, err)
		}
		if out.Lists == nil {
			out.Lists = map[string][]Item{}
		}
		out.Lists[name] = items
	}
	*r = out

	return nil
}
