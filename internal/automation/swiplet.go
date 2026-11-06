package automation

import (
	"slices"
	"strconv"

	E "github.com/sagernet/sing/common/exceptions"
)

const (
	SourceFolder  = "src"
	AssetsFolder  = "assets"
	DataFolder    = "data"
	InterfaceFile = "app.js"
	ExtensionFile = "extension.js"
	ExtensionMap  = "extension.js.map"
	ManifestFile  = "manifest.json"
	IconFile      = "icon.png"
	serviceFile   = ".service"
	longestID     = 128
)

var iconColors = []string{"red", "orange", "tangerine", "yellow", "lime", "teal", "cyan", "blue", "navy", "grape", "purple", "pink", "grayBlue", "grayGreen", "grayBrown"}

var DefaultIcon = Icon{Color: "grayBrown", Symbol: "square.2.layers.3d.fill"}

type Icon struct {
	Color  string `json:"color"`
	Symbol string `json:"symbol"`
}

func (i Icon) Check() error {
	if !slices.Contains(iconColors, i.Color) {
		return E.New("unknown color ", strconv.Quote(i.Color))
	}
	if i.Symbol == "" {
		return E.New("the symbol is empty")
	}
	return nil
}

func LoadIcon(directory string) Icon {
	manifest, err := LoadManifest(directory)
	if err != nil {
		return DefaultIcon
	}
	return manifest.Icon
}

func CheckID(id string) error {
	valid := id != "" && len(id) <= longestID
	for index, char := range id {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
		case index > 0 && (char == '.' || char == '_' || char == '-'):
		default:
			valid = false
		}
	}
	if !valid {
		return E.Cause1(ErrInvalid, E.New("the swiplet ID ", strconv.Quote(id), " is not 1 to ", longestID, " lowercase letters, digits, dots, underscores and hyphens starting with a letter or a digit"))
	}
	return nil
}
