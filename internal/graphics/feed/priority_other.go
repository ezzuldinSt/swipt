//go:build !windows

package feed

// raiseThreadPriority leaves the thread as it is off Windows.
func raiseThreadPriority() (string, func()) {
	return "", func() {}
}
