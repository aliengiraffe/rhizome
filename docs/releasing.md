# Releasing Rhizome

Pushing a SemVer tag builds, signs, notarizes, smoke-tests and publishes a
GitHub Release in `aliengiraffe/rhizome`. The pipeline is adapted from Lander's
release pipeline in `aliengiraffe/env-manager`. It uses native `codesign` and
`notarytool`, gates the build on executing the binary, and requires an explicit
`Accepted` notarization verdict.

| File | Role |
| --- | --- |
| [`.github/workflows/release.yml`](../.github/workflows/release.yml) | Jobs: `meta` → `build` → `smoke` matrix → `publish`. |
| [`.goreleaser.yaml`](../.goreleaser.yaml) | Cross-builds, darwin fusion and signing hook, archives. Never publishes. |
| [`internal/release/`](../internal/release) | Tag validation, asset matrix, checksum manifest, archive smoke (`releasetool`). |
| [`scripts/release/`](../scripts/release) | Signing, notarization, `.pkg` build, payload verification, release notes. |

## Assets

For tag `vX.Y.Z[-PRE][+BUILD]`, `<v>` is the version without `v`, with `+`
replaced by `.` because GitHub rewrites `+` in asset names.

| Asset | Contents | Signing |
| --- | --- | --- |
| `rhizome_<v>_darwin_all.tar.gz` | Universal (x86_64 + arm64) `rhizome`, LICENSE, README | Developer ID Application, hardened runtime, secure timestamp, notarized |
| `rhizome_<v>_macos.pkg` | The same signed binary at `/usr/local/bin/rhizome` | Developer ID Installer, notarized, stapled |
| `rhizome_<v>_linux_{amd64,arm64}.tar.gz` | `rhizome` | Unsigned |
| `rhizome_<v>_windows_{amd64,arm64}.zip` | `rhizome.exe` | Unsigned; no Authenticode, SmartScreen may warn |
| `rhizome_<v>_checksums.txt` | SHA-256 of every asset above | Covered by the release's integrity only |

Every binary reports its identity with `rhizome --version`, for example
`rhizome 1.3.0-rc.1 (commit <full sha>, built <commit date>)`. The command reads
no configuration, writes nothing and contacts nothing. Local builds report
`dev` and the VCS stamp of the checkout.

The binaries are built with Go 1.27 and need macOS 12 or later.

## Tags

