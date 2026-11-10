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
| `GET` | `<root>/config/section` | which section answers here, which sections it refers to, and the audience and scope keys a token for the service carries — **no token, no tenant, no person** |

And the rules behind them:

- **A section is the owning API's own payload**, named `<section>-config/1`. There is no second format "for
  export".
- **The version is derived from the section's bytes**: a SHA-256 over the section, salted with its name and its
  tenant. A token computed from the data cannot disagree with the data, whoever wrote it and however. It is
  opaque — compared for equality, never ordered.
- **A preview writes nothing**, and reports what an apply of the same document would do: each item `added`,
  `changed` (naming the members), `unchanged`, `refused` (naming the reason and saying why) or `skipped` (an item
  the owner never takes from any document, naming the reason and saying why; it holds nothing else back), by key.
- **An apply names the version its preview was read against.** None named: `428`. Moved since: `412`. Nothing is
  written in either case.
- **One refused item and nothing is written.** A changed kind, type, unit or format on an existing key is refused
  (`config_key_conflict`, `409` when raised): a new meaning is a new key.
- **A document never removes an item.** An item another service may name is retired — it stays in the section,
  valid for everything that already names it — because no check across two services can be atomic with a removal.
- **Reads are every member's**, because the section is the vocabulary everybody works in and holds nobody's
  records. **Writes are the administrator's.**
- **Every transport is recorded by its owner, in its own history.** A read sent with `Config-Purpose: export` is
  answered exactly as the read is, and only once the owner has recorded who took its part out and the part's hash;
  only a caller who may apply the section may make it. An apply records the part's hash and the document's content
  hash, which the caller names in `Config-Document`. A part's hash is taken the way the document's content hash
  takes each section, so a line in the history can be matched to the file.

## Packages

| Package | What it is |
|---|---|
| [`contract`](./contract) | The vocabulary both sides speak: the paths, the schema name, the version token, the report and its statuses, the section answer, and the four reasons, registered with their status and public title |
| [`configserver`](./configserver) | The five operations for a service that owns a section: mounted on the service's router, behind its gate, over its own store |
| [`configclient`](./configclient) | Reading another owner's section: the version, the section with its `ETag`, the section answer, and a cache that hands a copy out only against the version its owner just confirmed |
| [`document`](./document) | The configuration document that carries every owner's section in one file: its types, its content hash, parsing an import, and the words an import answers with |
| [`match`](./match) | Judging a document against what an owner holds, item by item and by key, for an owner whose apply is written in Go |
| [`owners`](./owners) | Finding a deployment's owners from their addresses alone (each answers its section answer), and the order an import writes their sections in |
| [`transfer`](./transfer) | Carrying a tenant's whole configuration as one document: the export over every owner, and the import's dry run and apply, whole or not at all, for any service that hosts it |
| [`configtest`](./configtest) | The proof ladder: every promise above, checked against a service's own routes from its own tests |

## Owning a section

The service brings its store — the one place that reads its section, writes a document and records each
transport in its history — and its gate. The store answers the section and its version from one snapshot, compares
an apply's version with the section's own under a per-tenant lock, and records the hashes it is handed on the line
it writes for the apply.

```go
type Store interface {
	ConfigRead(ctx context.Context, tenant string) (configserver.Read, error)
	ConfigVersion(ctx context.Context, tenant string) (string, error)
	ConfigApply(ctx context.Context, a configserver.Apply) (json.RawMessage, error) // a.PartHash, a.Document
	ConfigExported(ctx context.Context, e configserver.Export) error                 // who took the part out, its hash
}
```

