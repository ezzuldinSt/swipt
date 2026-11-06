//go:build !darwin || !cgo

package automation

import (
	"slices"
	"strings"
)

// preferredLocalizations matches like Bundle.preferredLocalizations(from:forPreferences:)
// without CoreFoundation: the first preference that an available localization
// serves picks it and its less specific parents, most specific first; a
// Chinese preference without a script takes the script of its region. When
// none matches, English or else the first localization is taken.
func preferredLocalizations(localizations []string, preferences []string) []string {
	if len(localizations) == 0 {
		return []string{}
	}
	for _, preference := range preferences {
		var matched []string
		for _, candidate := range fallbacks(preference) {
			for _, localization := range localizations {
				if strings.EqualFold(normalizeLocale(localization), candidate) && !slices.Contains(matched, localization) {
					matched = append(matched, localization)
				}
			}
		}
		if len(matched) > 0 {
			return matched
		}
	}
	for _, localization := range localizations {
		if language, _, _ := strings.Cut(normalizeLocale(localization), "-"); strings.EqualFold(language, "en") {
			return []string{localization}
		}
	}
	return []string{localizations[0]}
}

// fallbacks lists a preference and its parents, as zh-Hans-CN, zh-Hans, zh.
func fallbacks(preference string) []string {
	parts := strings.Split(normalizeLocale(preference), "-")
	if strings.EqualFold(parts[0], "zh") && (len(parts) < 2 || len(parts[1]) != 4) {
		script := "Hans"
		if len(parts) > 1 {
			switch strings.ToUpper(parts[1]) {
			case "TW", "HK", "MO":
				script = "Hant"
			}
		}
		parts = append([]string{parts[0], script}, parts[1:]...)
	}
	var out []string
	for length := len(parts); length > 0; length-- {
		out = append(out, strings.Join(parts[:length], "-"))
	}
	return out
}

func normalizeLocale(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), "_", "-")
}
