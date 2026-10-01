// Package match judges a configuration document against what an owner holds,
// item by item and by key, for an owner whose apply is written in Go.
//
// Every owner judges a document the same way, and this package is that way:
//
//   - the document is an object carrying the section's own schema, and each list
//     it carries is a list — otherwise it is refused whole;
//   - every entry is an object with a key, and no key appears twice;
//   - an entry the owner does not hold is added;
//   - an entry it holds is unchanged, or changed in the members that may change
//     (a label, a look, whether it is retired) — the report names them;
//   - a change to what an item is — its kind, type, unit or format — on an
//     existing key is refused, naming the key: a new meaning is a new key;
//   - an item is active or retired, and a document never removes one. An item
//     the document leaves out stays as it is; one it marks retired stays in the
//     section, valid for everything that already names it. That is the only
//     safe answer for a kind another owner may name, since no check across two
//     services can be atomic with a removal.
//
// The owner keeps its own rules (Kind.Check) and its writes: Match answers the
// report and the entries to act on, and writes nothing, so the dry pass and the
// writing pass answer the same report.
package match

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gmb-lib/go-configbyte/contract"
)

// The two states an item is ever in.
const (
	Active  = "active"
	Retired = "retired"
)

// statusMember is the member an item's state travels in.
const statusMember = "status"

// Entry is one item as it travels: its members by name. Numbers are kept as
// json.Number, so a value compares by its text.
type Entry map[string]any

// Key answers the entry's key, surrounding space removed.
func (e Entry) Key() string { return Text(e["key"]) }

// Status answers the entry's state: its status member, or active without one.
func (e Entry) Status() string {
	if s := Text(e[statusMember]); s != "" {
		return s
	}

	return Active
}

// Text is a member's value as it is compared: a string without surrounding
// space, a number or a boolean as written, compact JSON for an object or a list,
// and "" for null or a member that is absent.
func Text(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}

		return "false"
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}

		return string(b)
	}
}

// Kind is how one list of a section is judged.
type Kind struct {
	// Word names one item in a refusal's sentence, such as "priority".
	Word string
	// Required are the members an entry must carry with a value, beside its key.
	Required []string
	// Fixed are the members that say what an item is. A different value on an
	// existing key is refused as a key conflict.
	Fixed []string
	// Compared are the members whose change applies, such as a label. The status
	// is always compared. A compared member the entry leaves out, with no
	// default, is left as it is held.
	Compared []string
	// Defaults are the values a member takes when an entry leaves it out.
	Defaults map[string]string
	// Check is the owner's own rule on one entry, run after the shared ones. It
	// answers a reason and a sentence to refuse the entry, or two empty strings.
	Check func(e Entry) (reason, detail string)
}

// Judged is an entry the document carries that was not refused: what to do with
// it, and for a change, which members change.
type Judged struct {
	Key     string
	Entry   Entry
	Status  contract.Status
	Changed []string
}

// Match judges one list of a document against the items the owner holds, by
// key. It answers one report line per entry, in the document's order, and the
// entries that were not refused. It writes nothing.
func (k Kind) Match(list []json.RawMessage, held map[string]Entry) ([]contract.Item, []Judged) {
	items := make([]contract.Item, 0, len(list))
	var judged []Judged
	seen := map[string]bool{}

	for i, raw := range list {
		e, ok := decode(raw)
		if !ok {
			items = append(items, refused("", contract.ReasonBadDocument, fmt.Sprintf("entry %d is not an object", i+1)))

			continue
		}
		key := e.Key()
		if key == "" {
			items = append(items, refused("", contract.ReasonBadDocument, fmt.Sprintf("entry %d is missing its key", i+1)))

			continue
		}
		if seen[key] {
			items = append(items, refused(key, contract.ReasonBadDocument, k.name(key)+" appears twice"))

			continue
		}
		seen[key] = true

		if item, bad := k.shape(key, e); bad {
			items = append(items, item)

			continue
		}

		have, found := held[key]
		if !found {
			items = append(items, contract.Item{Key: key, Status: contract.Added})
			judged = append(judged, Judged{Key: key, Entry: e, Status: contract.Added})

			continue
		}

		if item, conflict := k.conflict(key, e, have); conflict {
			items = append(items, item)

			continue
		}

		changed := k.changed(e, have)
		if len(changed) == 0 {
			items = append(items, contract.Item{Key: key, Status: contract.Unchanged})
			judged = append(judged, Judged{Key: key, Entry: e, Status: contract.Unchanged})

			continue
		}
		items = append(items, contract.Item{Key: key, Status: contract.Changed, Detail: strings.Join(changed, ", ")})
		judged = append(judged, Judged{Key: key, Entry: e, Status: contract.Changed, Changed: changed})
	}

	return items, judged
}

