//go:build !windows

package dnssd

import "context"

func Browse(ctx context.Context, service string, found func(name string)) error {
	return ErrUnsupported
}

func Resolve(ctx context.Context, name string) (Instance, error) {
	return Instance{}, ErrUnsupported
}
