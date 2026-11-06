package automation

import (
	"os"
	"path/filepath"

	E "github.com/sagernet/sing/common/exceptions"
)

func writeAtomically(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	err = E.Errors(err, file.Close())
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		_ = os.Remove(file.Name())
	}
	return err
}