- Tags must be [SemVer 2.0.0](https://semver.org/spec/v2.0.0.html), optionally
  prefixed with a lowercase `v`: `v1.2.3`, `1.2.3`, `v1.3.0-rc.1`,
  `v1.3.0-rc.1+build.5`. The workflow trigger only preselects `v[0-9]*` and
  `[0-9]*`. The `meta` job then validates the full grammar before any job can
  reach secrets, and rejects malformed tags such as `v1.2`, `v01.2.3`,
  `V1.2.3` or `v1.2.3-`, plus numeric parts above 2147483647.
- A prerelease component (`-rc.1`) publishes a **prerelease** that never becomes
  "Latest". Build metadata alone (`v1.2.3+build.5`) is a stable release.
- The binary and the release keep the exact version and tag. The installer
  receipt (`pkgutil --pkg-info com.aliengiraffe.rhizome`) can only hold
  numbers, so it records `MAJOR.MINOR.PATCH`. Use `rhizome --version` to see
  the exact version.
- Ordinary branch pushes never release.

To cut a release from an up-to-date `main`:

```sh
git tag -s v1.3.0-rc.1 -m "rhizome v1.3.0-rc.1"   # or -a if you do not sign tags
git push origin v1.3.0-rc.1
```

## Pipeline and gates

| Trigger | Signing | Result |
| --- | --- | --- |
| SemVer tag push | `RHIZOME_SIGNING=required`, `release` environment | Draft staged, verified, then published |
| Pull request touching Go, module, release or workflow files | ad-hoc, no environment, no secrets | Artifacts only, no release |
| `workflow_dispatch` (manual snapshot) | ad-hoc, no environment, no secrets | Artifacts only, no release |

Snapshots are versioned `0.0.0-snapshot+<12-char commit>`.

1. **meta** (`contents: read`) validates the tag with `releasetool version` and
   confirms the checkout is the tagged commit.
2. **build** (`macos-15`) runs GoReleaser. The darwin slices are fused first.
   The `universal_binaries` post hook (`sign-macos-binary.sh`) then signs the
   binary and runs `codesign --verify --strict` on it and on each slice. In
   production it also requires a Developer ID authority, hardened runtime and
   a secure timestamp, and rejects ad-hoc signatures. It **executes** the
   binary natively and the x86_64 slice through Rosetta when Rosetta is
   present, then notarizes and re-executes. All of this happens before any
   archive exists. Next, `build-macos-pkg.sh` packages the binary extracted
   from the darwin archive. In production it signs, notarizes and staples the
   package, then checks it with `stapler validate` and
   `spctl --assess --type install`. `verify-pkg-payload.sh` expands the package
   without installing it. It requires exactly one payload file,
   `usr/local/bin/rhizome`, and no installer scripts. The payload must be
   byte-identical to the archive binary and must run. Checksums are written
   last.
3. **smoke** downloads the assets on native hosts: Linux amd64 and arm64,
   Windows amd64 and arm64, macOS arm64 and Intel. Each host verifies the
   manifest, extracts its archive and runs `--version`, which must report the
   expected version and commit. The macOS hosts re-verify the package payload.
   In production they also check the Developer ID signature of the bare
   binary, and the installer signature, stapled ticket and Gatekeeper verdict
   of the package. `spctl` cannot assess a bare binary, so its notarization
   `Accepted` verdict is enforced in the build job.
4. **publish** (`contents: write`, no signing secrets) runs only for tags and
   only after every build and smoke job succeeds. It verifies the artifact set
   exactly and confirms the tag still names the built commit. It stages a
   **draft** with every asset, downloads the draft's assets and re-verifies
   them against the manifest, then publishes. A run that fails at any point
   leaves no public release.

Notarization fails unless `notarytool` exits zero **and** its JSON response is
an object with a submission id and `"status": "Accepted"`. `Invalid`,
`Rejected`, `In Progress`, malformed output or a nonzero exit all fail, and
Apple's log is printed when a submission id exists.

### Execution coverage

| Platform | Executed on | Notes |
| --- | --- | --- |
| macOS arm64 | `macos-15` (build hook, package payload, smoke) | native |
| macOS x86_64 | `macos-15` via Rosetta (build hook); `macos-15-intel` (smoke) | build hook reports `NOT EXECUTED` if Rosetta is absent |
| Linux amd64 / arm64 | `ubuntu-latest` / `ubuntu-24.04-arm` | native |
| Windows amd64 / arm64 | `windows-latest` / `windows-11-arm` | native, unsigned |

The arm64 Linux and Windows hosted runners and `macos-15-intel` must be available
to the repository. If GitHub stops offering one, its smoke job never succeeds,
so nothing is published. Replace the runner label or record the gap rather than
removing the job silently. Smoke runs check only that the binary starts and
reports its identity. Native service behavior is covered by
[verification.md](verification.md).

## One-time setup

Apple credentials are **not** shared from env-manager automatically; add them
to this repository. Never paste their values into source, issues or logs.

1. Create a GitHub environment named **`release`**
   (Settings → Environments). Under *Deployment branches and tags*, allow only
   tags matching `v*` and `[0-9]*`. Add required reviewers if you want a
   manual approval before signing.
2. Add these **environment secrets** to `release`. Do not add them as
   repository secrets, because pull request jobs could reference those:

   | Secret | Value |
   | --- | --- |
   | `APPLE_DEVELOPER_ID_CERT_P12_BASE64` | base64 of the Developer ID **Application** certificate + key `.p12` |
   | `APPLE_DEVELOPER_ID_CERT_PASSWORD` | its export password |
   | `APPLE_INSTALLER_CERT_P12_BASE64` | base64 of the Developer ID **Installer** certificate + key `.p12` |
   | `APPLE_INSTALLER_CERT_PASSWORD` | its export password |
   | `APPLE_NOTARY_API_KEY_P8_BASE64` | base64 of the App Store Connect API key `.p8` (Developer role) |
   | `APPLE_NOTARY_API_KEY_ID` | the key's id |
   | `APPLE_NOTARY_API_ISSUER_ID` | the issuer id |

3. Optional **environment variables** pin the identities when the keychain
   holds more than one: `APPLE_DEVELOPER_ID_IDENTITY`
   (`Developer ID Application: … (TEAMID)`) and `APPLE_INSTALLER_IDENTITY`
   (`Developer ID Installer: … (TEAMID)`). Without them, the scripts use the
   first matching identity in the temporary keychain.
4. Recommended: enable GitHub **immutable releases** so a published release's
   assets and tag cannot change.

Encode files with `base64 -i cert.p12 | pbcopy`. The build job checks every
secret before using any of them, and fails with the names of any that are
missing. Certificates go into a throwaway keychain under `$RUNNER_TEMP`. The
`.p12` files are deleted right after import, and an `always()` step deletes the
keychain and the `.p8` key whether the job succeeds or fails.

## Validating before a stable release

1. Open a pull request (or run the workflow manually). The ad-hoc snapshot
   exercises the full matrix without secrets.
2. Push a prerelease tag such as `v0.1.0-rc.1`. This is the controlled
   end-to-end check of real signing, notarization, stapling, publication and
   downloads.
3. Check the result from a clean Mac (not the build runner):

   ```sh
   gh release download v0.1.0-rc.1 -R aliengiraffe/rhizome -D /tmp/rz && cd /tmp/rz
   shasum -a 256 -c rhizome_0.1.0-rc.1_checksums.txt
   tar xzf rhizome_0.1.0-rc.1_darwin_all.tar.gz rhizome
   codesign --verify --strict --verbose=2 rhizome
   pkgutil --check-signature rhizome_0.1.0-rc.1_macos.pkg
   xcrun stapler validate rhizome_0.1.0-rc.1_macos.pkg
   spctl -a -t install -vv rhizome_0.1.0-rc.1_macos.pkg
   pkgutil --expand-full rhizome_0.1.0-rc.1_macos.pkg x && find x -path '*Payload*' -type f
   ./rhizome --version
   ```

   Record the outcome in [verification.md](verification.md).

## Installing and upgrading

- **macOS installer**: open the `.pkg` or run
  `sudo installer -pkg rhizome_<v>_macos.pkg -target /`. It installs only
  `/usr/local/bin/rhizome`, runs no scripts, and never creates or changes
  `~/.rhizome`, tokens, runtime state or the observer service. Run
  `rhizome setup` and `rhizome service install` yourself. Upgrades replace the
  binary in place.
- **Archives**: verify the checksum, extract, and put `rhizome` (or
  `rhizome.exe`) on `PATH`. A bare macOS binary has no stapled ticket, so
  Gatekeeper checks its notarization online on first run. Choose the `.pkg`
  for offline installs. Archives downloaded with a browser get a quarantine
  attribute; downloads made with `curl` or `gh` do not.
- **Upgrading a running observer**: the service keeps running the old binary
  until restarted. Run `rhizome service stop` and then `rhizome service start`,
  and restart MCP clients so they launch the new binary. The service
  definition records the binary's absolute path. If the path changes (for
  example when moving from a source build to the package), run
  `rhizome service uninstall` with the old binary, then
  `rhizome service install` with the new one.
