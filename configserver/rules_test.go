package configserver_test

import (
	"strings"
	"testing"

	"azugo.io/azugo"
	"azugo.io/azugo/user"
	"github.com/go-quicktest/qt"

	"github.com/gmb-lib/go-authbyte/permissions"

	"github.com/gmb-lib/go-configbyte/configserver"
	"github.com/gmb-lib/go-configbyte/contract"
)

// A service keeping two registers, each with its own ladder, whose one section
// reaches into both: either register's reader may read it, and applying it
// needs the setup of both — every one of the three setup permissions for a
// person, both administering rungs (or the three permissions) for a service
// acting as itself.
var registers = permissions.MustNew("registers", "Registers", []permissions.Permission{
	{Feature: "setup", Act: "certificateKinds", Description: "Define certificate kinds", Class: permissions.TenantConfiguration, Plane: permissions.Tenant},
	{Feature: "setup", Act: "assetDateKinds", Description: "Define asset date kinds", Class: permissions.TenantConfiguration, Plane: permissions.Tenant},
	{Feature: "setup", Act: "assetFields", Description: "Define asset fields", Class: permissions.TenantConfiguration, Plane: permissions.Tenant},
	{Feature: "workforce/person", Act: "view", Description: "See people", Class: permissions.Ordinary, Plane: permissions.Object},
})

func twoRegisters(o *configserver.Owner, s *served) {
	s.gate = registers.Gate(func(_ *azugo.Context, required string) { s.refused = append(s.refused, required) })
	o.Section, o.Domain, o.Gate = "orders", "registers", s.gate
	o.Read = configserver.AnyLevel(permissions.Levels("workforce", "read"), permissions.Levels("assets", "read"))
	o.Write = configserver.Rule{
		Level: configserver.EveryLevel(permissions.Levels("workforce", "admin"), permissions.Levels("assets", "admin")),
		Perms: []permissions.Perm{
			registers.Declared("setup", "certificateKinds"),
			registers.Declared("setup", "assetDateKinds"),
			registers.Declared("setup", "assetFields"),
		},
		All: true,
	}
}

const allThreeBoxes = "registers/setup:certificateKinds registers/setup:assetDateKinds registers/setup:assetFields"

func TestEitherRegistersReaderReadsAndOnlyBothAdministrationsApply(t *testing.T) {
	s := serve(t, twoRegisters)

	for _, scopes := range []string{"workforce:read", "assets:read", "registers/workforce/person:view"} {
		c := caller{scopes: scopes, sub: "svc:machine", tenant: "tenant-a"}
		qt.Check(t, qt.Equals(s.do("GET", contract.PathConfig, c, "").status, 200), qt.Commentf("%s", scopes))
		qt.Check(t, qt.Equals(s.do("GET", contract.PathVersion, c, "").status, 200), qt.Commentf("%s", scopes))
	}
	qt.Check(t, qt.Equals(s.do("GET", contract.PathConfig, caller{scopes: "projects:read projects/task:view", sub: "svc:other", tenant: "tenant-a"}, "").status, 403),
		qt.Commentf("another service's levels and permissions reach nothing here"))

	passes := []caller{
		{scopes: "workforce:admin assets:admin", sub: "svc:both", tenant: "tenant-a"},
		{scopes: allThreeBoxes, sub: "svc:boxes", tenant: "tenant-a"},
		{scopes: allThreeBoxes, tenant: "tenant-a"},
	}
	for _, c := range passes {
		qt.Check(t, qt.Equals(s.do("POST", contract.PathPreview, c, doc).status, 200), qt.Commentf("%v", c))
		qt.Check(t, qt.Equals(s.do("POST", contract.PathApply, c, doc, "If-Match", `"v"`).status, 200), qt.Commentf("%v", c))
	}
	refused := []caller{
		{scopes: "workforce:admin workforce:write workforce:read", sub: "svc:one", tenant: "tenant-a"},
		{scopes: "assets:admin assets:write assets:read", sub: "svc:one", tenant: "tenant-a"},
		{scopes: "workforce:write assets:write workforce:read assets:read", sub: "svc:lower", tenant: "tenant-a"},
		{scopes: "workforce:admin assets:admin", tenant: "tenant-a"},
		{scopes: "registers/setup:certificateKinds registers/setup:assetDateKinds", tenant: "tenant-a"},
		{scopes: "registers/setup:certificateKinds registers/setup:assetFields", sub: "svc:two-boxes", tenant: "tenant-a"},
	}
	for _, c := range refused {
		qt.Check(t, qt.Equals(s.do("POST", contract.PathPreview, c, doc).status, 403), qt.Commentf("%v", c))
		qt.Check(t, qt.Equals(s.do("POST", contract.PathApply, c, doc, "If-Match", `"v"`).status, 403), qt.Commentf("%v", c))
	}

	said := strings.Join(s.refused, "|")
	qt.Check(t, qt.StringContains(said, "workforce:admin and assets:admin or all of "+allThreeBoxes))
	qt.Check(t, qt.StringContains(said, "workforce:read or assets:read or any registers/ permission"))
}

// The combined rungs name what would have been enough the way a refusal says
// it, and skip a rung that holds nothing.
func TestCombinedRungsNameThemselvesAndHoldAsSaid(t *testing.T) {
	wf, as := permissions.Levels("workforce", "read"), permissions.Levels("assets", "read")
	either := configserver.AnyLevel(wf, as, permissions.Level{})
	both := configserver.EveryLevel(wf, as)
	qt.Check(t, qt.Equals(either.Name, "workforce:read or assets:read"))
	qt.Check(t, qt.Equals(both.Name, "workforce:read and assets:read"))

	for scopes, want := range map[string][2]bool{
		"workforce:read":             {true, false},
		"assets:read":                {true, false},
		"workforce:read assets:read": {true, true},
		"workforce:write":            {false, false},
		"":                           {false, false},
	} {
		u := user.NewIdentity("svc:x", scopes, nil)
		qt.Check(t, qt.Equals(either.Holds(u), want[0]), qt.Commentf("either, %q", scopes))
		qt.Check(t, qt.Equals(both.Holds(u), want[1]), qt.Commentf("both, %q", scopes))
	}

	none := configserver.EveryLevel()
	qt.Check(t, qt.IsNil(none.Holds), qt.Commentf("no rung at all is the zero level, which nobody holds"))
}
