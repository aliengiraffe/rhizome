## Downloads

| Platform | Asset | Signing |
| --- | --- | --- |
| macOS installer (Intel and Apple silicon) | `rhizome_<version>_macos.pkg` | Developer ID Installer, notarized, stapled |
| macOS binary (universal) | `rhizome_<version>_darwin_all.tar.gz` | Developer ID Application, hardened runtime, notarized |
| Linux amd64 / arm64 | `rhizome_<version>_linux_<arch>.tar.gz` | Unsigned |
| Windows amd64 / arm64 | `rhizome_<version>_windows_<arch>.zip` | Unsigned (SmartScreen may warn) |

Verify downloads against `rhizome_<version>_checksums.txt` (SHA-256). The
installer places only `/usr/local/bin/rhizome`; it never changes `~/.rhizome`
configuration, tokens or state and never installs or restarts the observer.
After upgrading a running observer, run `rhizome service stop` and
`rhizome service start`. See `docs/releasing.md` for verification and
platform notes.