func (k Kind) name(key string) string {
	if k.Word == "" {
		return key
	}

	return k.Word + " " + key
}

func refused(key, reason, detail string) contract.Item {
	return contract.Item{Key: key, Status: contract.Refused, Reason: reason, Detail: detail}
}

// decode reads one entry, keeping numbers as their text.
func decode(raw json.RawMessage) (Entry, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var e Entry
	if err := dec.Decode(&e); err != nil {
		return nil, false
	}

	return e, true
}

// value is a member of an entry as it is judged: its default when the entry
// leaves it out. present is false when it is out and has no default.
func (k Kind) value(e Entry, member string) (string, bool) {
	if v := Text(e[member]); v != "" {
		return v, true
	}
	if d, ok := k.Defaults[member]; ok {
		return d, true
	}

	return "", false
}

// shape applies the shared rules on one entry, then the owner's.
func (k Kind) shape(key string, e Entry) (contract.Item, bool) {
	for _, m := range k.Required {
		if _, ok := k.value(e, m); !ok {
			return refused(key, contract.ReasonBadDocument, k.name(key)+" has no "+m), true
		}
	}
	if s := e.Status(); s != Active && s != Retired {
		return refused(key, contract.ReasonBadDocument, k.name(key)+": status must be active or retired"), true
	}
	if k.Check != nil {
		if reason, detail := k.Check(e); reason != "" {
			return refused(key, reason, detail), true
		}
	}

	return contract.Item{}, false
}

// conflict refuses a new meaning on an existing key.
func (k Kind) conflict(key string, e, have Entry) (contract.Item, bool) {
	for _, m := range k.Fixed {
		now, _ := k.value(have, m)
		next, _ := k.value(e, m)
		if now != next {
			return refused(key, contract.ReasonKeyConflict, fmt.Sprintf(
				"%s: %s is %s here and %s in the document; a new meaning is a new key",
				k.name(key), m, quoted(now), quoted(next))), true
		}
	}

	return contract.Item{}, false
}

func quoted(v string) string {
	if v == "" {
		return "empty"
	}

	return fmt.Sprintf("%q", v)
}

// changed answers the members a held item changes in, in the kind's order, the
// status last.
func (k Kind) changed(e, have Entry) []string {
	var out []string
	for _, m := range k.Compared {
		next, present := k.value(e, m)
		if !present {
			continue
		}
		if now, _ := k.value(have, m); now != next {
			out = append(out, m)
		}
	}
	if have.Status() != e.Status() {
		out = append(out, statusMember)
	}

	return out
}

// Refusal is a document refused whole, before any item is judged.
type Refusal struct {
	Detail string
}

func (e *Refusal) Error() string { return "configuration: " + e.Detail }

// Reason is the reason a whole document is refused with.
func (*Refusal) Reason() string { return contract.ReasonBadDocument }

// Open reads a document as a section for judging. It must be an object carrying
// the schema, and each list named must be a list — null is not one; a list the
// document leaves out is empty. It answers the lists by name, or a Refusal.
func Open(doc []byte, schema string, lists ...string) (map[string][]json.RawMessage, error) {
	var members map[string]json.RawMessage
	trimmed := bytes.TrimSpace(doc)
	if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(trimmed, &members) != nil {
		return nil, &Refusal{Detail: "the section must be an object"}
	}

	raw, has := members["schema"]
	var named any
	_ = json.Unmarshal(raw, &named)
	if s, ok := named.(string); !has || !ok || s != schema {
		detail := "the section is not " + schema
		if has {
			detail += " (it says " + said(raw) + ")"
		}

		return nil, &Refusal{Detail: detail}
	}

	out := make(map[string][]json.RawMessage, len(lists))
	for _, name := range lists {
		raw, ok := members[name]
		if !ok {
			out[name] = nil

			continue
		}
		var list []json.RawMessage
		if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &list) != nil {
			return nil, &Refusal{Detail: name + " must be a list"}
		}
		out[name] = list
	}

	return out, nil
}

// said is what a document's schema member says, as a sentence quotes it.
func said(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	var s string
	if len(trimmed) > 0 && trimmed[0] == '"' && json.Unmarshal(trimmed, &s) == nil {
		return s
	}

	return string(trimmed)
}
