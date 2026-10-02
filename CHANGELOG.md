# Changelog

Notable changes to this library, newest first. Versions are git tags; this file is written
for whoever bumps the dependency — what changed, and what it means for code that already
uses it.

## v0.2.0

Every owner now records each transport of its own section in its own history, and the section answer is asked with
no token at all, naming what a token for the service carries. **This release changes the API: a service bumping to
it changes its wiring, its store and its contract tests.**

- **The section answer needs no token.** `Owner.Mount(r, open)` takes a second router: the same versioned root
  with no authentication, where only `GET <root>/config/section` is mounted. The other four stay behind `r`'s
  authentication. Mounting with either router missing is an error.
- **The answer names the service's audience and scope keys.** Set `Owner.Audience` (such as `"svc:orders"`) and
  `Owner.ScopeKeys` (the keys of every scope the service's routes check: its ladder's groups and its permissions'
  service key). The answer gains `audience` and `scopeKeys`; `contract.NewSectionAnswer` takes both, and an
  answer missing either fails its check, so a reader using `configclient.Section` refuses an owner still on
  v0.1.0.

  Before: `{"section":"orders","schema":"orders-config/1","refersTo":[]}`
  After: `{"section":"orders","schema":"orders-config/1","refersTo":[],"audience":"svc:orders","scopeKeys":["orders"]}`
- **An export is recorded.** A section read sent with `Config-Purpose: export` is answered exactly as the read is,
  once the store's new `ConfigExported` has recorded who took the part out and its hash. Only a caller who passes
  the write rule may make it (`403` otherwise); a purpose other than `export` is `400`; a failed record fails
  the read. A plain read records nothing.
- **An apply carries what it records.** `Apply` gains `PartHash` (the part as it arrived) and `Document` (the
  content hash the caller names in `Config-Document`, or empty). A `Config-Document` that is not a sha256 hash is
  refused as `config_bad_document`, before the store is asked.
- **`contract.PartHash` and `contract.ValidHash`.** A part's hash is taken over the section as it travels —
  compacted and escaped as an encoder writes it — which is the form each section takes inside a document's content
  hash, so a part read from a file hashes to the value its owner recorded.
- **`configtest`.** `Service` gains `Audience` and `ScopeKeys`. `Gates` checks the section answer with no
  token, and that only a writer may read the section as an export. `Live` requires `Exports` and `Imports`, read
  from the service's own history, and checks one line for each export and for each apply that was not refused,
  none for a plain read, a refused document or a refused version.

## v0.1.0

Initial code: the tenant configuration contract for a service that owns a section, and for
anything that reads one.

- **`contract`** — the five operations' paths, the `<section>-config/1` schema name, the version
  token derived from a section's bytes, the item report (`added` · `changed` · `unchanged` ·
  `refused`), the tenant-free section answer, and the four reasons (`config_bad_document` 422 ·
  `config_key_conflict` 409 · `config_version_required` 428 · `config_version_moved` 412),
  registered with the error taxonomy when the package loads.
- **`configserver`** — mounts the five operations on a service's versioned API root, behind its
  permissions gate: members read, the write rule's holders preview and apply, an apply naming no
  version is refused before the store is asked. Contributes the `setup:import` permission to the
  service's list.
- **`configclient`** — reads another owner's version, section and section answer on the caller's
  own authority, with a cache that hands a copy out only against the version its owner just
  confirmed.
- **`document`** — the configuration document (`gmbConfig` 1.0): its types, its content hash, the
  checks an import makes before any owner is asked, and the words an import answers with.
- **`match`** — judging a document's items against what an owner holds, by key, for an owner whose
  apply is written in Go.
- **`configtest`** — the proof ladder a service runs from its own tests: the gates with no store
  behind them, and the whole contract against a real one.
