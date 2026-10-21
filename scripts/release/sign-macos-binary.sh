#!/usr/bin/env bash
#
# sign-macos-binary.sh — sign the fused universal rhizome binary with Apple's
# native codesign, verify it, EXECUTE it, and notarize it.
#
# GoReleaser runs this as the universal_binaries post hook (.goreleaser.yaml),
# after the amd64 and arm64 slices are fused and before the archive exists, so
# a nonzero exit means no archive and no release.
#
# Usage: sign-macos-binary.sh <path-to-universal-binary>
#
# Inputs (env):
#   RHIZOME_SIGNING              required | adhoc (default); see lib.sh.
#   APPLE_DEVELOPER_ID_IDENTITY  "Developer ID Application: … (TEAMID)" or its
#                                SHA-1; discovered from RHIZOME_SIGN_KEYCHAIN
#                                when unset.
#   RHIZOME_SIGN_KEYCHAIN        temporary keychain holding the identities.
#   APPLE_NOTARY_KEY_P8, APPLE_NOTARY_KEY_ID, APPLE_NOTARY_ISSUER_ID
#                                App Store Connect API key for notarytool.
#   RHIZOME_EXPECT_VERSION       version the executed binary must report.
set -euo pipefail

BIN="${1:?usage: sign-macos-binary.sh <path-to-binary>}"
[ -f "$BIN" ] || { echo "ERROR: $BIN does not exist" >&2; exit 1; }

# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"

# Production prerequisites are checked before any tool runs so a missing secret
# fails clearly instead of degrading to an ad-hoc artifact.
if signing_required; then
  if [ "$(uname -s)" != Darwin ]; then
    echo "::error::RHIZOME_SIGNING=required needs a macOS runner with codesign and notarytool" >&2
    exit 1
  fi
  require_notary_credentials
fi

if [ "$(uname -s)" != Darwin ]; then
  echo "==> $(uname -s) host: cannot sign or execute darwin binaries; skipping the gate for $BIN"
  exit 0
fi

archs=$(lipo -archs "$BIN")
case " $archs " in
  *" x86_64 "*) ;;
  *) echo "::error::$BIN lacks an x86_64 slice (has: $archs); fuse before signing" >&2; exit 1 ;;
esac
case " $archs " in
  *" arm64 "*) ;;
  *) echo "::error::$BIN lacks an arm64 slice (has: $archs); fuse before signing" >&2; exit 1 ;;
esac

KEYCHAIN_ARGS=()
[ -n "${RHIZOME_SIGN_KEYCHAIN:-}" ] && KEYCHAIN_ARGS=(--keychain "$RHIZOME_SIGN_KEYCHAIN")

IDENTITY="${APPLE_DEVELOPER_ID_IDENTITY:-}"
[ -n "$IDENTITY" ] || IDENTITY=$(discover_identity "Developer ID Application:" codesigning)

if [ -n "$IDENTITY" ]; then
  echo "==> codesign (Developer ID, hardened runtime, secure timestamp): $BIN"
  codesign --force --sign "$IDENTITY" --identifier com.aliengiraffe.rhizome \
    --options runtime --timestamp ${KEYCHAIN_ARGS[@]+"${KEYCHAIN_ARGS[@]}"} --verbose=2 "$BIN"
elif signing_required; then
  echo "::error::RHIZOME_SIGNING=required but no Developer ID Application identity was given or found in the keychain" >&2
  exit 1
else
  echo "==> no Developer ID identity: signing AD-HOC. This artifact is NOT shippable;"
  echo "    ad-hoc signing only lets the execution gate run on Apple Silicon."
  codesign --force --sign - --identifier com.aliengiraffe.rhizome --options runtime "$BIN"
fi

verify_failed=0
echo "==> codesign --verify --strict (signature + designated requirement)"
codesign --verify --strict --verbose=2 "$BIN" || verify_failed=1
for arch in $archs; do
  if codesign --verify --strict --arch "$arch" "$BIN"; then
    echo "    slice OK: $arch"
  else
    echo "::error::slice $arch failed codesign --verify --strict" >&2
    verify_failed=1
  fi
done
if signing_required; then
  check_release_signature "$BIN" || verify_failed=1
fi

echo "==> EXECUTION GATE"
gate_failed=0
run_gate "universal binary on $(uname -m)" "$BIN" || gate_failed=1

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
for arch in $archs; do
  [ "$arch" = "$(uname -m)" ] && continue
  if [ "$arch" != x86_64 ] || ! arch -x86_64 /usr/bin/true >/dev/null 2>&1; then
    echo "    NOT EXECUTED: $arch slice signed and verified, but this $(uname -m) host cannot run it."
    continue
  fi
  lipo "$BIN" -thin "$arch" -output "$scratch/rhizome-$arch"
  run_gate "$arch slice" "$scratch/rhizome-$arch" arch "-$arch" || gate_failed=1
done

if [ "$verify_failed" -ne 0 ] || [ "$gate_failed" -ne 0 ]; then
  echo "::error::signing gate failed for $BIN (verify=$verify_failed exec=$gate_failed)" >&2
  exit 1
fi

if signing_required; then
  # notarytool takes .zip/.pkg/.dmg; a bare Mach-O cannot hold a stapled
  # ticket, so Gatekeeper checks its notarization online.
  zip_path="$scratch/rhizome-notarize.zip"
  /usr/bin/ditto -c -k --keepParent "$BIN" "$zip_path"
  notarize_submission "$zip_path" "$scratch"
  echo "==> EXECUTION GATE (post-notarization)"
  run_gate "notarized universal binary" "$BIN"
else
  echo "==> RHIZOME_SIGNING=adhoc: notarization skipped"
fi

echo "==> $BIN signed, verified and executed"
