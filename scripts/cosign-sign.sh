#!/usr/bin/env bash
# Sign one release artifact with cosign (FEAT-0033, ADR-0024).
#
#   cosign-sign.sh <artifact> <signature-out> <certificate-out>
#
# Called by GoReleaser's `signs` block, never by hand.
#
# There are two modes, and the difference between them matters:
#
#   keyless   In CI. GitHub's OIDC token is the identity; Sigstore issues a
#             short-lived certificate naming the workflow, the repository and
#             the ref that asked for it, and records the signature in the
#             public transparency log. There is no private key to manage,
#             which is the usual reason signing gets skipped or done badly.
#
#   throwaway Off CI. There is no OIDC identity to borrow, and the alternative
#             to this mode is a dry-run that cannot exercise the signing step
#             at all -- which would leave `make release-dry-run` unable to
#             answer the one question it exists for: does the pipeline produce
#             every artifact? So it generates a key pair, signs with it,
#             throws the key away, and says so. The files are real signatures
#             over real artifacts and they prove exactly nothing about
#             provenance. Never publish them.
#
# The mode is chosen by whether an OIDC token is actually available, not by a
# flag someone could pass by mistake.
set -euo pipefail

artifact="${1:?artifact path is required}"
signature="${2:?signature output path is required}"
certificate="${3:?certificate output path is required}"

if ! command -v cosign >/dev/null 2>&1; then
    echo "cosign-sign: cosign is not installed." >&2
    echo "  macOS:  brew install cosign" >&2
    echo "  other:  https://docs.sigstore.dev/system_config/installation/" >&2
    exit 1
fi

# ACTIONS_ID_TOKEN_REQUEST_URL is set only when a workflow has been granted
# `id-token: write`. Testing for it rather than for GITHUB_ACTIONS means a CI
# run that forgot the permission fails loudly here instead of silently
# producing a throwaway signature and publishing it.
if [ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ]; then
    echo "cosign-sign: keyless signing $artifact as the GitHub workflow identity"
    # --use-signing-config=false keeps cosign 3 on the detached signature and
    # certificate pair that `cosign verify-blob --signature --certificate`
    # reads, which is the form the release notes tell people to use. The
    # alternative is a single --bundle file, and switching to it is a change to
    # published verification instructions rather than a flag tidy-up.
    cosign sign-blob \
        --yes \
        --use-signing-config=false \
        --output-signature "$signature" \
        --output-certificate "$certificate" \
        "$artifact"
    exit 0
fi

if [ "${GITHUB_ACTIONS:-}" = "true" ]; then
    echo "cosign-sign: running in GitHub Actions without an OIDC token." >&2
    echo "  The release job needs 'permissions: id-token: write'." >&2
    echo "  Refusing to fall back to a throwaway key: a published signature" >&2
    echo "  that attests to nothing is worse than an unsigned release." >&2
    exit 1
fi

echo "cosign-sign: DRY RUN -- no OIDC identity, signing $artifact with a throwaway key."
echo "cosign-sign: these signatures prove the pipeline works and nothing else. Do not publish them."

keydir="$(mktemp -d)"
trap 'rm -rf "$keydir"' EXIT

# An empty password: the key lives for the next two commands and is deleted by
# the trap above. A password would be a secret that has to go somewhere.
COSIGN_PASSWORD="" cosign generate-key-pair --output-key-prefix "$keydir/dryrun" >/dev/null

# --tlog-upload=false is the important one. A dry run must not write anything
# to Sigstore's public transparency log: the entry would be permanent, and it
# would record a signature that attests to nothing.
COSIGN_PASSWORD="" cosign sign-blob \
    --yes \
    --key "$keydir/dryrun.key" \
    --use-signing-config=false \
    --tlog-upload=false \
    --new-bundle-format=false \
    --output-signature "$signature" \
    "$artifact" 2>&1 | grep -v '^Flag .* has been deprecated' || true

test -s "$signature" || { echo "cosign-sign: no signature was written" >&2; exit 1; }

# Keyless produces a certificate; a throwaway key does not. GoReleaser expects
# the file to exist, and an empty one would look like a real certificate that
# failed to parse, so it says what it is instead.
cat > "$certificate" <<'CERT'
This is not a certificate.

It stands in for the Sigstore certificate that keyless signing produces, so
that a dry run creates the same set of files as a real release. The signature
beside it was made with a throwaway key that no longer exists and attests to
nothing. A real release signs in CI with GitHub's OIDC identity, and this file
is then a short-lived X.509 certificate naming the workflow that signed.
CERT

# The public half, so a dry run can actually verify what it just signed rather
# than assuming it worked.
cp "$keydir/dryrun.pub" "${signature%.sig}.dryrun.pub"
