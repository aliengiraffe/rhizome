package release

// trustedKeys lists the Ed25519 public keys (lowercase hex, 32 bytes) whose
// release-manifest signatures this build accepts. The auto-updater and
// `releasetool verify -signed` trust exactly this list; the matching private
// seeds exist only as the RHIZOME_RELEASE_SIGNING_KEY secret of the GitHub
// `release` environment. Rotation: add the new public key here, ship a release
// signed by the old key so installed binaries learn the new one, then switch
// the secret; removing a key from this list revokes it for every later build.
// See docs/releasing.md.
var trustedKeys = []string{}