- **Uninstalling the package**: first run `rhizome uninstall --all` (or
  `rhizome uninstall` to pick agents) so the coding agents stop launching the
  binary and the service definition is removed; see
  [uninstalling](reference.md#uninstalling). Then run
  `sudo rm /usr/local/bin/rhizome` and
  `sudo pkgutil --forget com.aliengiraffe.rhizome`. `~/.rhizome` is left
  untouched; delete it yourself only if you want to remove configuration,
  tokens and state.

## Reruns and recovery

- **Failed before publish** (signing, notarization, package, smoke or upload):
  fix the cause and use *Re-run failed jobs*, or rerun the whole workflow for
  the same tag. The publish job deletes an unpublished draft from an earlier
  attempt before staging a new one. Nothing public changes.
- **Tag moved or deleted** during a run: publish refuses because the tag no
  longer names the built commit. Do not retag a version that was ever pushed.
  Cut a new version instead.
- **Already published**: the publish job refuses to replace a published
  release or its assets. Never use `--clobber` on a published release. To
  correct one, publish a new version and, if needed, mark the bad release as a
  prerelease or edit its notes.
- **Concurrency**: runs for the same tag are serialized and never cancelled.
  Pull request runs cancel superseded runs for the same branch.
- **Missing credentials** fail the build job with the missing secret names.
  There is no snapshot fallback for tags.

## Not included

There is no Windows Authenticode signing or MSI, no Linux packages (deb/rpm),
no Homebrew tap, no public mirror and no auto-updater. The macOS package is
CLI-only, with no menu-bar app and no launch agents. Linux and Windows assets
are unsigned; verify them with the checksum manifest.
