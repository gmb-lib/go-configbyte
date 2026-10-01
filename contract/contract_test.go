package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/go-quicktest/qt"
)

func TestTheFiveOperationsLiveUnderTheConfigWord(t *testing.T) {
	for _, p := range []string{PathConfig, PathPreview, PathApply, PathVersion, PathSection} {
		qt.Check(t, qt.IsTrue(p == "/config" || strings.HasPrefix(p, "/config/")), qt.Commentf("%s", p))
	}
	qt.Check(t, qt.DeepEquals([]string{PathPreview, PathApply, PathVersion, PathSection},
		[]string{"/config/preview", "/config/apply", "/config/version", "/config/section"}))
}

func TestASectionTravelsUnderItsNameAndTheFirstSchema(t *testing.T) {
	qt.Check(t, qt.Equals(Schema("orders"), "orders-config/1"))
	qt.Check(t, qt.Equals(Schema("stock-register"), "stock-register-config/1"))
}

func TestASectionNameIsLowerCaseLettersDigitsAndDashes(t *testing.T) {
	for _, ok := range []string{"orders", "stock-register", "a1"} {
		qt.Check(t, qt.IsTrue(ValidName(ok)), qt.Commentf("%q", ok))
	}
	for _, bad := range []string{"", "Orders", "1orders", "-orders", "orders/x", "or ders", strings.Repeat("a", 65)} {
		qt.Check(t, qt.IsFalse(ValidName(bad)), qt.Commentf("%q", bad))
	}
}

// The token is the SHA-256 of the schema, the scope and the section's bytes,
// each on its own line: the same formula an owner computing it in its database
// uses, so a token means the same wherever it was computed.
func TestTheTokenIsTheSaltedHashOfTheSectionBytes(t *testing.T) {
	section := []byte(`{"schema": "orders-config/1", "priorities": []}`)
	sum := sha256.Sum256([]byte("orders-config/1\ntenant-a\n" + string(section)))

	qt.Check(t, qt.Equals(Token("orders-config/1", "tenant-a", section), "sha256:"+hex.EncodeToString(sum[:])))
}

// Same bytes, same token; any other byte, scope or schema is another token, so
// two tenants holding one configuration never share one.
func TestATokenMovesWithTheBytesAndIsNeverShared(t *testing.T) {
	section := []byte(`{"schema":"orders-config/1","priorities":[{"key":"high","label":"High"}]}`)
	v := Token("orders-config/1", "tenant-a", section)

	qt.Check(t, qt.Equals(Token("orders-config/1", "tenant-a", append([]byte{}, section...)), v))
	qt.Check(t, qt.Not(qt.Equals(Token("orders-config/1", "tenant-b", section), v)), qt.Commentf("another tenant"))
	qt.Check(t, qt.Not(qt.Equals(Token("stock-config/1", "tenant-a", section), v)), qt.Commentf("another section"))
	edited := []byte(strings.Replace(string(section), "High", "Urgent", 1))
	qt.Check(t, qt.Not(qt.Equals(Token("orders-config/1", "tenant-a", edited), v)), qt.Commentf("another byte"))
	// The line breaks keep the parts apart: moving a character across the
	// boundary between scope and section is another input.
	qt.Check(t, qt.Not(qt.Equals(Token("orders-config/1", "tenant-a{", section[1:]), v)))
}

func TestAVersionTravelsQuotedAsTheETagAndIsReadBackFromIfMatch(t *testing.T) {
	v := "sha256:abc"
	qt.Check(t, qt.Equals(ETag(v), `"sha256:abc"`))
	for _, header := range []string{`"sha256:abc"`, `W/"sha256:abc"`, `  "sha256:abc" `, `sha256:abc`} {
		qt.Check(t, qt.Equals(EntityTag(header), v), qt.Commentf("%q", header))
	}
	for _, none := range []string{"", "  ", `""`, `W/""`} {
		qt.Check(t, qt.Equals(EntityTag(none), ""), qt.Commentf("%q names no version", none))
	}
}

