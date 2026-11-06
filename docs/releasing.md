# Releasing Rhizome

Pushing a SemVer tag builds, signs, notarizes, smoke-tests and publishes a
GitHub Release in `aliengiraffe/rhizome`, then publishes the same binaries to
npm as [`@aliengiraffe/rhizome`](#npm). A stable tag also updates the Homebrew
cask in `aliengiraffe/homebrew-spaceship`. The pipeline is adapted from Lander's
release pipeline in `aliengiraffe/env-manager`. It uses native `codesign` and
`notarytool`, gates the build on executing the binary, and requires an explicit
`Accepted` notarization verdict.

| File | Role |
| --- | --- |
| [`.github/workflows/release.yml`](../.github/workflows/release.yml) | Jobs: `meta` → `build` → `smoke` matrix → `publish` → `publish-npm` and `tap` (stable tags only). |
| [`.goreleaser.yaml`](../.goreleaser.yaml) | Cross-builds, darwin fusion and signing hook, archives. Never publishes. |
| [`internal/release/`](../internal/release) | Tag validation, asset matrix, checksum manifest, manifest signing and trusted keys, archive smoke, npm package generation, smoke and publishing, Homebrew cask rendering (`releasetool`). |
| [`internal/update/`](../internal/update) | The auto-updater that consumes these assets ([service.md](service.md#automatic-updates)). |
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
| `rhizome_<v>_checksums.txt` | SHA-256 of every asset above | Signed by the detached signature below |
| `rhizome_<v>_checksums.txt.sig` | Ed25519 signature over the manifest | [Release signing key](#release-signing-key); verified offline by the auto-updater and `releasetool verify -signed` |

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
| Stable SemVer tag push | `RHIZOME_SIGNING=required`, `release` environment | Draft staged, verified, then published; Homebrew cask updated |
| Prerelease SemVer tag push | `RHIZOME_SIGNING=required`, `release` environment | Draft staged, verified, then published as a prerelease; tap untouched |
| Pull request touching Go, module, release or workflow files | ad-hoc, no environment, no secrets | Artifacts only, no release, no tap |
| `workflow_dispatch` (manual snapshot) | ad-hoc, no environment, no secrets | Artifacts only, no release, no tap |

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
   last and, in production, signed with the release key
   (`releasetool sign`, seed from the `RHIZOME_RELEASE_SIGNING_KEY` secret);
   `releasetool verify -exact -signed` then checks the signature against the
   public keys compiled into the checkout, so a key missing from
   `internal/release/keys.go` fails the build rather than every later update.
   Snapshots are not signed.
3. **smoke** downloads the assets on native hosts: Linux amd64 and arm64,
   Windows amd64 and arm64, macOS arm64 and Intel. Each host verifies the
   manifest, extracts its archive and runs `--version`, which must report the
   expected version and commit. It then runs the npm launcher tests and
   `releasetool npm-smoke`: generate all six npm packages, `npm pack` each,
   install the launcher and native platform tarball into a temporary prefix
   (offline) and run the installed `rhizome --version` with the same
   expectations. The macOS hosts re-verify the package payload.
   In production they also check the Developer ID signature of the bare
   binary, and the installer signature, stapled ticket and Gatekeeper verdict
   of the package. `spctl` cannot assess a bare binary, so its notarization
   `Accepted` verdict is enforced in the build job.
4. **publish** (`contents: write`, no signing secrets) runs only for tags and
   only after every build and smoke job succeeds. It verifies the artifact set
   exactly, including the manifest signature, and confirms the tag still names
   the built commit. It stages a **draft** with every asset, downloads the
   draft's assets and re-verifies them (manifest, signature and byte-identical
   signature file), then publishes. A run that fails at any point
   before this leaves no public release.
5. **publish-npm** (`contents: read`, `id-token: write`, `release`
   environment, no secrets) runs only for tags and only after **publish**
   succeeds. It re-verifies the assets and manifest signature, generates the
   [npm packages](#npm) and publishes them through trusted publishing.
6. **tap** (`contents: read`, `homebrew-tap` environment, no signing secrets)
   runs only for stable tags, only after `publish` succeeds. It confirms the
   release is published and not a prerelease, downloads its assets and
   re-verifies them exactly against the manifest and its signature. It then
   mints a short-lived GitHub App token scoped to `homebrew-spaceship`
   contents and runs `releasetool cask`, which renders `Casks/rhizome.rb` from
   the manifest's digests. It never rebuilds anything. The cask is written
   through the contents API only when it changed, so a re-run makes no empty
   commit. A tap that already holds a newer release is kept rather than
   downgraded, and a cask the pipeline did not generate is refused rather
   than replaced.

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

## Homebrew cask

The tap job keeps GoReleaser's `release: disable: true`. GoReleaser's own
`homebrew_casks` would publish from the build job, which holds the signing
secrets and runs before the release is verified and public. Instead,
`releasetool cask` renders the cask from the published, re-verified assets.
The generated `Casks/rhizome.rb` starts with a "DO NOT EDIT" header; never
edit it in the tap by hand, because the next release refuses to replace a
cask without that header.

- **macOS** (arm64 and Intel): `on_macos` installs the universal
  `rhizome_<v>_darwin_all.tar.gz`, the same signed and notarized binary as the
  archive and the `.pkg`.
- **Linux** (amd64 and arm64, Homebrew on Linux): `on_linux` installs the
  unsigned Linux archives. Windows has no Homebrew.
- URLs interpolate `#{version}`. For a build-metadata tag the asset part stays
  literal, because asset names replace `+` with `.`.
- `binary "rhizome"` is the only artifact. There is no `postflight` and no
  `zap`: Homebrew never installs, starts, restarts or removes the observer
  service and never touches `~/.rhizome`. A caveat prints the restart steps.

Dry run, from a checkout, without pushing anything:

```sh
gh release download v0.9.1 -R aliengiraffe/rhizome -D /tmp/rz
go run ./internal/release/releasetool cask -tag v0.9.1 -dir /tmp/rz > /tmp/rhizome.rb
brew tap-new --no-git rhizomescratch/test
cp /tmp/rhizome.rb "$(brew --repository rhizomescratch/test)/Casks/rhizome.rb"
brew style rhizomescratch/test/rhizome
brew audit --cask --strict --online rhizomescratch/test/rhizome
brew untap rhizomescratch/test
```

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
   | `RHIZOME_RELEASE_SIGNING_KEY` | base64 Ed25519 seed from `releasetool keygen` ([release signing key](#release-signing-key)) |

3. Optional **environment variables** pin the identities when the keychain
   holds more than one: `APPLE_DEVELOPER_ID_IDENTITY`
   (`Developer ID Application: … (TEAMID)`) and `APPLE_INSTALLER_IDENTITY`
   (`Developer ID Installer: … (TEAMID)`). Without them, the scripts use the
   first matching identity in the temporary keychain.
4. Recommended: enable GitHub **immutable releases** so a published release's
   assets and tag cannot change.
5. For the Homebrew cask, create a second environment named
   **`homebrew-tap`**, also restricted to tags matching `v*` and `[0-9]*`. Add
   two environment secrets for the GitHub App installed on
   `aliengiraffe/homebrew-spaceship` with *Contents: read and write* (the app
   Vigilante's release uses): `APP_ID` (its app id) and `APP_PRIVATE_KEY` (its
   PEM private key). Never add them to `release`, which would give the signing
   jobs tap access, or as repository secrets. The tap job fails with the names
   of any that are missing; the release itself stays published.

Encode files with `base64 -i cert.p12 | pbcopy`. The build job checks every
secret before using any of them, and fails with the names of any that are
missing. Certificates go into a throwaway keychain under `$RUNNER_TEMP`. The
`.p12` files are deleted right after import, and an `always()` step deletes the
keychain and the `.p8` key whether the job succeeds or fails.

## Release signing key

Linux and Windows archives are unsigned, and the checksum manifest downloaded
from the same place as an asset proves integrity only. The detached Ed25519
signature `rhizome_<v>_checksums.txt.sig` is what makes a release
authenticatable offline on every platform: the public keys are compiled into
the binary (`internal/release/keys.go`) and the private seed exists only as the
`RHIZOME_RELEASE_SIGNING_KEY` secret of the `release` environment. The
[auto-updater](service.md#automatic-updates) accepts a release only when its
manifest verifies against one of those keys; the format is one line,
`rhizome-release-signature v1 <key id> <base64 signature>`, where the key id
is the first eight bytes of the public key's SHA-256. Standard-library
cryptography only; no external verifier is needed.

Generate the key once on a trusted machine, never in CI or an agent session:

```sh
go run ./internal/release/releasetool keygen -key-file /secure/path/rhizome-release.seed
```

The command refuses to overwrite, writes the seed with mode 0600 and prints
the public key and key id. Store the seed's content as the
`RHIZOME_RELEASE_SIGNING_KEY` environment secret on `release` (never a
repository secret, never in source, issues or logs), then add the printed
public key to `trustedKeys` in `internal/release/keys.go` and commit. Until
both exist, production builds fail at the signing step and no binary can
auto-update, which is the intended fail-closed state.

Verify a download by hand with
`go run ./internal/release/releasetool verify -tag v<v> -dir <download dir> -signed`
from a checkout that lists the key.

**Rotation.** Binaries trust the list compiled into them, so rotate in two
releases: add the new public key to `keys.go` and publish a release still
signed by the old key (installed binaries learn the new key when they update
to it), then switch the secret to the new seed. Remove the old key from
`keys.go` once no supported release needs it.

**Revocation.** Remove the compromised key from `keys.go` and switch the
secret immediately. Binaries built after the removal reject releases signed
by that key. Binaries built before it still trust it; they can only be moved
forward by a release signed by a key they already trust, so publish one
promptly and tell users of older builds to verify and install it by hand.
Published releases are immutable, so a release signed by a compromised key is
not unpublished but should be marked as a prerelease with a note.

## npm

Each release is also published to npm in the esbuild style: one small
launcher package plus one package per platform, selected by npm's `os`/`cpu`
fields. No package has install scripts (npm 12 no longer runs dependency
lifecycle scripts), and nothing downloads at install time.

| Package | `os` | `cpu` | Contents |
| --- | --- | --- | --- |
| `@aliengiraffe/rhizome` | – | – | `bin/rhizome.js` launcher; `optionalDependencies` on every platform package at the identical exact version |
| `@aliengiraffe/rhizome-darwin` | `darwin` | `x64`, `arm64` | `bin/rhizome`, the signed universal binary from the darwin archive |
| `@aliengiraffe/rhizome-linux-x64` | `linux` | `x64` | `bin/rhizome` |
| `@aliengiraffe/rhizome-linux-arm64` | `linux` | `arm64` | `bin/rhizome` |
| `@aliengiraffe/rhizome-win32-x64` | `win32` | `x64` | `bin/rhizome.exe` (unsigned; SmartScreen may warn) |
| `@aliengiraffe/rhizome-win32-arm64` | `win32` | `arm64` | `bin/rhizome.exe` (unsigned; SmartScreen may warn) |

`releasetool npm -tag <tag> -dir <assets> -out <dir>` (or `-snapshot <commit>`)
verifies the checksum manifest, extracts each binary and LICENSE with the same
strict extractor as the smoke runs, and writes the six package directories,
printing them in publish order. Every `package.json` carries
`repository.url` `git+https://github.com/aliengiraffe/rhizome.git` (trusted
publishing requires the exact value), `publishConfig.access: public` and the
source tag under `rhizome.tag`. The launcher (`internal/release/npm/rhizome.js`,
plain CommonJS) resolves the platform binary, runs it with inherited stdio,
forwards the exit status and re-raises a terminating signal. On an
unsupported platform or a missing platform package (for example installed
with `--omit=optional`) it names the package and points at GitHub Releases.
`task test-npm` runs its `node:test` suite.

**Versions and dist-tags.** The npm version is the tag without `v` and without
SemVer build metadata, which npm ignores: `v1.2.3` → `1.2.3` (`latest`),
`v1.3.0-rc.1` → `1.3.0-rc.1` (`next`, never `latest`), `v1.2.3+build.5` →
`1.2.3` (`latest`). Snapshots map to `0.0.0-snapshot-<commit>` and are never
published. `releasetool version` prints `npm_version` and `npm_dist_tag`.

**Publishing.** `releasetool npm-publish -tag <tag> -out <dir>` requires npm
11.5.1 or later, then publishes the platform packages first and the launcher
last with `npm publish --tag <dist-tag>`. Before each publish it asks the
registry for that exact version: one published from the same tag is skipped,
so a run that failed halfway can be re-run without cleanup; one published from
a different tag (`v1.2.3` and `v1.2.3+build.5`) fails the job, so cut a new
version instead. A failed platform publish stops before the launcher, so the
launcher never references a missing platform package.

**Trusted publishing.** `publish-npm` authenticates with a short-lived OIDC
token, not an npm token: no `NPM_TOKEN` or `NODE_AUTH_TOKEN` exists in the
repository or the `release` environment. The repository is public, so npm
records provenance attestations automatically. npm validates the calling
workflow's file name, so the job must stay in `release.yml` (not a reusable
workflow). Settings for **each** of the six packages on npmjs.com
(Settings → Trusted publishing → GitHub Actions):

| Field | Value |
| --- | --- |
| Organization or user | `aliengiraffe` |
| Repository | `rhizome` |
| Workflow filename | `release.yml` |
| Environment | `release` |
| Allowed actions | `npm publish` (stage publish is always allowed) |

### npm bootstrap runbook

Trusted publishing can only be configured for a package that already exists,
and a new trusted publisher configuration expires unless a publish uses it
within 2 days. The first publish is therefore manual, done once by an owner
of the `aliengiraffe` npm organization (2FA enforced). CI never does it.

1. Generate the packages from the existing v0.9.3 release (it predates the
   signed manifest, so `verify` checks the checksums without `-signed`):

   ```sh
   gh release download v0.9.3 -R aliengiraffe/rhizome -D /tmp/rz-assets
   go run ./internal/release/releasetool verify -tag v0.9.3 -dir /tmp/rz-assets -exact
   go run ./internal/release/releasetool npm -tag v0.9.3 -dir /tmp/rz-assets -out /tmp/rz-npm
   ```

2. `npm login` (npm 11 or later recommended), then publish each platform
   package and the launcher last, approving each with 2FA:

   ```sh
   for p in darwin linux-x64 linux-arm64 win32-x64 win32-arm64; do npm publish "/tmp/rz-npm/rhizome-$p"; done
   npm publish /tmp/rz-npm/rhizome
   ```

3. Configure trusted publishing for each of the six packages with the
   settings above.
4. Within 2 days of step 3, push the next release tag so CI completes the
   first trusted publish. Otherwise the configurations expire and step 3 must
   be repeated.
5. After CI has published, set each package's *Publishing access* to
   "Require two-factor authentication and disallow tokens".

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

- **Homebrew** (macOS, or Linux with Homebrew):
  `brew install --cask aliengiraffe/spaceship/rhizome`. Stable releases
  only. The cask links the binary into Homebrew's `bin` (for example
  `/opt/homebrew/bin/rhizome`). Upgrade with `brew upgrade --cask rhizome`
  and remove it with `brew uninstall --cask rhizome`. Neither touches
  `~/.rhizome` or the observer service. On macOS, setup and the service
  record the stable `bin/rhizome` link, so after an upgrade only the restart
  below is needed. On Linux they record the resolved, versioned Caskroom path,
  which an upgrade removes: run `rhizome service uninstall`,
  `rhizome service install` and `rhizome setup --agents` after each upgrade.
  Run `rhizome service uninstall` before `brew uninstall` if you installed the
  service. The observer's auto-updater follows the link and replaces the
  binary inside the Caskroom without Homebrew knowing; set
  `[update] enabled = false` if you want `brew upgrade` to own upgrades.
- **macOS installer**: open the `.pkg` or run
  `sudo installer -pkg rhizome_<v>_macos.pkg -target /`. It installs only
  `/usr/local/bin/rhizome`, runs no scripts, and never creates or changes
  `~/.rhizome`, tokens, runtime state or the observer service. Run
  `rhizome setup` and `rhizome service install` yourself. Upgrades replace the
  binary in place.
- **npm**: `npm i -g @aliengiraffe/rhizome` (Node.js 18 or later). The
  binary lives in the platform package inside the global `node_modules`, and
  `rhizome setup` and `rhizome service install` record that absolute path;
  `npm i -g @aliengiraffe/rhizome@latest` upgrades it in place. Use a global
  install for setup: `npx @aliengiraffe/rhizome` runs from npm's cache, whose
  path can disappear. Automatic updates replace the binary inside
  `node_modules` when that directory is writable, while npm keeps reporting
  the package version it installed.
- **Archives**: verify the checksum, extract, and put `rhizome` (or
  `rhizome.exe`) on `PATH`. A bare macOS binary has no stapled ticket, so
  Gatekeeper checks its notarization online on first run. Choose the `.pkg`
  for offline installs. Archives downloaded with a browser get a quarantine
  attribute; downloads made with `curl` or `gh` do not.
- **Automatic updates**: an installed observer with `[update] enabled = true`
  (the default) finds newer stable releases, verifies them (manifest
  signature, SHA-256, macOS Developer ID and Team ID, `--version`) and
  restarts itself onto them; see [service.md](service.md#automatic-updates).
  `rhizome update` reports and `rhizome update --apply` installs on demand.
  The updater needs write access to the binary's directory; for the package
  at `/usr/local/bin` it reports `not_writable` and you install the next
  package yourself. Releases before the updater existed must be upgraded by
  hand once.
- **Upgrading a running observer by hand**: the service keeps running the old
  binary until restarted. Run `rhizome service restart`, and restart MCP clients
  so they launch the new
  binary. The service definition records the binary's absolute path. If the
  path changes (for example when moving from a source build to the package),
  run `rhizome service uninstall` with the old binary, then
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
- **npm publish failed** after the GitHub Release was published: re-run the
  failed `publish-npm` job. Versions already published from the tag are
  skipped. An expired or missing trusted publisher configuration fails with
  an authentication error; fix it on npmjs.com and re-run.
- **Tap failed** (missing app secrets, token, API or conflict errors): the
  GitHub Release stays published and the run fails visibly. Fix the cause and
  re-run only the failed `tap` job; it is idempotent. A conflict from a
  concurrent tap update is safe to re-run.
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
no nightly or prerelease Homebrew cask and no public mirror besides npm. The auto-updater updates the `rhizome`
binary only, never agent clients, skills, hooks or plugins (`rhizome setup`
owns those), with no delta updates and no rollback beyond the single previous
binary kept for one cycle. The macOS package is CLI-only, with no menu-bar app
and no launch agents. Linux and Windows binaries are unsigned; the signed
checksum manifest is their authenticity check.
