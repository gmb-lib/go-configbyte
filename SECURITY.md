# Security policy

This library decides who may read a tenant's configuration and who may change it, and whether a
configuration document is written at all. A service that owns a section answers its reads, previews
and applies through it; anything that reads another service's section — a sign-in read, an export —
keeps its copies through it. A wrong answer here is not a bug in one endpoint: it is a tenant's
setup changed by somebody who may not change it, or one organisation handed another's.

Please report security problems privately. Do not open a public issue, pull request or discussion
for anything that could be exploited before a fix exists.

## How to report

Use **[private vulnerability reporting](https://github.com/gmb-lib/go-configbyte/security/advisories/new)**
on this repository. The report stays visible only to you and the maintainers until an advisory is
published, and it gives us one place to discuss and co-ordinate a fix with you.

Please include, as far as you can establish it:

- what the problem is, and what an attacker gains from it;
- the smallest set of steps that reproduces it, and against which version or commit;
- the configuration it needs, if it only appears under particular settings;
- whether you have told anyone else, and whether a disclosure date already binds you.

Please do not send us live tokens or a real tenant's configuration. The shape of either is enough to
explain almost any finding here.

## What happens next

- We acknowledge a report within **five working days**.
- We tell you whether we can reproduce it, and what we think its severity is, as soon as we know.
- We keep you updated while a fix is prepared, and we agree a disclosure date with you. Our default
  is to publish an advisory once a fix is available, and in any case within **90 days** of the
  report — earlier if the problem is already public or being exploited.
- We credit you in the advisory unless you would rather stay anonymous.

There is no bug-bounty programme. We are grateful anyway, and we say so publicly.

## What we consider most serious

The unacceptable failure modes are a write by somebody who may not write, and one organisation's
configuration reaching another. The classes that matter most:

- A preview or an apply admitted for a caller the write rule does not admit — a person passing by a
  ladder rung, one of several required permissions accepted as all of them, or a service acting as
  itself passing with another service's scopes.
- A read admitted for a service acting as itself that holds nothing of the owning service.
- An apply written that names no version, or a version the section has moved past, or a document
  with a refused item partly written.
- A section copy handed to a caller of another organisation, or under a version its owner did not
  stamp on those bytes.
- A version token that does not move when the section moves, or that two organisations share.
- A document that removes an item, or a report that says nothing was written when something was.

Denial of service and findings that need an already-compromised host or an already-authorised
administrator are in scope but lower priority. Reports about outdated dependencies are welcome
where you can show the vulnerable path is actually reachable.

## Scope

This policy covers the code in this repository. It does not cover the services that use it — their
stores, their own rules on what a section may hold, or how they authenticate a caller — nor the
authorization server that issues the tokens they read; report those to the parties that operate
them. How a service configures this library (its gate, its write rule) is that service's
responsibility; a report that a *default* is unsafe is very much in scope.

## Supported versions

Security fixes land on the most recent release. Older tags are not patched; if you are pinned to
one, the fix is to move forward.
