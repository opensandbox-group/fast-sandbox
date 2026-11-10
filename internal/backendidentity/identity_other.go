//go:build !linux

package backendidentity

import (
	"context"
	"errors"
)

func Attest(context.Context, string, string) error {
	return errors.New("backend qualification requires Linux")
}
func Verify(context.Context, string, string) error {
	return errors.New("backend qualification requires Linux")
}
