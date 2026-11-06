//go:build !(darwin && cgo)

package computer

import "os"

func Name() string {
	name, _ := os.Hostname()
	return name
}

func LocalHostName() string {
	return Name()
}

func Languages() []string {
	return languages()
}
