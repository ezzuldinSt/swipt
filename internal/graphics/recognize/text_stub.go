//go:build !windows

package recognize

import "context"

func Languages() ([]string, error) {
	return nil, unsupported()
}

func ReadText(ctx context.Context, img Image, options TextOptions) (*Text, error) {
	return nil, unsupported()
}

func unsupported() error {
	return &Error{Name: "NotSupportedError", Message: "graphics: text recognition is not available on this platform"}
}
