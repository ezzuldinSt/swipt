package computer

import "golang.org/x/sys/windows"

func languages() []string {
	names, err := windows.GetUserPreferredUILanguages(windows.MUI_LANGUAGE_NAME)
	if err != nil {
		return nil
	}
	return names
}