func FuzzEntityTagReadsBackEveryETag(f *testing.F) {
	for _, s := range []string{"sha256:abc", "", "W/", `"`, "a\"b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		got := EntityTag(v)
		if strings.HasPrefix(got, `"`) || strings.HasSuffix(got, `"`) {
			t.Fatalf("EntityTag(%q) = %q keeps a quote", v, got)
		}
		clean := strings.Trim(strings.TrimSpace(v), `"`)
		if clean == v && !strings.HasPrefix(v, "W/") && EntityTag(ETag(v)) != v {
			t.Fatalf("EntityTag(ETag(%q)) = %q", v, EntityTag(ETag(v)))
		}
	})
}

func TestTheSectionAnswerNamesTheSectionItsSchemaAndItsReferences(t *testing.T) {
	a, err := NewSectionAnswer("orders", "stock")
	qt.Assert(t, qt.IsNil(err))
	raw, err := json.Marshal(a)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(string(raw), `{"section":"orders","schema":"orders-config/1","refersTo":["stock"]}`))

	none, err := NewSectionAnswer("orders")
	qt.Assert(t, qt.IsNil(err))
	raw, _ = json.Marshal(none)
	qt.Check(t, qt.Equals(string(raw), `{"section":"orders","schema":"orders-config/1","refersTo":[]}`),
		qt.Commentf("no references is an empty list, never absent"))
}

func TestAnAnswerThatIsNotAConfigurationOwnersFailsItsCheck(t *testing.T) {
	for name, a := range map[string]SectionAnswer{
		"a name out of shape":        {Section: "Orders", Schema: "Orders-config/1", RefersTo: []string{}},
		"another section's schema":   {Section: "orders", Schema: "stock-config/1", RefersTo: []string{}},
		"references not said":        {Section: "orders", Schema: "orders-config/1"},
		"a reference out of shape":   {Section: "orders", Schema: "orders-config/1", RefersTo: []string{"Stock"}},
		"a reference to itself":      {Section: "orders", Schema: "orders-config/1", RefersTo: []string{"orders"}},
		"a reference named twice":    {Section: "orders", Schema: "orders-config/1", RefersTo: []string{"stock", "stock"}},
		"the schema of a later form": {Section: "orders", Schema: "orders-config/2", RefersTo: []string{}},
	} {
		qt.Check(t, qt.IsNotNil(a.Check()), qt.Commentf("%s", name))
	}
	qt.Check(t, qt.IsNil(SectionAnswer{Section: "orders", Schema: "orders-config/1", RefersTo: []string{"stock"}}.Check()))
}

// Every reason the contract refuses with renders with its status, keeps the
// owner's domain in its code, and carries a public title — a reason left
// unregistered would render as an opaque 500 while looking right everywhere
// else.
func TestEveryReasonCarriesItsStatusItsCodeAndATitle(t *testing.T) {
	for reason, want := range map[string]int{
		ReasonBadDocument:     422,
		ReasonKeyConflict:     409,
		ReasonVersionRequired: 428,
		ReasonVersionMoved:    412,
	} {
		err := Refusal("orders", reason, "the key is named here")
		var status interface{ StatusCode() int }
		qt.Assert(t, qt.IsTrue(errors.As(err, &status)), qt.Commentf("%s renders with a status, got %T", reason, err))
		qt.Check(t, qt.Equals(status.StatusCode(), want), qt.Commentf("%s", reason))
		var code interface{ ErrorCode() string }
		qt.Assert(t, qt.IsTrue(errors.As(err, &code)), qt.Commentf("%s keeps a code", reason))
		qt.Check(t, qt.Equals(code.ErrorCode(), "err:orders:"+reason))
		qt.Check(t, qt.Not(qt.Equals(Reasons[reason].Title, "")), qt.Commentf("%s carries a public title", reason))
	}
	qt.Check(t, qt.Equals(len(Reasons), 4))
	qt.Check(t, qt.Equals(Code("orders", ReasonVersionMoved), "err:orders:config_version_moved"))
}
