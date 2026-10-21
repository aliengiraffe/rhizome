#!/usr/bin/env bash
#
# lib.sh — shared release helpers, sourced by the scripts in this directory.
# Not executable on its own. Adapted from Lander's notarize-lib.sh and
# sign-macos-binary.sh in aliengiraffe/env-manager.
#
# RHIZOME_SIGNING selects the policy:
#   required  production: Developer ID signatures, notarization and stapling
#             are mandatory; anything missing or weaker fails the build.
#   adhoc     (default) PR and snapshot builds: ad-hoc signing so the
#             execution gate can still run; artifacts are not shippable.

signing_required() { [ "${RHIZOME_SIGNING:-adhoc}" = required ]; }

case "${RHIZOME_SIGNING:-adhoc}" in
  required|adhoc) ;;
  *) echo "::error::RHIZOME_SIGNING must be 'required' or 'adhoc', got '${RHIZOME_SIGNING}'" >&2; exit 2 ;;
esac

# require_notary_credentials fails unless all three notary inputs are present.
require_notary_credentials() {
  local missing=()
  [ -n "${APPLE_NOTARY_KEY_P8:-}" ] && [ -s "${APPLE_NOTARY_KEY_P8}" ] || missing+=(APPLE_NOTARY_KEY_P8)
  [ -n "${APPLE_NOTARY_KEY_ID:-}" ] || missing+=(APPLE_NOTARY_KEY_ID)
  [ -n "${APPLE_NOTARY_ISSUER_ID:-}" ] || missing+=(APPLE_NOTARY_ISSUER_ID)
  if [ "${#missing[@]}" -ne 0 ]; then
    echo "::error::production signing requires notarization credentials; missing: ${missing[*]}. See docs/releasing.md." >&2
    return 1
  fi
}

# discover_identity <pattern> <policy>: prints the SHA-1 of the first valid
# identity in RHIZOME_SIGN_KEYCHAIN whose name starts with pattern.
discover_identity() {
  [ -n "${RHIZOME_SIGN_KEYCHAIN:-}" ] || return 0
  security find-identity -v -p "$2" "$RHIZOME_SIGN_KEYCHAIN" 2>/dev/null \
    | awk -v want="\"$1" 'index($0, want) { print $2; exit }'
}

# check_release_signature <binary>: fails unless the binary carries a
# Developer ID Application signature with hardened runtime and a secure
# timestamp. Ad-hoc and unsigned binaries fail.
check_release_signature() {
  local bin="$1" info
  info=$(codesign --display --verbose=4 "$bin" 2>&1) || {
    echo "::error::$bin is not code signed" >&2
    return 1
  }
  local problems=()
  grep -q '^Signature=adhoc' <<<"$info" && problems+=("ad-hoc signature")
  grep -q '^Authority=Developer ID Application:' <<<"$info" || problems+=("no Developer ID Application authority")
  grep -Eq '^TeamIdentifier=[A-Z0-9]{10}$' <<<"$info" || problems+=("no team identifier")
  grep -Eq '^CodeDirectory .*flags=0x[0-9a-f]+\([^)]*runtime[^)]*\)' <<<"$info" || problems+=("hardened runtime not enabled")
  grep -q '^Timestamp=' <<<"$info" || problems+=("no secure timestamp")
  if [ "${#problems[@]}" -ne 0 ]; then
    local p
    for p in "${problems[@]}"; do echo "::error::$bin: $p" >&2; done
    return 1
  fi
  echo "    release signature OK: $(grep '^Authority=Developer ID' <<<"$info" | head -1)"
}

# run_gate <label> <binary> [prefix...]: executes the binary's --version and
# fails on a nonzero exit. A SIGKILL (137) means the kernel refused to load it.
run_gate() {
  local label="$1" path="$2" out rc
  shift 2
  set +e
  out=$("$@" "$path" --version 2>&1)
  rc=$?
  set -e
  if [ "$rc" -ne 0 ]; then
    echo "::error::EXECUTION GATE FAILED ($label): '$path --version' exited $rc" >&2
    [ "$rc" -eq 137 ] && echo "  exit 137 = SIGKILL: the kernel refused to load the binary despite userland verification." >&2
    echo "  output: ${out:-<none>}" >&2
    return 1
  fi
  if [ -n "${RHIZOME_EXPECT_VERSION:-}" ] && [ "${out#"rhizome $RHIZOME_EXPECT_VERSION (commit "}" = "$out" ]; then
    echo "::error::EXECUTION GATE FAILED ($label): reported '$out', expected version $RHIZOME_EXPECT_VERSION" >&2
    return 1
  fi
  echo "    OK ($label): $out"
}

# notarize_submission <artifact> <scratch_dir>: submits a .zip or .pkg and
# fails unless Apple's verdict is exactly "Accepted". notarytool can exit 0
# with an Invalid verdict, so its exit status alone is never trusted; a
# nonzero exit or unparseable response also fails.
notarize_submission() {
  local artifact="$1" scratch="$2" out rc verdict
  [ -n "$artifact" ] && [ -d "$scratch" ] || { echo "notarize_submission: artifact and scratch dir required" >&2; return 2; }
  require_notary_credentials || return 1
  out="$scratch/notarytool-$(basename "$artifact").json"
  echo "==> notarytool submit $(basename "$artifact")"
  set +e
  xcrun notarytool submit "$artifact" \
    --key "$APPLE_NOTARY_KEY_P8" \
    --key-id "$APPLE_NOTARY_KEY_ID" \
    --issuer "$APPLE_NOTARY_ISSUER_ID" \
    --wait --timeout 45m --output-format json >"$out" 2>"$out.stderr"
  rc=$?
  set -e
  cat "$out.stderr" >&2
  verdict=$(python3 -I -c '
import json, re, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print("malformed\t"); sys.exit()
if not isinstance(d, dict):
    print("malformed\t"); sys.exit()
sid = d.get("id") if isinstance(d.get("id"), str) else ""
if not re.fullmatch(r"[0-9a-fA-F-]{36}", sid):
    sid = ""
st = d.get("status")
print("%s\t%s" % (st if isinstance(st, str) and st else "missing", sid))
' "$out")
  local nstatus="${verdict%%$'\t'*}" id="${verdict#*$'\t'}"
  if [ "$rc" -eq 0 ] && [ "$nstatus" = Accepted ] && [ -n "$id" ]; then
    echo "    notarization Accepted (submission $id)"
    return 0
  fi
  echo "::error::notarization of $(basename "$artifact") failed: notarytool exit $rc, status '$nstatus' (expected Accepted)" >&2
  if [ -n "$id" ]; then
    xcrun notarytool log "$id" --key "$APPLE_NOTARY_KEY_P8" \
      --key-id "$APPLE_NOTARY_KEY_ID" --issuer "$APPLE_NOTARY_ISSUER_ID" >&2 || true
  fi
  return 1
}
