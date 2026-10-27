package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// TeamID is the Apple Developer Team that signs Rhizome releases. A macOS
// candidate signed by any other team is rejected.
const TeamID = "6BY6T468D8"

// signingIdentifier is the code signing identifier set by the release pipeline.
const signingIdentifier = "com.aliengiraffe.rhizome"

// codesignTimeout bounds one codesign invocation.
const codesignTimeout = 60 * time.Second

// maxCodesignOutput bounds captured codesign output.
const maxCodesignOutput = 64 << 10

// BinaryVerifier checks a candidate executable with platform tooling before
// it is installed. A nil verifier performs no platform check (Linux and
// Windows assets are unsigned; their trust rests on the signed manifest).
type BinaryVerifier interface {
	Verify(ctx context.Context, path string) error
}

// PlatformVerifier returns the verifier for goos: codesign on darwin, nil
// elsewhere.
func PlatformVerifier(goos string) BinaryVerifier {
	if goos == "darwin" {
		return &codesignVerifier{run: runCodesign}
	}
	return nil
}

// codesignVerifier requires a strict-verifying Developer ID Application
// signature with hardened runtime, secure timestamp, the release identifier
// and the expected Team ID. Ad-hoc signatures fail.
type codesignVerifier struct {
	run func(ctx context.Context, args ...string) (string, error)
}

func runCodesign(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, codesignTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codesign", args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &bounded{w: &out}, &bounded{w: &out}
	err := cmd.Run()
	return out.String(), err
}

type bounded struct{ w *bytes.Buffer }

func (b *bounded) Write(p []byte) (int, error) {
	if room := maxCodesignOutput - b.w.Len(); room > 0 {
		b.w.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// Verify implements BinaryVerifier.
func (v *codesignVerifier) Verify(ctx context.Context, path string) error {
	if _, err := v.run(ctx, "--verify", "--strict", "--verbose=2", path); err != nil {
		return coded("codesign_failed", fmt.Errorf("codesign --verify --strict: %w", err))
	}
	info, err := v.run(ctx, "--display", "--verbose=4", path)
	if err != nil {
		return coded("codesign_failed", fmt.Errorf("codesign --display: %w", err))
	}
	if err := checkCodesignDisplay(info); err != nil {
		return coded("codesign_failed", err)
	}
	return nil
}

var hardenedRuntime = regexp.MustCompile(`(?m)^CodeDirectory .*flags=0x[0-9a-f]+\([^)]*runtime[^)]*\)`)

// checkCodesignDisplay applies the release signature policy to the output of
// `codesign --display --verbose=4`.
func checkCodesignDisplay(info string) error {
	var problems []string
	has := func(prefix string) bool {
		for _, line := range strings.Split(info, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), prefix) {
				return true
			}
		}
		return false
	}
	if has("Signature=adhoc") {
		problems = append(problems, "ad-hoc signature")
	}
	if !has("Identifier=" + signingIdentifier) {
		problems = append(problems, "unexpected signing identifier")
	}
	if !has("Authority=Developer ID Application:") {
		problems = append(problems, "no Developer ID Application authority")
	}
	if !has("TeamIdentifier=" + TeamID) {
		problems = append(problems, "team identifier is not "+TeamID)
	}
	if !hardenedRuntime.MatchString(info) {
		problems = append(problems, "hardened runtime not enabled")
	}
	if !has("Timestamp=") {
		problems = append(problems, "no secure timestamp")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}
