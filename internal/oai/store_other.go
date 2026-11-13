//go:build !darwin && !linux && !windows

package oai

import (
	"context"
	"strconv"
)

type nativeStore struct{}

func stringIndex(i int) string                                  { return strconv.Itoa(i) }
func (nativeStore) Get(context.Context, string) (string, error) { return "", errStore }
func (nativeStore) Set(context.Context, string, string) error   { return errStore }
func (nativeStore) Delete(context.Context, string) error        { return errStore }
