//go:build !windows && (!darwin || !cgo)

package movie

import "os"

func openShared(path string) (*os.File, error) {
	return os.Open(path)
}
