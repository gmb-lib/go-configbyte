package document

import "encoding/json"

// Outcome is one word for what an import did to the whole document.
type Outcome string

const (
	// Previewed is a dry run with nothing refused.
	Previewed Outcome = "previewed"
	// Applied is every section written.
	Applied Outcome = "applied"
	// Refused is a document one owner refused: nothing was written, and the
	// clean sections are reported withheld.
	Refused Outcome = "refused"
	// RolledBack is a document whose later section failed after earlier ones
	// were written; each written section was restored to what it held before.
	RolledBack Outcome = "rolledBack"
	// Partial is a document a restore could not complete: each section's state
	// is named.
	Partial Outcome = "partial"
	// Failed is a document an owner could not be asked about, or whose first
	// write failed: nothing was written.
	Failed Outcome = "failed"
)

// Status is what an import did to one section.
type Status string

const (
	// SectionPreviewed is a clean dry run of the section.
	SectionPreviewed Status = "previewed"
	// SectionApplied is the section written.
	SectionApplied Status = "applied"
	// SectionRefused is a section its owner refused an item of.
	SectionRefused Status = "refused"
	// SectionWithheld is a clean section not written because another section
	// was refused or failed.
	SectionWithheld Status = "withheld"
	// SectionRolledBack is a section written, then restored because a later
	// section failed.
	SectionRolledBack Status = "rolledBack"
	// SectionSkipped is a section nobody in the deployment owns, or one the
	// importing account may not reach. It is reported, never dropped silently.
	SectionSkipped Status = "skipped"
	// SectionFailed is a section whose owner could not be asked, or refused the
	// call itself.
	SectionFailed Status = "failed"
)

// SectionResult is one section's line in an import's answer.
type SectionResult struct {
	Status Status `json:"status"`
	// Version is the owner's version as the answer leaves the section; after a
	// dry run, the value an apply names in Expect.
	Version string `json:"version,omitempty"`
	// Report is the owner's report, verbatim, when it answered one.
	Report json.RawMessage `json:"report,omitempty"`
	// Problem is the owner's refusal, verbatim, when the call itself was
	// refused or failed.
	Problem json.RawMessage `json:"problem,omitempty"`
	// Detail says why a section was skipped, or what a restore did to it.
	Detail string `json:"detail,omitempty"`
}

// Answer is an import's answer: one outcome for the whole document and one line
// per section. Its members are written in byte order, the order an importer
// building the answer as a map has always written them.
type Answer struct {
	// DocumentEdited is whether the sections differ from the export the
	// document names by hash; absent when it carries none.
	DocumentEdited *bool                    `json:"documentEdited,omitempty"`
	DryRun         bool                     `json:"dryRun"`
	Outcome        Outcome                  `json:"outcome"`
	Sections       map[string]SectionResult `json:"sections"`
}

// The refusals an importer answers for a document it will not start on.
const (
	// CodeBadDocument (422) is a document Parse refused.
	CodeBadDocument = "err:config:badDocument"
	// CodeVersionRequired (428) is an apply naming no version for a section.
	CodeVersionRequired = "err:config:versionRequired"
	// CodeVersionMoved (412) is a section that moved after its dry run.
	CodeVersionMoved = "err:config:versionMoved"
)
