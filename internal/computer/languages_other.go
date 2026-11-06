//go:build !windows && !(darwin && cgo)

package computer

import (
	"os"
	"strings"
)

// languages reads the POSIX locale variables, as en_US.UTF-8 becomes en-US.
func languages() []string {
	var out []string
	for _, name := range []string{"LANGUAGE", "LC_ALL", "LC_MESSAGES", "LANG"} {
		for _, locale := range strings.Split(os.Getenv(name), ":") {
			locale, _, _ = strings.Cut(locale, ".")
			locale, _, _ = strings.Cut(locale, "@")
			if locale == "" || locale == "C" || locale == "POSIX" {
				continue
			}
			out = append(out, strings.ReplaceAll(locale, "_", "-"))
		}
		if len(out) > 0 {
			return out
		}
	}
	return out
}
