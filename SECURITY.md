# Security Policy

## Supported versions

AtlasCache is pre-alpha. No version is supported for production use, and there
is no backporting: fixes land on `main` and ship in the next release.

| Version | Supported |
|---------|-----------|
| v0.1.x | Yes, on `main` |
| Anything earlier | No — nothing earlier was released |

## Reporting a vulnerability

**Please do not open a public issue.**

Report privately through GitHub's
[security advisory form](https://github.com/b3vet/atlascache/security/advisories/new),
which is the preferred route because it keeps the report, the discussion, and
the eventual advisory in one place.

Please include what you would want if you were receiving it: the version and
platform, what an attacker gains, and the steps to reproduce. A proof of concept
is welcome but not required — a clear description of the mechanism is enough to
act on.

**What to expect:** an acknowledgement within a week, and an honest assessment
rather than a holding message. This is a single-maintainer project, so that is a
realistic commitment rather than an ambitious one. If a fix will take time, you
will be told that instead of hearing nothing.

You will be credited in the advisory unless you would rather not be.

## What is in scope

AtlasCache ships with **TLS and authentication disabled by default**, and warns
loudly at startup when they are off. That default is deliberate — it makes the
software work on first run — and a report that an unconfigured server accepts
unauthenticated connections is describing the documented behaviour, not a
vulnerability.

What is in scope:

- Any way to read or modify data that the configuration should have prevented
- Authentication bypass when authentication is enabled
- A crash, hang, or unbounded memory growth triggered by network input
- TLS misuse: certificate validation gaps, downgrade, or weak defaults
- Information disclosure to an unauthenticated client
- Anything reachable through the admin API beyond what its configuration allows

The admin API binds to loopback by default for the same reason a database does.
Exposing it deliberately and finding it powerful is expected; finding it
reachable when it should not be is a bug.

## Known posture, stated plainly

Some things are not vulnerabilities but are worth knowing before you deploy
anything:

- **No persistence.** All data is in memory and is lost on restart. Durability
  arrives in v0.2.0.
- **No encryption at rest**, which follows from having no rest.
- **Authentication is a single shared token**, not per-user credentials.
- **The admin token is separate from the data token** on purpose: a client
  holding a data token must not thereby gain administrative access.

Deploy it with TLS on, authentication on, the admin port unreachable from
outside the host, and a memory limit set. `docs/configuration.md` shows that
configuration in full.
