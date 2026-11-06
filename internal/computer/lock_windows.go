package computer

import (
	"errors"
	"os"

	E "github.com/sagernet/sing/common/exceptions"

	"golang.org/x/sys/windows"
)

func acquire(path string, url string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if err != nil {
		_ = file.Close()
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, E.Cause(err, "lock ", path)
		}
		return nil, E.New("another Swipt server is running on ", thisComputer, `; stop it with "swipt device stop -d `, url, `"`)
	}
	return file, nil
}
