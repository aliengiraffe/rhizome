#!/usr/bin/env bash
#
# build-macos-pkg.sh — wrap an already signed rhizome binary in a
# distribution-format installer that places it at /usr/local/bin/rhizome,
# then sign, notarize and staple the package.
#
# The package has no scripts: installing or upgrading never creates or
# overwrites ~/.rhizome configuration, tokens or runtime state, and never
# installs or starts the observer. Users run `rhizome setup` and
# `rhizome service install` themselves.
#
# Inputs (env):
#   RHIZOME_BINARY            (required) signed universal binary to package
#   RHIZOME_PKG_VERSION       (required) numeric MAJOR.MINOR.PATCH receipt version
#   RHIZOME_PKG_OUT           (required) output .pkg path
#   RHIZOME_SIGNING           required | adhoc (default); see lib.sh
#   APPLE_INSTALLER_IDENTITY  "Developer ID Installer: … (TEAMID)" or SHA-1;
#                             discovered from RHIZOME_SIGN_KEYCHAIN when unset
#   APPLE_NOTARY_KEY_P8, APPLE_NOTARY_KEY_ID, APPLE_NOTARY_ISSUER_ID
set -euo pipefail

PKG_IDENTIFIER=com.aliengiraffe.rhizome
COMPONENT_PKG=rhizome-component.pkg
HERE=$(cd "$(dirname "$0")" && pwd)

# shellcheck source=lib.sh
. "$HERE/lib.sh"

: "${RHIZOME_BINARY:?RHIZOME_BINARY is required}"
: "${RHIZOME_PKG_VERSION:?RHIZOME_PKG_VERSION is required}"
: "${RHIZOME_PKG_OUT:?RHIZOME_PKG_OUT is required}"
[[ "$RHIZOME_PKG_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "::error::RHIZOME_PKG_VERSION '$RHIZOME_PKG_VERSION' must be numeric MAJOR.MINOR.PATCH" >&2; exit 1; }
[ -f "$RHIZOME_BINARY" ] || { echo "ERROR: $RHIZOME_BINARY does not exist" >&2; exit 1; }
[ "$(uname -s)" = Darwin ] || { echo "::error::building a macOS package requires macOS" >&2; exit 1; }

IDENTITY="${APPLE_INSTALLER_IDENTITY:-}"
[ -n "$IDENTITY" ] || IDENTITY=$(discover_identity "Developer ID Installer:" basic)
if signing_required; then
  [ -n "$IDENTITY" ] || {
    echo "::error::RHIZOME_SIGNING=required but no Developer ID Installer identity was given or found in the keychain" >&2; exit 1; }
  require_notary_credentials
  check_release_signature "$RHIZOME_BINARY"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/payload/usr/local/bin" "$work/component" "$(dirname "$RHIZOME_PKG_OUT")"
chmod 0755 "$work/payload" "$work/payload/usr" "$work/payload/usr/local" "$work/payload/usr/local/bin"
# Copy the signed bytes unchanged; verify-pkg-payload.sh compares digests.
cp -p "$RHIZOME_BINARY" "$work/payload/usr/local/bin/rhizome"
chmod 0755 "$work/payload/usr/local/bin/rhizome"

echo "==> pkgbuild (identifier=$PKG_IDENTIFIER version=$RHIZOME_PKG_VERSION, no scripts)"
pkgbuild --root "$work/payload" --identifier "$PKG_IDENTIFIER" \
  --version "$RHIZOME_PKG_VERSION" --install-location / --ownership recommended \
  "$work/component/$COMPONENT_PKG"

sed -e "s|@PKG_IDENTIFIER@|$PKG_IDENTIFIER|g" -e "s|@PKG_VERSION@|$RHIZOME_PKG_VERSION|g" \
  -e "s|@COMPONENT_PKG@|$COMPONENT_PKG|g" "$HERE/macos/distribution.xml" >"$work/distribution.xml"
echo "==> productbuild (distribution format)"
productbuild --distribution "$work/distribution.xml" --package-path "$work/component" "$work/unsigned.pkg"

rm -f "$RHIZOME_PKG_OUT"
if [ -n "$IDENTITY" ]; then
  KEYCHAIN_ARGS=()
  [ -n "${RHIZOME_SIGN_KEYCHAIN:-}" ] && KEYCHAIN_ARGS=(--keychain "$RHIZOME_SIGN_KEYCHAIN")
  echo "==> productsign (Developer ID Installer)"
  productsign --sign "$IDENTITY" --timestamp ${KEYCHAIN_ARGS[@]+"${KEYCHAIN_ARGS[@]}"} "$work/unsigned.pkg" "$RHIZOME_PKG_OUT"
  sig=$(pkgutil --check-signature "$RHIZOME_PKG_OUT")
  echo "$sig"
  grep -q 'Developer ID Installer:' <<<"$sig" || {
    echo "::error::$RHIZOME_PKG_OUT is not signed with a Developer ID Installer certificate" >&2; exit 1; }
else
  echo "==> no installer identity: emitting an UNSIGNED package (not shippable)"
  cp "$work/unsigned.pkg" "$RHIZOME_PKG_OUT"
fi

if signing_required; then
  notarize_submission "$RHIZOME_PKG_OUT" "$work"
  echo "==> staple + validate"
  xcrun stapler staple "$RHIZOME_PKG_OUT"
  xcrun stapler validate "$RHIZOME_PKG_OUT"
  assess=$(spctl --assess --type install --verbose=4 "$RHIZOME_PKG_OUT" 2>&1) || {
    echo "::error::Gatekeeper rejected $RHIZOME_PKG_OUT: $assess" >&2; exit 1; }
  echo "$assess"
  grep -q 'source=Notarized Developer ID' <<<"$assess" || {
    echo "::error::Gatekeeper does not see $RHIZOME_PKG_OUT as notarized: $assess" >&2; exit 1; }
fi

if ! xar -tf "$RHIZOME_PKG_OUT" | grep -qx Distribution; then
  echo "::error::$RHIZOME_PKG_OUT is not a distribution-format package" >&2
  exit 1
fi
echo "==> built $RHIZOME_PKG_OUT"
