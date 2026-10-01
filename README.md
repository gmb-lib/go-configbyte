# go-configbyte

The tenant configuration contract, written once, for Go services built on [Azugo](https://azugo.io).

A service that holds a tenant's configuration — its kinds of work, their stages, its field definitions, its
lists of words — answers the same five operations, at the same address, with the same rules. That is what lets
one read hand an application its whole vocabulary at sign-in, one document carry a tenant's setup from one
installation to another, and one administration console write to every service without any of them sharing a
database. Each service keeps its own configuration where it already is; this library is the part they would
otherwise each write by hand.

```sh
go get github.com/gmb-lib/go-configbyte
```

See [`CHANGELOG.md`](./CHANGELOG.md) for what each release changed, and what it means for code that already uses
this library, before you bump.

## The contract

Every service that holds tenant configuration exposes these five, inside its own versioned API root, and no
variants:

| Method | Path | Answers |
|---|---|---|
| `GET` | `<root>/config` | the tenant's whole section, verbatim, with its **version as the `ETag`** |
| `POST` | `<root>/config/preview` | exactly what a document would change, item by item — **writes nothing** |
| `POST` | `<root>/config/apply` | the change, **atomically and idempotently**, naming its version in **`If-Match`** |
| `GET` | `<root>/config/version` | the version alone: the cheap "has anything changed?" |
| `GET` | `<root>/config/section` | which section answers here and which sections it refers to — **no tenant, no person** |

And the rules behind them:

- **A section is the owning API's own payload**, named `<section>-config/1`. There is no second format "for
  export".
- **The version is derived from the section's bytes**: a SHA-256 over the section, salted with its name and its
  tenant. A token computed from the data cannot disagree with the data, whoever wrote it and however. It is
  opaque — compared for equality, never ordered.
- **A preview writes nothing**, and reports what an apply of the same document would do: each item `added`,
  `changed` (naming the members), `unchanged` or `refused` (naming the reason and saying why), by key.
- **An apply names the version its preview was read against.** None named: `428`. Moved since: `412`. Nothing is
  written in either case.
- **One refused item and nothing is written.** A changed kind, type, unit or format on an existing key is refused
  (`config_key_conflict`, `409` when raised): a new meaning is a new key.
- **A document never removes an item.** An item another service may name is retired — it stays in the section,
  valid for everything that already names it — because no check across two services can be atomic with a removal.
- **Reads are every member's**, because the section is the vocabulary everybody works in and holds nobody's
  records. **Writes are the administrator's.**

## Packages

| Package | What it is |
|---|---|
| [`contract`](./contract) | The vocabulary both sides speak: the paths, the schema name, the version token, the report and its statuses, the section answer, and the four reasons, registered with their status and public title |
| [`configserver`](./configserver) | The five operations for a service that owns a section: mounted on the service's router, behind its gate, over its own store |
| [`configclient`](./configclient) | Reading another owner's section: the version, the section with its `ETag`, the section answer, and a cache that hands a copy out only against the version its owner just confirmed |
| [`document`](./document) | The configuration document that carries every owner's section in one file: its types, its content hash, parsing an import, and the words an import answers with |
| [`match`](./match) | Judging a document against what an owner holds, item by item and by key, for an owner whose apply is written in Go |
| [`configtest`](./configtest) | The proof ladder: every promise above, checked against a service's own routes from its own tests |

## Owning a section

The service brings its store — the one place that reads its section and writes a document — and its gate. The
store answers the section and its version from one snapshot, and compares an apply's version with the section's
own under a per-tenant lock.

```go
type Store interface {
	ConfigRead(ctx context.Context, tenant string) (configserver.Read, error)
	ConfigVersion(ctx context.Context, tenant string) (string, error)
	ConfigApply(ctx context.Context, a configserver.Apply) (json.RawMessage, error)
}
```

```go
// The service's permissions, with this library's contribution: "<service>/setup:import".
var Permissions = permissions.MustNew("orders", "Orders", own, configserver.Permissions())

owner := &configserver.Owner{
	Section: "orders",                 // travels as "orders-config/1"
	Domain:  "orders",                 // refusals are "err:orders:…"
	Gate:    Permissions.Gate(deny),
	Read:    permissions.Levels("orders", "read"),
	Write:   configserver.ImportRule(Permissions, permissions.Levels("orders", "admin")),
	Tenant:  tenantOf, // the caller's tenant, or the refusal written
	Actor:   actorOf,  // who a change is attributed to
	Store:   storeOf,  // the store, or "not ready" written
	Applied: recordConfigChange,
}
if err := owner.Mount(v1); err != nil { // v1: the router group at "/api/v1", behind authentication
	return err
}
```

A person always reads; a service acting as itself reads by the reading rung or any permission the service
declares. A person writes by the permissions of the write rule and never by a rung; a service acting as itself by
the rung or the permissions. A section that reaches into two ladders says so:

```go
Read:  configserver.AnyLevel(permissions.Levels("workforce", "read"), permissions.Levels("assets", "read")),
Write: configserver.Rule{
	Level: configserver.EveryLevel(permissions.Levels("workforce", "admin"), permissions.Levels("assets", "admin")),
	Perms: []permissions.Perm{certificateKinds, assetDateKinds, assetFields},
	All:   true, // every one of them
},
```

The permissions are [go-authbyte](https://github.com/gmb-lib/go-authbyte)'s: the routes are checked through the
service's own gate, so its permission test sees the configuration routes with the rest.

## Reading another owner's section

The caller brings the transport — a function that reads one path below the owner's API root, on the caller's own
authority, so the owner answers exactly what it would answer the caller directly:

```go
fetch := func(ctx context.Context, path string) (*configclient.Response, error) { … }

cache := configclient.NewCache(1000)
body, fromCopy, err := cache.Current(ctx, tenant, "orders", fetch)

var refused *configclient.Refused
if errors.As(err, &refused) {
	// the owner answered, and refused: relay refused.Status and refused.Body as they came
}
```

`Current` asks the owner for its version every time, answers the copy when it was stored under that version, and
otherwise reads the section and keeps it under the version stamped on those very bytes. Nothing expires by time.
Whether a caller may be answered a section at all is decided before the cache is asked.

## Proving it

```go
func TestTheConfigurationContract(t *testing.T) {
	configtest.Gates{
		Service:    configtest.Service{Client: client, Root: "/api/v1", Section: "orders", Domain: "orders"},
		Anyone:     tokenWithoutTenant,
		Readers:    []configtest.Caller{member, readingService},
		Unreadable: []configtest.Caller{anotherServicesMachine},
		Writers:    []configtest.Caller{administrator, administeringService},
		Refused:    []configtest.Caller{member, personHoldingRungs},
	}.Run(t)
}
```

`configtest.Live` walks the rest against a real store: the unchanged document that moves nothing, the service's
own refusals — each reported alike by preview and apply, each writing nothing — a real change, a stale apply, and
another tenant's section.

## Dependencies

[Azugo](https://azugo.io) for routing, [go-authbyte](https://github.com/gmb-lib/go-authbyte) for permissions and
the service-subject marker, [go-platform-kit](https://github.com/gmb-lib/go-platform-kit) for the error taxonomy
the reasons are registered with. Tests use [quicktest](https://github.com/go-quicktest/qt).

## Contributing

See [`CONTRIBUTING.md`](./CONTRIBUTING.md). Report a security problem privately, as
[`SECURITY.md`](./SECURITY.md) describes.

## License

MIT — see [`LICENSE`](./LICENSE).
