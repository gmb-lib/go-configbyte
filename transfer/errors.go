package transfer

import (
	"errors"
	"net/http"

	"github.com/gmb-lib/go-authbyte/authclient"
	pkerrors "github.com/gmb-lib/go-platform-kit/errors"

	"github.com/gmb-lib/go-configbyte/document"
)

// ErrNothingHeld refuses a transfer for someone who holds nothing any owner of
// the deployment accepts, before any owner is called.
var ErrNothingHeld = errors.New("transfer: this account holds nothing any configuration owner here accepts")

// VersionRequired refuses an apply in which a section names no version: the
// document has to be previewed first, and applied with the versions the preview
// answered.
type VersionRequired struct{ Section string }

func (e *VersionRequired) Error() string {
	return "transfer: the " + e.Section + " section names no version"
}

// VersionMoved refuses an apply whose section changed after the document was
// previewed. Nothing was written.
type VersionMoved struct{ Section string }

func (e *VersionMoved) Error() string {
	return "transfer: the " + e.Section + " configuration changed after the document was previewed"
}

// OwnerRefused is an owner's own refusal of a call the transfer could not go on
// without. Body is the owner's answer as it came; it stays in its field, never
// in the message, because an error message ends up in logs.
type OwnerRefused struct {
	Section string
	Status  int
	Body    []byte
}

func (e *OwnerRefused) Error() string {
	return "transfer: the " + e.Section + " owner refused the call (" + http.StatusText(e.Status) + ")"
}

// Unreached is a call that produced no owner's answer: the authority refused
// the token for this person, or the hop failed.
type Unreached struct {
	Section string
	// What names the call, such as "configuration export from the orders owner".
	What string
	Err  error
}

func (e *Unreached) Error() string { return e.What + " failed: " + e.Err.Error() }
func (e *Unreached) Unwrap() error { return e.Err }

// unreached records a call that produced no owner's answer, telling an
// authority's refusal from a failed hop, and answers it as an [*Unreached].
func (t *Transfer) unreached(section, what string, err error) error {
	if _, ok := authorityRefusal(err); ok {
		t.warn(what + " refused by the authority")
	} else {
		t.warn(what + " failed: " + err.Error())
	}

	return &Unreached{Section: section, What: what, Err: err}
}

// authorityRefusal answers the authority's refusal to mint a token for this
// person, when that is what err is: an answer somebody decided, which the
// person can usually act on, unlike a hop that failed.
func authorityRefusal(err error) (*authclient.Error, bool) {
	var refused *authclient.Error
	if errors.As(err, &refused) &&
		refused.Status >= http.StatusBadRequest && refused.Status < http.StatusInternalServerError {
		return refused, true
	}

	return nil, false
}

// Problem answers the problem a host renders for an error of this package, so
// every host answers the same code, status and words; service is the host's
// own name, the one a relayed problem carries. An error that is none of this
// package's is answered as it came.
//
//   - [ErrNothingHeld]: 403 err:request:forbidden.
//   - [*document.Invalid]: 422 err:config:badDocument with its reason.
//   - [*VersionRequired]: 428 err:config:versionRequired, naming the section.
//   - [*VersionMoved]: 412 err:config:versionMoved, naming the section.
//   - [*OwnerRefused]: the owner's problem relayed with its own code and words.
//   - [*Unreached]: the authority's refusal relayed, or 502
//     err:upstream:unavailable when the hop failed.
//
// The words are public details, so they reach a person when the host is the
// edge the person's application talks to. A host behind another edge hands on
// the code and the status; that edge decides the words.
func Problem(err error, service string) error {
	var (
		invalid   *document.Invalid
		required  *VersionRequired
		moved     *VersionMoved
		refused   *OwnerRefused
		unreached *Unreached
	)

	switch {
	case errors.Is(err, ErrNothingHeld):
		return pkerrors.NewProblem("err:request:forbidden",
			pkerrors.WithStatus(http.StatusForbidden),
			pkerrors.WithPublicDetail("this account holds nothing any configuration owner here accepts"))
	case errors.As(err, &invalid):
		return pkerrors.NewProblem(document.CodeBadDocument,
			pkerrors.WithStatus(http.StatusUnprocessableEntity),
			pkerrors.WithPublicDetail(invalid.Why))
	case errors.As(err, &required):
		return pkerrors.NewProblem(document.CodeVersionRequired,
			pkerrors.WithStatus(http.StatusPreconditionRequired),
			pkerrors.WithPublicDetail("the "+required.Section+" section names no version: preview the document, then apply it with the versions the preview answered"))
	case errors.As(err, &moved):
		return pkerrors.NewProblem(document.CodeVersionMoved,
			pkerrors.WithStatus(http.StatusPreconditionFailed),
			pkerrors.WithPublicDetail("the "+moved.Section+" configuration changed after the document was previewed; nothing was written — preview it again"))
	case errors.As(err, &refused):
		return relayed(service, refused.Status, refused.Body)
	case errors.As(err, &unreached):
		if authority, ok := authorityRefusal(unreached.Err); ok {
			return relayed(service, authority.Status, []byte(authority.Body))
		}

		return pkerrors.NewProblem("err:upstream:unavailable", pkerrors.WithStatus(http.StatusBadGateway))
	}

	return err
}

// relayed answers an owner's refusal, keeping the one thing a person always
// needs out of it: that they were refused, rather than that something broke.
//
// A body that can be read keeps its own code and its own words. A body that
// cannot still keeps its status, and says only that the request was refused —
// never a reason invented to fill the gap. A gateway failure is reserved for
// what it describes: the owner broke, or was never reached at all.
func relayed(service string, status int, body []byte) error {
	if down, ok := pkerrors.ParseProblem(body); ok {
		return pkerrors.Relay(down, service, status)
	}

	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return pkerrors.NewProblem("err:upstream:refused", pkerrors.WithStatus(status))
	}

	return pkerrors.NewProblem("err:upstream:unavailable", pkerrors.WithStatus(http.StatusBadGateway))
}
