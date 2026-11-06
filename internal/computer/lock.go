//go:build unix

package computer

import (
	"errors"
	"os"

	E "github.com/sagernet/sing/common/exceptions"

	"golang.org/x/sys/unix"
)

func acquire(path string, url string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return nil, E.Cause(err, "lock ", path)
		}
		return nil, E.New("another Swipt server is running on ", thisComputer, `; stop it with "swipt device stop -d `, url, `"`)
	}
	return file, nil
}
