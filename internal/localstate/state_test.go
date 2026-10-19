package localstate

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLockRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	a, err := Lock(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := Lock(path, false); !errors.Is(err, ErrBusy) {
		if b != nil {
			b.Close()
		}
		t.Fatal(err)
	}
	a.Close()
	b, err := Lock(path, false)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
}
