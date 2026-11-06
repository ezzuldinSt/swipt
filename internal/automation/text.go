package automation

import (
	"strings"
	"unicode/utf8"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/jsc"
)

func (e *engine) encodeText(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 1)
	if err != nil {
		return nil, err
	}
	return jsc.Bytes(values[0]), nil
}

func (e *engine) decodeText(arguments []jsc.Value) (any, error) {
	if len(arguments) < 2 {
		return nil, E.New("expected the data and fatal")
	}
	data, isBytes := arguments[0].Bytes()
	if !isBytes {
		return nil, E.New("the data is not a Uint8Array")
	}
	if utf8.Valid(data) {
		return string(data), nil
	}
	text, restored, ok := decodeUTF8(data, arguments[1].Number() != 0)
	if !ok {
		return restored, nil
	}
	return text, nil
}

func decodeUTF8(data []byte, fatal bool) (text string, restored int, ok bool) {
	var out strings.Builder
	if !fatal {
		out.Grow(len(data))
	}
	needed, seen := 0, 0
	var point rune
	lower, upper := byte(0x80), byte(0xbf)
	for index := 0; index < len(data); {
		b := data[index]
		if needed == 0 {
			index++
			switch {
			case b < 0x80:
				out.WriteByte(b)
			case b >= 0xc2 && b <= 0xdf:
				needed, point = 1, rune(b&0x1f)
			case b >= 0xe0 && b <= 0xef:
				lower, upper = 0x80, 0xbf
				switch b {
				case 0xe0:
					lower = 0xa0
				case 0xed:
					upper = 0x9f
				}
				needed, point = 2, rune(b&0x0f)
			case b >= 0xf0 && b <= 0xf4:
				lower, upper = 0x80, 0xbf
				switch b {
				case 0xf0:
					lower = 0x90
				case 0xf4:
					upper = 0x8f
				}
				needed, point = 3, rune(b&0x07)
			default:
				if fatal {
					return "", -1, false
				}
				out.WriteRune(utf8.RuneError)
			}
			continue
		}
		if b < lower || b > upper {
			if fatal {
				return "", index, false
			}
			needed, seen, point = 0, 0, 0
			lower, upper = 0x80, 0xbf
			out.WriteRune(utf8.RuneError)
			continue
		}
		index++
		lower, upper = 0x80, 0xbf
		point = point<<6 | rune(b&0x3f)
		if seen++; seen < needed {
			continue
		}
		out.WriteRune(point)
		needed, seen, point = 0, 0, 0
	}
	if needed != 0 {
		if fatal {
			return "", -1, false
		}
		out.WriteRune(utf8.RuneError)
	}
	return out.String(), 0, true
}
