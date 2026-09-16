# Contributing to AtlasCache

## Contributions are not open yet

AtlasCache is pre-alpha and built by a single maintainer. Pull requests are not
being accepted while the foundational releases are put together, because the
architecture is still moving underneath them and a patch written today may not
apply to next week's design.

**What changes that:** once v0.1.0 ships and the v0.2.0 plan is public, the
repository opens to contributions. That is the intent, not a vague hope.

**What is welcome right now:** bug reports, questions, and ideas, through
[issues](https://github.com/b3vet/atlascache/issues). Being told something is
broken or confusing is valuable at this stage, and it costs you nothing that a
rejected pull request would.

If you want to build on the code meanwhile, the licence is MIT — fork it.

## Building and testing

```bash
make tools     # golangci-lint plus the git hooks — run this once
make build     # the server binary, into bin/
make check     # fmt, vet, lint, and unit tests with the race detector
make e2e       # the regression gate (see below)
```

`make help` lists every target.

This repository contains **three Go modules**, tied together by `go.work`:

| Module | Purpose |
|--------|---------|
| `github.com/b3vet/atlascache` | server, engine, tooling |
| `github.com/b3vet/atlascache/pkg/client` | the Go SDK — zero dependencies |
| `github.com/b3vet/atlascache/test/e2e` | the end-to-end suite |

A bare `go test ./...` at the root reaches **only the first one**. Use the
Makefile targets, which cover all three. This catches people out, including the
maintainer.

`pkg/client` has no dependencies and must stay that way, so that importing the
SDK never pulls in the server's dependency tree. `make tidy-check` enforces it.

## The E2E gate

The end-to-end suite is the primary signal of correctness here, more than unit
tests are. It starts real server processes and drives them over a real socket
with an independent RESP implementation — deliberately not the project's own
client, so a protocol bug cannot hide by being symmetrical.

```bash
make e2e-smoke   # ~3s, runs on every commit via a hook
make e2e-full    # every pull request and phase close
make e2e-soak    # nightly and at milestone close
```

Tiers are cumulative: `full` includes everything in `smoke`.

**A failing spec after a previously green run means you caused a regression.**
It does not mean a flaky test to re-run. The suite is built to be deterministic,
and if you find genuine flakiness, that is itself a bug worth reporting — it
undermines the one signal everything else depends on.

Specs are declarative YAML in `test/e2e/specs/`, with a Go escape hatch for the
cases YAML cannot express. Adding a spec is usually the cheapest way to
demonstrate a bug.

## Commits

Commits follow [Conventional Commits](https://www.conventionalcommits.org/), and
a hook installed by `make tools` enforces it — subject line length, a single
scope, and the type prefix. The hook also runs the smoke tier before each commit;
`git config atlascache.pregate false` turns that off if you need it out of the
way.

You will see references like `refs FEAT-0031` or `refs ISSUE-0023` in the
history. **Those will not resolve to anything you can read**, and nothing is
broken: planning material lives in a `dev/` directory that is deliberately not
part of this repository. The commit messages are written to stand on their own
without it — if one does not, that is a fair thing to raise.

## Reporting a bug

Please include the version (`atlascache --version`), your platform, your
configuration with any secrets removed, and the steps to reproduce. The issue
templates ask for exactly this. A report missing the version costs a round trip
every time, which is the difference between a bug fixed today and one fixed next
week.

## Security

Do not open a public issue for a security vulnerability. See
[SECURITY.md](SECURITY.md).
