package transfer

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/gmb-lib/go-authbyte/authclient"
	pkerrors "github.com/gmb-lib/go-platform-kit/errors"

	"github.com/gmb-lib/go-configbyte/document"
)

// maxTitle is the most of an owner's title a section's line carries, in
// characters: a title is a line, not a body.
const maxTitle = 200

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

// publicProblem is an owner's refusal or failure as a section's line carries
// it: only the part of its problem meant for whoever reads the answer — its
// code, status, title, type and trace id — never the services it passed
// through, and never its detail, which is internal once decoded from another
// service's answer. The line travels inside an answer of 200, past the host's
// own projection of its errors, so it is projected here. Its status is the one
// the owner answered with, and its title is kept to a line. A body that is no
// problem, or carries no code of the fleet's shape, keeps only its status:
// refused for a 4xx, unavailable otherwise.
func publicProblem(status int, body []byte) json.RawMessage {
	if p, ok := pkerrors.ParseProblem(body); ok {
		if _, coded := pkerrors.ParseCode(p.Code); coded {
			p.Status = status
			pub := p.Public()
			if r := []rune(pub.Title); len(r) > maxTitle {
				pub.Title = string(r[:maxTitle])
			}
			if b, err := json.Marshal(pub); err == nil {
				return b
			}
		}
	}
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return json.RawMessage(`{"code":"err:upstream:refused","status":` + strconv.Itoa(status) + `}`)
	}

	return unavailable()
}
