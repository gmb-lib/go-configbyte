# Changelog

Notable changes to this library, newest first. Versions are git tags; this file is written
for whoever bumps the dependency — what changed, and what it means for code that already
uses it.

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
