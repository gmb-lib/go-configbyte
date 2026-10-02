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

func TestTheSectionAnswerNamesTheSectionItsSchemaItsReferencesAndItsTokens(t *testing.T) {
	a, err := NewSectionAnswer("orders", "svc:orders", []string{"orders", "stock-keeping"}, []string{"stock"})
	qt.Assert(t, qt.IsNil(err))
	raw, err := json.Marshal(a)
	qt.Assert(t, qt.IsNil(err))
	qt.Check(t, qt.Equals(string(raw),
		`{"section":"orders","schema":"orders-config/1","refersTo":["stock"],"audience":"svc:orders","scopeKeys":["orders","stock-keeping"]}`))

	none, err := NewSectionAnswer("orders", "svc:orders", []string{"orders"}, nil)
	qt.Assert(t, qt.IsNil(err))
	raw, _ = json.Marshal(none)
	qt.Check(t, qt.Equals(string(raw), `{"section":"orders","schema":"orders-config/1","refersTo":[],"audience":"svc:orders","scopeKeys":["orders"]}`),
		qt.Commentf("no references is an empty list, never absent"))
}

func TestAnAnswerThatIsNotAConfigurationOwnersFailsItsCheck(t *testing.T) {
	good := func() SectionAnswer {
		return SectionAnswer{Section: "orders", Schema: "orders-config/1", RefersTo: []string{"stock"},
			Audience: "svc:orders", ScopeKeys: []string{"orders"}}
	}
	for name, edit := range map[string]func(a *SectionAnswer){
		"a name out of shape":        func(a *SectionAnswer) { a.Section, a.Schema = "Orders", "Orders-config/1" },
		"another section's schema":   func(a *SectionAnswer) { a.Schema = "stock-config/1" },
		"references not said":        func(a *SectionAnswer) { a.RefersTo = nil },
		"a reference out of shape":   func(a *SectionAnswer) { a.RefersTo = []string{"Stock"} },
		"a reference to itself":      func(a *SectionAnswer) { a.RefersTo = []string{"orders"} },
		"a reference named twice":    func(a *SectionAnswer) { a.RefersTo = []string{"stock", "stock"} },
		"the schema of a later form": func(a *SectionAnswer) { a.Schema = "orders-config/2" },
		"no audience":                func(a *SectionAnswer) { a.Audience = "" },
		"an audience with a space":   func(a *SectionAnswer) { a.Audience = "svc:orders svc:stock" },
		"an audience out of bounds":  func(a *SectionAnswer) { a.Audience = "svc:" + strings.Repeat("o", 300) },
		"no scope keys":              func(a *SectionAnswer) { a.ScopeKeys = nil },
		"an empty list of keys":      func(a *SectionAnswer) { a.ScopeKeys = []string{} },
		"a scope key out of shape":   func(a *SectionAnswer) { a.ScopeKeys = []string{"orders:read"} },
		"a scope key named twice":    func(a *SectionAnswer) { a.ScopeKeys = []string{"orders", "orders"} },
	} {
		a := good()
		edit(&a)
		qt.Check(t, qt.IsNotNil(a.Check()), qt.Commentf("%s", name))
	}
	qt.Check(t, qt.IsNil(good().Check()))
}

// A part's hash is the same whether it is taken from the bytes the owner holds
// or from a file the part travelled in, re-indented on the way and with its
// characters escaped as an encoder escapes them — and it is the form the part
// takes inside a document's content hash.
func TestAPartHashesTheSameAsItTravelsAndAsAFileHoldsIt(t *testing.T) {
	held := []byte(`{"schema":"orders-config/1","label":"R&D <north>","list":[1,2]}`)
	file := []byte("{\n  \"schema\": \"orders-config/1\",\n  \"label\": \"R\\u0026D \\u003cnorth\\u003e\",\n  \"list\": [\n    1,\n    2\n  ]\n}")

	h := PartHash(held)
	qt.Check(t, qt.IsTrue(ValidHash(h)), qt.Commentf("%s", h))
	qt.Check(t, qt.Equals(PartHash(file), h))

	travelled, err := json.Marshal(json.RawMessage(held))
	qt.Assert(t, qt.IsNil(err))
	sum := sha256.Sum256(travelled)
	qt.Check(t, qt.Equals(h, "sha256:"+hex.EncodeToString(sum[:])), qt.Commentf("the hash is over the part as it travels"))

	qt.Check(t, qt.Not(qt.Equals(PartHash([]byte(`{"schema":"orders-config/1","label":"R&D"}`)), h)))
}

func TestOnlyASha256HashHasTheShapeOfOne(t *testing.T) {
	qt.Check(t, qt.IsTrue(ValidHash("sha256:"+strings.Repeat("a0", 32))))
	for _, s := range []string{"", "sha256:", "sha256:" + strings.Repeat("A0", 32), "sha1:" + strings.Repeat("a", 40),
		"sha256:" + strings.Repeat("a", 63), "sha256:" + strings.Repeat("a", 65), " sha256:" + strings.Repeat("a0", 32)} {
		qt.Check(t, qt.IsFalse(ValidHash(s)), qt.Commentf("%q", s))
	}
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
