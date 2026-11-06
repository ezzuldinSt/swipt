package computer

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
)

const settingsFile = "server.json"

const (
	lowestPort  = 10000
	highestPort = 49151
)

type settings struct {
	Port int `json:"port"`
}

func loadSettings(directory string) (settings, error) {
	var loaded settings
	data, err := os.ReadFile(filepath.Join(directory, settingsFile))
	if err == nil {
		err = json.Unmarshal(data, &loaded)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return settings{}, err
	}
	if loaded.Port >= lowestPort && loaded.Port <= highestPort {
		return loaded, nil
	}
	loaded.Port = randomPort()
	return loaded, loaded.save(directory)
}

func (s settings) save(directory string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, settingsFile), append(data, '\n'), 0o600)
}

func randomPort() int {
	offset, _ := rand.Int(rand.Reader, big.NewInt(highestPort-lowestPort+1))
	return lowestPort + int(offset.Int64())
}
