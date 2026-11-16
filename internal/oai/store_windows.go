package oai

import (
	"context"
	"errors"
	"github.com/danieljoos/wincred"
	"strconv"
	"syscall"
)

type nativeStore struct{}

func stringIndex(i int) string { return strconv.Itoa(i) }
func (nativeStore) Get(ctx context.Context, key string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	c, e := wincred.GetGenericCredential(key)
	if errors.Is(e, syscall.ERROR_NOT_FOUND) {
		return "", errMissing
	}
	if e != nil {
		return "", errStore
	}
	return string(c.CredentialBlob), nil
}
func (nativeStore) Set(ctx context.Context, key, value string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c := wincred.NewGenericCredential(key)
	c.CredentialBlob = []byte(value)
	if c.Write() != nil {
		return errStore
	}
	return nil
}
func (nativeStore) Delete(ctx context.Context, key string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c, e := wincred.GetGenericCredential(key)
	if errors.Is(e, syscall.ERROR_NOT_FOUND) {
		return errMissing
	}
	if e != nil || c.Delete() != nil {
		return errStore
	}
	return nil
}
