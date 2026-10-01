package configserver

import (
	"strings"

	"github.com/gmb-lib/go-authbyte/permissions"
)

// The act of applying a configuration document, as this package contributes it
// to a service's permissions: "<service>/setup:import".
const (
	ImportFeature = "setup"
	ImportAct     = "import"
)

// Permissions is this package's contribution to the permissions a service
// declares: the one act of applying a configuration document, granted across
// the tenant and changing the tenant's own setup. A service adds it to its Set
// beside its own list:
//
//	var Permissions = permissions.MustNew("projects", "Projects", own, configserver.Permissions())
//
// A service whose documents are applied by the setup permissions it already
// declares — one per list the section carries — leaves it out and names those
// in its write rule instead.
func Permissions() []permissions.Permission {
	return []permissions.Permission{{
		Feature:     ImportFeature,
		Act:         ImportAct,
		Description: "Apply a configuration file",
		Class:       permissions.TenantConfiguration,
		Plane:       permissions.Tenant,
		Labels:      map[string]string{"lv": "Importēt konfigurācijas failu"},
	}}
}

// Import is the import permission as the service's Set declares it. It stops
// the service at load when the Set does not carry this package's contribution.
func Import(set *permissions.Set) permissions.Perm {
	return set.Declared(ImportFeature, ImportAct)
}

// ImportRule is the write rule of a service whose documents are applied by the
// import permission: a service acting as itself passes by level, or by the
// permission; a person by the permission.
func ImportRule(set *permissions.Set, level permissions.Level) Rule {
	return Rule{Level: level, Perms: []permissions.Perm{Import(set)}}
}

// AnyLevel passes a caller holding any one of the levels: a section that
// reaches into two ladders is read with either one's reading rung.
func AnyLevel(levels ...permissions.Level) permissions.Level {
	return combine(levels, " or ", false)
}

// EveryLevel passes a caller holding every one of the levels: a document that
// reaches into two ladders is applied only with both administering rungs.
func EveryLevel(levels ...permissions.Level) permissions.Level {
	return combine(levels, " and ", true)
}

func combine(levels []permissions.Level, word string, every bool) permissions.Level {
	names := make([]string, 0, len(levels))
	checks := make([]func(permissions.Scopes) bool, 0, len(levels))
	for _, l := range levels {
		if l.Holds == nil {
			continue
		}
		names = append(names, l.Name)
		checks = append(checks, l.Holds)
	}
	if len(checks) == 0 {
		return permissions.Level{}
	}

	return permissions.Level{
		Name: strings.Join(names, word),
		Holds: func(u permissions.Scopes) bool {
			for _, holds := range checks {
				if holds(u) != every {
					return !every
				}
			}

			return every
		},
	}
}
