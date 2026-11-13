//go:build darwin || linux

package oai

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type nativeStore struct{}

func stringIndex(i int) string { return strconv.Itoa(i) }

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxRecord {
		return 0, errStore
	}
	return b.Buffer.Write(p)
}
func storeCommand(ctx context.Context, input string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(input)
	var out limitedOutput
	cmd.Stdout = &out // Never forward native stderr (it can echo a secret).
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && ((runtime.GOOS == "darwin" && exit.ExitCode() == 44) || (runtime.GOOS == "linux" && exit.ExitCode() == 1)) {
			return "", errMissing
		}
		return "", errStore
	}
	return strings.TrimSpace(out.String()), nil
}
func (nativeStore) Get(ctx context.Context, key string) (string, error) {
	if runtime.GOOS == "linux" {
		return storeCommand(ctx, "", "secret-tool", "lookup", "application", "rhizome-oai", "record", key)
	}
	s, e := storeCommand(ctx, "", "/usr/bin/security", "find-generic-password", "-s", "com.aliengiraffe.rhizome.oai", "-a", key, "-w")
	if e != nil {
		return "", e
	}
	b, e := base64.StdEncoding.DecodeString(s)
	if e != nil {
		return "", errStore
	}
	return string(b), nil
}
func (nativeStore) Set(ctx context.Context, key, value string) error {
	if runtime.GOOS == "linux" {
		_, e := storeCommand(ctx, value, "secret-tool", "store", "--label=Rhizome experimental OAI", "application", "rhizome-oai", "record", key)
		return e
	}
	// Only generated ASCII keys and base64 values enter security's stdin command parser.
	line := "add-generic-password -U -s com.aliengiraffe.rhizome.oai -a " + key + " -w " + base64.StdEncoding.EncodeToString([]byte(value)) + "\n"
	if len(line) > 4000 {
		return errStore
	}
	_, e := storeCommand(ctx, line, "/usr/bin/security", "-i")
	if e != nil {
		return e
	}
	// Interactive security may exit successfully even when an individual command fails.
	got, e := (nativeStore{}).Get(ctx, key)
	if e != nil || got != value {
		return errStore
	}
	return nil
}
func (nativeStore) Delete(ctx context.Context, key string) error {
	if runtime.GOOS == "linux" {
		_, e := storeCommand(ctx, "", "secret-tool", "clear", "application", "rhizome-oai", "record", key)
		return e
	}
	_, e := storeCommand(ctx, "", "/usr/bin/security", "delete-generic-password", "-s", "com.aliengiraffe.rhizome.oai", "-a", key)
	return e
}
