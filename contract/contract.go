// Package contract is the vocabulary of the tenant configuration contract: the
// five operations a service holding a tenant's configuration answers, the name
// its section travels under, the version token that moves when — and only
// when — the section moves, the item-by-item report a preview and an apply
// answer, and the reasons a refusal is given with.
//
// Both sides speak it. A service that owns a section answers with these words
// (package configserver); anything that reads another owner's section — a
// sign-in read, an export, an import, a coordinator checking its owners at
// start — reads them (package configclient).
//
// The contract in one paragraph: a section is the owning API's own payload,
// verbatim, named "<section>-config/1". Its version is derived from its bytes,
// so a token computed from the data cannot disagree with the data, whoever
// wrote it and however. The section read carries that token as its ETag,
// computed from the very bytes returned. A preview writes nothing and reports
// exactly what an apply of the same document would do, item by item, by key.
// An apply names, as If-Match, the version its preview was read against, and
// is refused when it names none or the section has moved since. A document
// never removes an item: a kind another owner may name is retired, and stays
// valid for everything that already names it.
package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The five operations, relative to the owning service's own versioned API
// root, such as "/api/v1". The words and the methods are fixed: an owner adds
// no variant of them, and configuration never lives beside the version.
const (
	// PathConfig (GET) answers the tenant's whole section, with its version as
	// the ETag.
	PathConfig = "/config"
	// PathPreview (POST) answers what a document would change, and writes
	// nothing.
	PathPreview = "/config/preview"
	// PathApply (POST) applies a document atomically and idempotently. It names
	// the version its preview was read against in If-Match.
	PathApply = "/config/apply"
	// PathVersion (GET) answers the version alone: the cheap "has anything
	// changed?".
	PathVersion = "/config/version"
	// PathSection (GET) answers which section this owner holds and which
	// sections it refers to. It needs no tenant and no person, and says
	// nothing about any tenant's configuration.
	PathSection = "/config/section"
)

// SchemaSuffix follows a section's name to make the name its payload carries:
// the "orders" section travels as "orders-config/1".
const SchemaSuffix = "-config/1"

// Schema is the name a section's payload carries.
func Schema(section string) string { return section + SchemaSuffix }

var nameShape = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// maxName bounds a section name, which is also the section's key in a
// configuration document.
const maxName = 64

// ValidName reports whether a section name has the shape the contract uses:
// lower-case letters, digits and "-", starting with a letter.
func ValidName(name string) bool {
	return len(name) <= maxName && nameShape.MatchString(name)
}

// Token derives a section's version from its bytes: "sha256:" and the hex
// SHA-256 of the schema, a line break, the scope, a line break and the section
// as the owner renders it. The scope is what keeps two holders of the same
// configuration from sharing a token — the tenant, for a section held per
// tenant — and the schema is what keeps two sections apart.
//
// A token is opaque to every reader: compared for equality and nothing else.
// It is not a count, not a time, and not ordered.
func Token(schema, scope string, section []byte) string {
	h := sha256.New()
	h.Write([]byte(schema))
	h.Write([]byte{'\n'})
	h.Write([]byte(scope))
	h.Write([]byte{'\n'})
	h.Write(section)

	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// ETag renders a version as the entity tag a section read carries.
func ETag(version string) string { return `"` + version + `"` }

// EntityTag reads the version an ETag or If-Match header names: surrounding
// space, a weak marker and the quotes removed. It answers "" when the header
// names nothing.
func EntityTag(header string) string {
	v := strings.TrimSpace(header)
	v = strings.TrimPrefix(v, "W/")

	return strings.Trim(v, `"`)
}

// VersionAnswer is the body of a version read.
type VersionAnswer struct {
	Version string `json:"version"`
}

// SectionAnswer is the body of a section read: which section this owner holds,
// the name its payload carries, and the sections its own section refers to. A
// section that names another section's keys refers to it, and an import applies
// it after every section it refers to.
type SectionAnswer struct {
	Section  string   `json:"section"`
	Schema   string   `json:"schema"`
	RefersTo []string `json:"refersTo"`
}

// NewSectionAnswer builds the answer for a section and the sections it refers
// to, checked as [SectionAnswer.Check] checks it.
func NewSectionAnswer(section string, refersTo ...string) (SectionAnswer, error) {
	a := SectionAnswer{Section: section, Schema: Schema(section), RefersTo: append([]string{}, refersTo...)}

	return a, a.Check()
}

// Check reports what is wrong with an answer: a section name out of shape, a
// schema that is not the section's own, or a reference that is out of shape,
// named twice, or names the section itself. A coordinator that reads an answer
// failing it has been given an address that is not a configuration owner.
func (a SectionAnswer) Check() error {
	var errs []error
	if !ValidName(a.Section) {
		errs = append(errs, fmt.Errorf("configuration: section name %q must be lower-case letters, digits and \"-\"", a.Section))
	}
	if a.Schema != Schema(a.Section) {
		errs = append(errs, fmt.Errorf("configuration: section %q travels as %q, not %q", a.Section, Schema(a.Section), a.Schema))
	}
	if a.RefersTo == nil {
		errs = append(errs, fmt.Errorf("configuration: section %q does not say which sections it refers to", a.Section))
	}
	seen := map[string]bool{}
	for _, ref := range a.RefersTo {
		switch {
		case !ValidName(ref):
			errs = append(errs, fmt.Errorf("configuration: section %q refers to %q, which is not a section name", a.Section, ref))
		case ref == a.Section:
			errs = append(errs, fmt.Errorf("configuration: section %q refers to itself", a.Section))
		case seen[ref]:
			errs = append(errs, fmt.Errorf("configuration: section %q names %q twice", a.Section, ref))
		}
		seen[ref] = true
	}

	return errors.Join(errs...)
}
