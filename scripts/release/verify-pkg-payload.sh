#!/usr/bin/env bash
#
# verify-pkg-payload.sh — expand a rhizome .pkg WITHOUT installing it, prove
# its payload is exactly the expected signed binary at usr/local/bin/rhizome
# with no installer scripts, and execute that payload.
#
# Usage: verify-pkg-payload.sh <pkg> <expected-binary>
#
# expected-binary is the binary extracted from the published darwin archive,
# so the package and the archive are proven to ship identical bytes.
# RHIZOME_SIGNING=required additionally demands the release signature;
# RHIZOME_EXPECT_VERSION pins the reported version.
set -euo pipefail

PKG="${1:?usage: verify-pkg-payload.sh <pkg> <expected-binary>}"
EXPECTED="${2:?usage: verify-pkg-payload.sh <pkg> <expected-binary>}"
HERE=$(cd "$(dirname "$0")" && pwd)

# shellcheck source=lib.sh
. "$HERE/lib.sh"

[ -f "$PKG" ] && [ -f "$EXPECTED" ] || { echo "ERROR: $PKG or $EXPECTED does not exist" >&2; exit 1; }
[ "$(uname -s)" = Darwin ] || { echo "::error::verifying a macOS package requires macOS" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
echo "==> pkgutil --expand-full (no install, no root): $PKG"
pkgutil --expand-full "$PKG" "$work/x"

failed=0
xar -tf "$PKG" | grep -qx Distribution || { echo "::error::not a distribution-format package" >&2; failed=1; }

components=$(find "$work/x" -maxdepth 1 -name '*.pkg' | wc -l | tr -d ' ')
[ "$components" = 1 ] || { echo "::error::expected one component package, found $components" >&2; failed=1; }
comp="$work/x/rhizome-component.pkg"
[ -d "$comp" ] || { echo "::error::rhizome-component.pkg missing" >&2; exit 1; }

if [ -e "$comp/Scripts" ]; then
  echo "::error::package carries installer scripts; it must not run anything at install time" >&2
  failed=1
fi
grep -q 'identifier="com.aliengiraffe.rhizome"' "$comp/PackageInfo" || {
  echo "::error::receipt identifier is not com.aliengiraffe.rhizome" >&2; failed=1; }
grep -q 'install-location="/"' "$comp/PackageInfo" || {
  echo "::error::install location is not /" >&2; failed=1; }

files=$(cd "$comp/Payload" && find . -type f -o -type l)
if [ "$files" != "./usr/local/bin/rhizome" ]; then
  echo "::error::payload must contain only usr/local/bin/rhizome; found:" >&2
  echo "$files" >&2
  failed=1
fi
payload="$comp/Payload/usr/local/bin/rhizome"
[ -f "$payload" ] || { echo "::error::usr/local/bin/rhizome missing from payload" >&2; exit 1; }

got=$(shasum -a 256 "$payload" | awk '{print $1}')
want=$(shasum -a 256 "$EXPECTED" | awk '{print $1}')
if [ "$got" != "$want" ]; then
  echo "::error::payload digest $got does not match the archive binary $want" >&2
  failed=1
else
  echo "    payload matches the archive binary: $got"
fi

codesign --verify --strict --verbose=2 "$payload" || { echo "::error::payload failed codesign --verify" >&2; failed=1; }
if signing_required; then
  check_release_signature "$payload" || failed=1
fi

echo "==> EXECUTION GATE: package payload"
run_gate "package payload" "$payload" || failed=1

if [ "$failed" -ne 0 ]; then
  echo "::error::$PKG failed payload verification" >&2
  exit 1
fi
echo "==> $PKG payload verified and executed"