```go
// The service's permissions, with this library's contribution: "<service>/setup:import".
var Permissions = permissions.MustNew("orders", "Orders", own, configserver.Permissions())

owner := &configserver.Owner{
	Section:   "orders",           // travels as "orders-config/1"
	Audience:  "svc:orders",       // the audience a token for this service carries
	ScopeKeys: []string{"orders"}, // the keys of every scope its routes check
	Domain:    "orders",           // refusals are "err:orders:…"
	Gate:      Permissions.Gate(deny),
	Read:      permissions.Levels("orders", "read"),
	Write:     configserver.ImportRule(Permissions, permissions.Levels("orders", "admin")),
	Tenant:    tenantOf, // the caller's tenant, or the refusal written
	Actor:     actorOf,  // who a change is attributed to
	Store:     storeOf,  // the store, or "not ready" written
	Applied:   recordConfigChange,
}

v1 := app.Group("/api/v1")
v1.Use(app.AuthMiddleware())
open := app.Group("/api/v1") // the same root, no authentication: only the section answer is mounted here
if err := owner.Mount(v1, open); err != nil {
	return err
}
```

The section answer needs no token because it is asked before the asker holds one: a coordinator given nothing but
the service's address learns from it which audience to ask a token for, and which of a person's scopes to ask it
with. It says nothing about any tenant.

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

## Carrying a whole configuration

A service that offers an administrator the export and the import — an administration service, or an
application's own backend — finds the deployment's owners once at start, and answers each request through
`transfer`:

```go
set, err := owners.Discover(ctx, addresses, owners.HTTPProber(get), time.Minute, time.Second)

t := &transfer.Transfer{Owners: set, Warn: logWarning}
caller := transfer.CallerFunc(func(ctx context.Context, o *owners.Owner, scope, method, path string,
	header http.Header, body []byte) (*configclient.Response, error) {
	// one call to o.Address+path, on the person's behalf, asking for exactly scope
})
person := transfer.Person{Tenant: tenant, Scopes: scopesThePersonHolds}

file, err := t.Export(ctx, person, caller, time.Now())                // file.Name, file.Body, file.Hash
imported, err := t.Import(ctx, person, caller, requestBody, dryRun) // imported.Answer is the answer
if err != nil {
	return transfer.Problem(err, "my-service") // the same code, status and words from every host
}
```

The export asks every owner the person holds anything of for its section, as an export, and fails whole when
one refuses or cannot be reached. The dry run asks each owner to preview its section and answers each one's
version. The apply names those versions: it stops when one moved, writes nothing when one section is refused,
then writes the sections in the owners' order and, when a later one fails, restores the ones already written,
newest first. A write that produced no answer is read back: when its section moved, or cannot be read, the import
is `partial` and the section is not written back over. A section no owner holds, or whose owner the person holds
nothing of, is reported skipped. A section's line carries only the public part of an owner's refusal or failure,
since the answer is a 200 that the host's own error projection never sees. Every owner judges every call with the
person's own scopes; the transfer adds no rule of its own, and never sees a session or a token.

## Proving it

```go
func TestTheConfigurationContract(t *testing.T) {
	configtest.Gates{
		Service: configtest.Service{Client: client, Root: "/api/v1", Section: "orders", Domain: "orders",
			Audience: "svc:orders", ScopeKeys: []string{"orders"}},
		Anyone:     tokenWithoutTenant,
		Readers:    []configtest.Caller{member, readingService},
		Unreadable: []configtest.Caller{anotherServicesMachine},
		Writers:    []configtest.Caller{administrator, administeringService},
		Refused:    []configtest.Caller{member, personHoldingRungs},
	}.Run(t)
}
```

`configtest.Live` walks the rest against a real store: the unchanged document that moves nothing, the service's
own refusals — each reported alike by preview and apply, each writing nothing — a real change, a stale apply,
another tenant's section, and the record of every transport, which it reads through the service's own history
(`Exports` and `Imports`).

## Dependencies

[Azugo](https://azugo.io) for routing, [go-authbyte](https://github.com/gmb-lib/go-authbyte) for permissions,
the service-subject marker and, in `transfer`, telling the token authority's refusal from a failed call,
[go-platform-kit](https://github.com/gmb-lib/go-platform-kit) for the error taxonomy the reasons are registered
with and the problems `transfer` answers. Tests use [quicktest](https://github.com/go-quicktest/qt).

## Contributing

See [`CONTRIBUTING.md`](./CONTRIBUTING.md). Report a security problem privately, as
[`SECURITY.md`](./SECURITY.md) describes.

## License

MIT — see [`LICENSE`](./LICENSE).
