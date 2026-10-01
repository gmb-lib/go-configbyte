# Contributing

Thank you for considering a contribution. Bug reports, fixes and improvements are welcome. For
anything that could be exploited, use the private route in [SECURITY.md](SECURITY.md) — never a
public issue.

For anything larger than a small fix, please open an issue first and describe what you want to
change and why. It protects your time: a change that fights the library's design is better
redirected before it is written than after.

## Building and testing

You need the Go toolchain at the version named in [go.mod](go.mod). The gate a change must pass is
the same one CI runs:

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
```

Three more checks run in CI and are worth running before you push:

- **Lint** — `golangci-lint run`, at the version pinned in
  [.github/workflows/ci.yml](.github/workflows/ci.yml); the repo's [.golangci.yml](.golangci.yml)
  carries the configuration.
- **Vulnerabilities** — `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`.
- **Fuzz** — a short sweep runs every `Fuzz*` target for 30 seconds. Reading an `If-Match` header
  and parsing an imported document have one each; if you add a parser for input that comes from
  outside, add one.

The committed tree must already be tidy: CI runs `go mod tidy -diff` and fails if it would change
anything, so run `go mod tidy` after touching dependencies. All Go code is `gofmt`-formatted, and
`.gitattributes` pins Go files to LF line endings — leave that alone, it keeps the tidy-diff gate
stable across platforms.

## What a change to this library needs

This library decides who may read and who may write a tenant's configuration, and whether a document
is written at all. Three rules worth knowing before you start:

- **Never widen a gate or loosen a precondition.** If a change lets somebody read or apply who was
  refused before, or lets an apply land that names no version or a stale one, that is the change,
  and it needs to be stated plainly in the pull request.
- **A cache key is a security boundary.** A section copy is kept per organisation and section and
  handed out only against a version its owner just confirmed; anything that changes what goes into
  that key needs a test that another organisation cannot be handed the copy.
- **Nothing is ever removed by a document.** Retiring an item is a change; removing one is not
  something this library expresses.

A change in behaviour comes with a test that **fails when the change is removed** — proving the
guard is what refuses, not something else further down. The proof ladder in `configtest` is a
public promise to the services that run it: a rung may be added, but not weakened.

## Proposing a change

- Work on a branch and open a pull request against `develop`. `develop` is merged into `main` and
  tagged there when a release goes out, so `main` is never committed to directly.
- **Sign off every commit.** This project uses the
  [Developer Certificate of Origin](https://developercertificate.org/): by adding a
  `Signed-off-by: Your Name <you@example.org>` line you certify that you wrote the change or
  otherwise have the right to submit it under this project's licence. `git commit -s` adds the line
  for you; the name and address must match the commit author. A pull request whose commits lack it
  fails the DCO check and cannot be merged.
- Keep the change focused: one concern per pull request.
- Match the style around you — naming, error handling, comment density. Comments explain what and
  why in plain domain terms; a reference to a standard is cited in bracket form, such as
  `[IETF RFC 9110 §13.1.1]`.
- Pull requests also run a dependency review. A new dependency needs a reason the standard library
  or the existing ones cannot cover.

## Releases

A release is a tag on `main`, and for a Go module the tag *is* the publication — the release
workflow re-runs the gate, then checks the tagged version is actually importable from the module
proxy before declaring success. If your change is a breaking one, say so in the pull request: the
version it lands in is decided from that.

## Licence

This project is licensed under the MIT License (see [LICENSE](LICENSE)). By submitting a
contribution you agree that it is provided under the same licence.
