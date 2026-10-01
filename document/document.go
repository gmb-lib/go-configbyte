// Package document is the configuration document: one JSON file carrying the
// sections of every owner a deployment runs, which an administrator exports,
// may edit, and imports — into the same organisation or into another one.
//
//	{
//	  "gmbConfig": "1.0",
//	  "exportedAt": "2026-01-31T09:00:00Z",
//	  "tenant": {"id": "…"},
//	  "contentHash": "sha256:…",
//	  "sections": {
//	    "orders": {"schema": "orders-config/1", …},
//	    "inventory": {"schema": "inventory-config/1", …}
//	  }
//	}
//
// Each section is its owner's own payload, verbatim. The tenant block is
// informative: an import always lands in the importing administrator's own
// organisation. The content hash answers, honestly, whether the sections are
// still the exact export — a flag in the import's answer, never a gate, because
// editing the document is the feature working. The file is not a security
// boundary; the permission to import one is.
//
// "gmbConfig" is the version of this document's format. It is not a section's
// version, which is each owner's own token and never travels in the file: a
// token is the importing organisation's own, so an apply names the version its
// dry run in that organisation answered, in Expect.
package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// Version is the format version this package writes.
const Version = "1.0"

// Document is the transport document.
type Document struct {
	GmbConfig   string                     `json:"gmbConfig"`
	ExportedAt  string                     `json:"exportedAt,omitempty"`
	Tenant      *Tenant                    `json:"tenant,omitempty"`
	ContentHash string                     `json:"contentHash,omitempty"`
	Sections    map[string]json.RawMessage `json:"sections"`
	// Expect names, per section, the version its owner answered in the dry run
	// the administrator read; an apply is written only against it. An exported
	// file never carries it.
	Expect map[string]string `json:"expect,omitempty"`
}

// Tenant names the organisation a document was exported from.
type Tenant struct {
	ID string `json:"id,omitempty"`
}

// New builds the document for an export of the sections, from the organisation
// named, at the moment given.
func New(sections map[string]json.RawMessage, tenant string, at time.Time) *Document {
	return &Document{
		GmbConfig:   Version,
		ExportedAt:  at.UTC().Format(time.RFC3339),
		Tenant:      &Tenant{ID: tenant},
		ContentHash: ContentHash(sections),
		Sections:    sections,
	}
}

// Encode writes the document indented, for a person to read and edit.
func (d *Document) Encode() ([]byte, error) {
	return json.MarshalIndent(d, "", "  ")
}

// ContentHash is "sha256:" and the hex SHA-256 over the canonical sections
// object: each section compacted, the object's keys sorted. Every reader
// computes it the same way, so a document exported by one is recognised by
// another however it was re-indented on the way.
func ContentHash(sections map[string]json.RawMessage) string {
	compact := make(map[string]json.RawMessage, len(sections))
	for k, raw := range sections {
		var buf bytes.Buffer
		if err := json.Compact(&buf, raw); err != nil {
			compact[k] = raw

			continue
		}
		compact[k] = json.RawMessage(buf.String())
	}
	canonical, err := json.Marshal(compact) // map keys marshal sorted
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canonical)

	return "sha256:" + hex.EncodeToString(sum[:])
}

// Invalid is a document a reader cannot honestly interpret. Why is a sentence
// fit for the person who sent it.
type Invalid struct {
	Why string
}

func (e *Invalid) Error() string { return "configuration document: " + e.Why }

// Parse reads a document for an import: valid JSON, a 1.x format version, and at
// least one section. The sections themselves are never judged here — that is
// each owner's job.
func Parse(body []byte) (*Document, error) {
	var d Document
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, &Invalid{Why: "the document is not valid JSON"}
	}
	if !strings.HasPrefix(d.GmbConfig, "1.") {
		return nil, &Invalid{Why: "the document does not declare a gmbConfig 1.x version"}
	}
	if len(d.Sections) == 0 {
		return nil, &Invalid{Why: "the document carries no sections"}
	}

	return &d, nil
}

// Edited answers whether the sections differ from the export the document names
// by its hash, or nil when the document carries no hash to compare against.
func (d *Document) Edited() *bool {
	if d.ContentHash == "" {
		return nil
	}
	edited := ContentHash(d.Sections) != d.ContentHash

	return &edited
}
