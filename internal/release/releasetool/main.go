// Command releasetool is the release workflow's helper: it validates tags,
// lists and checksums assets, and smoke-tests archives. Run it with
// "go run ./internal/release/releasetool <command> [flags]".
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"rhizome/internal/release"
)

const usage = "usage: releasetool version|assets|checksums|sign|verify|extract|smoke|keygen [flags]"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "releasetool:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tag := fs.String("tag", "", "release tag, e.g. v1.2.3")
	snapshot := fs.String("snapshot", "", "commit of an untagged snapshot build")
	dir := fs.String("dir", "dist", "directory holding release assets")
	exact := fs.Bool("exact", false, "verify: reject files besides the assets, manifest and signature")
	signed := fs.Bool("signed", false, "verify: require a manifest signature by a trusted key")
	keyFile := fs.String("key-file", "", "keygen: file to create with the base64 private seed (never printed)")
	commit := fs.String("commit", "", "smoke: commit the binary must report")
	goos := fs.String("goos", runtime.GOOS, "smoke/extract: archive operating system")
	goarch := fs.String("goarch", runtime.GOARCH, "smoke/extract: archive architecture (darwin uses all)")
	archive := fs.String("archive", "", "extract: archive path")
	out := fs.String("out", "", "extract: destination directory")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
	}
	if args[0] == "keygen" {
		return keygen(*keyFile, stdout)
	}
	if args[0] == "extract" {
		bin, err := release.ExtractBinary(*archive, *goos, *out)
		if err == nil {
			fmt.Fprintln(stdout, bin)
		}
		return err
	}
	v, err := identity(*tag, *snapshot)
	if err != nil {
		return err
	}
	switch args[0] {
	case "version":
		// KEY=VALUE lines, suitable for $GITHUB_OUTPUT.
		fmt.Fprintf(stdout, "tag=%s\nversion=%s\nasset_version=%s\npackage_version=%s\nprerelease=%t\n",
			v.Tag, v.Version, v.AssetVersion(), v.PackageVersion(), v.IsPrerelease())
	case "assets":
		fmt.Fprintln(stdout, strings.Join(append(release.Assets(v.AssetVersion()), release.ManifestName(v.AssetVersion())), "\n"))
	case "checksums":
		if err := release.WriteManifest(*dir, v.AssetVersion()); err != nil {
			return err
		}
		return release.VerifyManifest(*dir, v.AssetVersion(), false)
	case "sign":
		// The seed comes only from the environment, so it never appears in
		// command lines or logs.
		key, err := release.ParseSigningKey(os.Getenv("RHIZOME_RELEASE_SIGNING_KEY"))
		if err != nil {
			return fmt.Errorf("RHIZOME_RELEASE_SIGNING_KEY: %w", err)
		}
		if err := release.SignManifest(*dir, v.AssetVersion(), key); err != nil {
			return err
		}
		// Self-check only; `verify -signed` enforces the trusted key list.
		id, err := release.VerifySignedManifest(*dir, v.AssetVersion(), []ed25519.PublicKey{key.Public().(ed25519.PublicKey)})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "signed %s with key %s\n", release.ManifestName(v.AssetVersion()), id)
	case "verify":
		if err := release.VerifyManifest(*dir, v.AssetVersion(), *exact); err != nil {
			return err
		}
		if *signed {
			keys, err := release.TrustedKeys()
			if err != nil {
				return err
			}
			id, err := release.VerifySignedManifest(*dir, v.AssetVersion(), keys)
			if err != nil {
				return fmt.Errorf("manifest signature: %w", err)
			}
			fmt.Fprintf(stdout, "manifest signature verified with key %s\n", id)
		}
	case "smoke":
		arch := *goarch
		if *goos == "darwin" {
			arch = "all"
		}
		line, err := release.Smoke(ctx, *dir, v, *commit, *goos, arch)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, line)
	default:
		return fmt.Errorf("%s", usage)
	}
	return nil
}

// keygen creates a release signing key. The private seed is written only to
// keyFile (0600, never overwritten); the public key and its id are printed so
// they can be added to internal/release/keys.go.
func keygen(keyFile string, stdout io.Writer) error {
	if keyFile == "" {
		return fmt.Errorf("keygen requires -key-file")
	}
	priv, seed, err := release.GenerateSigningKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(seed + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	pub := priv.Public().(ed25519.PublicKey)
	fmt.Fprintf(stdout, "private seed written to %s; store it as the RHIZOME_RELEASE_SIGNING_KEY secret of the release environment\npublic key: %s\nkey id: %s\n", keyFile, hex.EncodeToString(pub), release.KeyID(pub))
	return nil
}

func identity(tag, snapshot string) (release.Version, error) {
	switch {
	case tag != "" && snapshot != "":
		return release.Version{}, fmt.Errorf("-tag and -snapshot are exclusive")
	case tag != "":
		return release.ParseTag(tag)
	case snapshot != "":
		return release.Snapshot(snapshot)
	}
	return release.Version{}, fmt.Errorf("-tag or -snapshot is required")
}
