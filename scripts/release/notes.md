## Downloads

| Platform | Asset | Signing |
| --- | --- | --- |
| macOS installer (Intel and Apple silicon) | `rhizome_<version>_macos.pkg` | Developer ID Installer, notarized, stapled |
| macOS binary (universal) | `rhizome_<version>_darwin_all.tar.gz` | Developer ID Application, hardened runtime, notarized |
| Linux amd64 / arm64 | `rhizome_<version>_linux_<arch>.tar.gz` | Unsigned |
| Windows amd64 / arm64 | `rhizome_<version>_windows_<arch>.zip` | Unsigned (SmartScreen may warn) |

Verify downloads against `rhizome_<version>_checksums.txt` (SHA-256).
`rhizome_<version>_checksums.txt.sig` is the Ed25519 release signature over
that manifest; the auto-updater built into `rhizome` accepts a release only
when it verifies against the keys compiled into the running binary. The
installer places only `/usr/local/bin/rhizome`; it never changes `~/.rhizome`
configuration, tokens or state and never installs or restarts the observer.
A running observer with `[update] enabled = true` (the default) installs
later releases itself and restarts; otherwise run `rhizome update --apply`,
or replace the binary and run `rhizome service stop` and
`rhizome service start`. See `docs/releasing.md` for verification and
platform notes.
