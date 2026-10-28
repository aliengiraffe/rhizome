// Command releasetool is the release workflow's helper: it validates tags,
// lists and checksums assets, smoke-tests archives and renders the Homebrew
// cask. Run it with
// "go run ./internal/release/releasetool <command> [flags]".
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"rhizome/internal/release"
)

const usage = "usage: releasetool version|assets|checksums|verify|extract|smoke|cask [flags]"

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
	exact := fs.Bool("exact", false, "verify: reject files besides the assets and manifest")
	commit := fs.String("commit", "", "smoke: commit the binary must report")
	goos := fs.String("goos", runtime.GOOS, "smoke/extract: archive operating system")
	goarch := fs.String("goarch", runtime.GOARCH, "smoke/extract: archive architecture (darwin uses all)")
	archive := fs.String("archive", "", "extract: archive path")
	out := fs.String("out", "", "extract: destination directory")
	current := fs.String("current", "", "cask: the tap's existing cask file, if any")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments %q", fs.Args())
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
	case "verify":
		return release.VerifyManifest(*dir, v.AssetVersion(), *exact)
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
	case "cask":
		return cask(stdout, *dir, *current, v)
	default:
		return fmt.Errorf("%s", usage)
	}
	return nil
}

// cask writes the rendered cask to stdout. When the tap already holds a newer
// release it writes the existing cask unchanged, so the caller sees no change
// and never downgrades the tap.
func cask(stdout io.Writer, dir, current string, v release.Version) error {
	rendered, err := release.RenderCask(dir, v)
	if err != nil {
		return err
	}
	var existing []byte
	if current != "" {
		existing, err = os.ReadFile(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	superseded, err := release.CaskSuperseded(existing, v)
	if err != nil {
		return err
	}
	if superseded {
		fmt.Fprintf(os.Stderr, "releasetool: the tap already holds a release newer than %s; keeping it\n", v.Version)
		rendered = existing
	}
	_, err = stdout.Write(rendered)
	return err
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
