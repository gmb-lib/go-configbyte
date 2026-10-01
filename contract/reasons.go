package contract

import (
	"net/http"

	pkerrors "github.com/gmb-lib/go-platform-kit/errors"
)

// The reasons the contract refuses with. Each owner raises them in its own
// error domain — "err:<domain>:config_version_moved" — so a refusal names the
// service that gave it, and every owner's refusal of the same kind carries the
// same status and the same public title.
const (
	// ReasonBadDocument is a document that cannot be read as this owner's
	// section: not JSON, not an object, another section's schema, a part that is
	// not a list, or an entry out of shape.
	ReasonBadDocument = "config_bad_document"
	// ReasonKeyConflict is a key that already means something else here: a
	// change to what an item is (its kind, type, unit or format) on an existing
	// key. A new meaning arrives as a new key.
	ReasonKeyConflict = "config_key_conflict"
	// ReasonVersionRequired is an apply that names no version: nothing says the
	// document was reviewed against the configuration it would be written to.
	ReasonVersionRequired = "config_version_required"
	// ReasonVersionMoved is an apply naming a version the section has moved
	// past since it was previewed. Nothing is written.
	ReasonVersionMoved = "config_version_moved"
)

// Reasons is each reason's status and public title. The package registers them
// with the error taxonomy when it loads, so a refusal crosses every hop as
// itself rather than as an opaque internal error.
var Reasons = map[string]pkerrors.ReasonSpec{
	ReasonBadDocument:     {Status: http.StatusUnprocessableEntity, Title: "Malformed configuration document"},
	ReasonKeyConflict:     {Status: http.StatusConflict, Title: "Configuration key already means something else"},
	ReasonVersionRequired: {Status: http.StatusPreconditionRequired, Title: "Configuration version required"},
	ReasonVersionMoved:    {Status: http.StatusPreconditionFailed, Title: "Configuration changed since it was previewed"},
}

func init() {
	for reason, spec := range Reasons {
		pkerrors.RegisterReason(reason, spec)
	}
}

// Code is a reason as an owner raises it: "err:<domain>:<reason>".
func Code(domain, reason string) string {
	return pkerrors.Prefix + ":" + domain + ":" + reason
}

// Refusal is the error an owner answers a refused request with: the reason in
// its domain, rendered with the reason's status and title, and the sentence as
// its detail.
func Refusal(domain, reason, detail string) error {
	return pkerrors.FromResultCode(Code(domain, reason), detail)
}
