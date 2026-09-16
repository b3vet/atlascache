# Releasing

How a version of AtlasCache is cut. This is a maintainer document; if you are
trying to *use* a release, see [deployment.md](deployment.md).

A release is the one piece of automation in this repository with no undo. A
pushed tag, a published image and a signature cannot be quietly retracted once
anyone has fetched them, so the order below is not a suggestion: everything is
rehearsed before anything is published.

- [Two tags, because there are three modules](#two-tags-because-there-are-three-modules)
- [What a release produces](#what-a-release-produces)
- [The runbook](#the-runbook)
- [The dry run](#the-dry-run)
- [Reproducibility](#reproducibility)
- [If something goes wrong](#if-something-goes-wrong)

## Two tags, because there are three modules

This repository holds three Go modules:

| Module | Path | Published? |
|---|---|---|
| Server and CLI | `github.com/b3vet/atlascache` | yes, as binaries and an image |
| Go SDK | `github.com/b3vet/atlascache/pkg/client` | yes, as an import path |
| E2E suite | `github.com/b3vet/atlascache/test/e2e` | never |

Go versions a module living in a subdirectory by a tag that carries the
subdirectory as a prefix. `v0.1.0` versions the **root** module and nothing
else. With only that tag:

```
go get github.com/b3vet/atlascache/pkg/client@v0.1.0
  -> invalid version: unknown revision pkg/client/v0.1.0
```

because the tag the resolver looks for is `pkg/client/v0.1.0`. This is not a
choice; it is how the module resolver finds a subdirectory module, and no
amount of configuration changes it.

**So a release is two tags on one commit:**

```
pkg/client/v0.1.0   the SDK
v0.1.0              the server and CLI -- this one triggers the pipeline
```

The release workflow triggers on `v*`, which does not match `pkg/client/v0.1.0`
(a `*` in a GitHub ref filter does not cross `/`), and `.goreleaser.yml` sets
`git.ignore_tags: ["pkg/client/*"]` so an SDK tag on the release commit cannot
be mistaken for the version being released. The SDK tag is pushed by hand,
deliberately: it needs no build and it must not be able to start a release.

`test/e2e` is never tagged. It is a test harness that happens to be a module,
its `go.mod` carries `replace` directives to the other two, and tagging it
would publish an import path nobody should import.

### The ordering problem this creates

The root `go.mod` currently says:

```
require github.com/b3vet/atlascache/pkg/client v0.0.0
replace github.com/b3vet/atlascache/pkg/client => ./pkg/client
```

That `replace` is what makes the tree build today. It is also invisible to
consumers — `replace` applies only to the main module — so with it in place,
`go install github.com/b3vet/atlascache/cmd/atlasctl@v0.1.0` tries to resolve
`pkg/client@v0.0.0` from the proxy and fails. The binaries in the release are
unaffected (they are built from a checkout, where the replace applies), but the
`go install` path and `go get github.com/b3vet/atlascache@v0.1.0` are not.

Fixing it means requiring a real SDK version, which cannot exist before the SDK
tag is pushed and the repository is public. That is the reason for the order in
the runbook below, and it is why the SDK tag goes first.

## What a release produces

| Artifact | Detail |
|---|---|
| Binaries | `atlascache` and `atlasctl`, linux/darwin/windows × amd64/arm64 |
| Archives | one `.tar.gz` (`.zip` on Windows) per platform, holding both binaries plus LICENSE, README, CHANGELOG, `config.example.yaml` and `docs/` |
| Checksums | `checksums.txt`, SHA256 over everything |
| SBOMs | syft, SPDX JSON, one per archive and one attached to each image |
| Images | `ghcr.io/b3vet/atlascache:X.Y.Z` and `:X.Y.Z-debug`, multi-arch |
| Signature | cosign keyless over `checksums.txt`, and over each image |
| Release | GitHub, as a **draft pre-release** |

Every `v0.x` release is marked pre-release. The version number alone does not
stop package tooling — or a person in a hurry — from treating `v0.1.0` as
production-ready.

## The runbook

**1. Land everything.** `main` green, every feature for the milestone done,
`make phase-check PHASE=Px` passing.

**2. Draft the changelog, then edit it.**

```sh
make changelog-draft          # git-cliff, to stdout
```

The draft removes the grunt work; the edit is the point. `CHANGELOG.md` should
read as release notes a user would want to read, not as a dump of commit
subjects.

**3. Set `VERSION`.**

```sh
echo 0.1.0 > VERSION
```

The bump is a deliberate act and stays manual. Commit it with the changelog.

**4. Rehearse locally.**

```sh
make release-dry-run
```

Every artifact lands in `dist/` and nothing is pushed. Look at it — see [the
dry run](#the-dry-run) below for what to check.

**5. Rehearse in CI.** Run the **Release** workflow manually from the Actions
tab. A manual run is always a dry run: it builds and signs everything and
publishes nothing. This is the only place keyless signing can be exercised,
because keyless signing *is* CI's identity, and it is the last check before the
tag exists.

> Note: a keyless signature — even a rehearsal one — is recorded in Sigstore's
> public transparency log. Nothing secret is disclosed (the entry is a hash and
> a certificate), but the entry is permanent.

**6. Tag the SDK first.**

```sh
git tag pkg/client/v0.1.0
git push origin pkg/client/v0.1.0
```

It triggers nothing. It makes `go get .../pkg/client@v0.1.0` resolvable, which
is the precondition for the root module to be able to require a real version.

**7. Tag the release.**

```sh
make release-check TAG=v0.1.0     # tag matches VERSION, before anything is built
git tag v0.1.0
git push origin v0.1.0
```

The workflow runs the same check first thing, then the tests, the cross-compile
check and the full E2E tier, and only then builds. Tagging `v0.1.0` while
`VERSION` says `0.1.0-dev` would produce binaries that report the wrong version
forever: the artifacts are immutable and the tag cannot be moved once anyone has
fetched it.

**8. Publish the draft.** Read the notes once more, check the artifact list,
then publish. The release is created as a draft precisely so this step exists.

**9. Verify what you published.**

```sh
cosign verify ghcr.io/b3vet/atlascache:0.1.0 \
  --certificate-identity-regexp '^https://github.com/b3vet/atlascache/\.github/workflows/release\.yml@refs/tags/v0.1.0$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

docker run --rm -p 6379:6379 ghcr.io/b3vet/atlascache:0.1.0
```

## The dry run

```sh
make release-dry-run
```

`dist/` should hold, for `X` the snapshot version:

```
atlascache_X_linux_amd64.tar.gz     + .sbom.json
atlascache_X_linux_arm64.tar.gz     + .sbom.json
atlascache_X_darwin_amd64.tar.gz    + .sbom.json
atlascache_X_darwin_arm64.tar.gz    + .sbom.json
atlascache_X_windows_amd64.zip      + .sbom.json
atlascache_X_windows_arm64.zip      + .sbom.json
checksums.txt
checksums.txt.sig
checksums.txt.pem
```

plus per-architecture images tagged `ghcr.io/b3vet/atlascache:X-<arch>` and
`:X-debug-<arch>` in the local image store, and the raw binaries under
`dist/atlascache_*/` and `dist/atlasctl_*/`.

Worth actually checking, rather than counting files:

```sh
(cd dist && shasum -a 256 -c checksums.txt)          # the checksums are right
tar tzf dist/atlascache_*_linux_amd64.tar.gz         # both binaries and the docs
file dist/atlascache_*/atlascache                    # six distinct platforms
```

**The signatures in a local dry run are made with a throwaway key.** Keyless
signing needs an OIDC identity and there is none off CI, so
`scripts/cosign-sign.sh` generates a key pair, signs, deletes the key and says
so. The files are real signatures over real artifacts and they prove exactly
nothing about provenance — they prove the pipeline produces them. The public
half is written to `dist/checksums.txt.dryrun.pub` so the dry run can verify
its own output:

```sh
cosign verify-blob dist/checksums.txt \
  --key dist/checksums.txt.dryrun.pub \
  --signature dist/checksums.txt.sig \
  --insecure-ignore-tlog=true
```

Never publish a dry-run signature. Step 5 of the runbook is where keyless is
exercised for real.

## Reproducibility

A given tag produces byte-identical binaries. Two things make that true:

- `-trimpath`, so no path from the build machine reaches the binary;
- `mod_timestamp` and the version stamp both taken from the **commit**
  (`.CommitTimestamp`, `.CommitDate`) rather than from the wall clock.

`GOWORK=off` is set for every release build. Inside the workspace, `pkg/client`
resolves from the tree; off it, the root module builds exactly as `go.mod`
describes — which is what a consumer gets, and what the checksums and the
signature therefore attest to.

This is what makes a signature worth having. Without reproducibility a
signature attests only that the pipeline ran, not what it built. To check:

```sh
make release-dry-run && shasum -a 256 dist/atlascache_*/atlascache* > /tmp/a
make release-dry-run && shasum -a 256 dist/atlascache_*/atlascache* > /tmp/b
diff /tmp/a /tmp/b
```

## If something goes wrong

**Before the tag is pushed:** nothing has happened. Fix and re-run the dry run.

**After the tag, before the draft is published:** delete the draft release,
delete the tag locally and remotely, fix, and tag again. Nothing was fetchable,
so nothing was fetched — unless the images were pushed, which happens before
the draft is created. Treat a pushed image tag as permanent and use the next
patch version.

**After the release is published:** do not delete it and do not move the tag.
Someone has it, the checksums are in a file somebody may have saved, and the
signature covers a specific artifact. Ship `vX.Y.Z+1` and say what was wrong in
its notes. The only exception is a leaked credential, where the artifact comes
down regardless of the confusion it causes and the credential is rotated first.
