package raster

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"io"
	"math"
)

// metadata is what ImageIO reads besides the pixels: kCGImagePropertyOrientation,
// kCGImagePropertyDPIWidth and the ICC profile.
type metadata struct {
	orientation int
	dpi         float64
	profile     []byte
}

const pngSignature = "\x89PNG\r\n\x1a\n"

func readMetadata(data []byte) metadata {
	switch {
	case bytes.HasPrefix(data, []byte(pngSignature)):
		return pngMetadata(data)
	case len(data) > 3 && data[0] == 0xff && data[1] == 0xd8:
		return jpegMetadata(data)
	}
	return metadata{}
}

func pngMetadata(data []byte) metadata {
	var found metadata
	for offset := len(pngSignature); offset+8 <= len(data); {
		length := int(binary.BigEndian.Uint32(data[offset:]))
		kind := string(data[offset+4 : offset+8])
		start := offset + 8
		if length < 0 || start+length > len(data) || kind == "IDAT" {
			break
		}
		chunk := data[start : start+length]
		switch kind {
		case "pHYs":
			// Pixels per meter; ImageIO reports whole dots per inch.
			if len(chunk) == 9 && chunk[8] == 1 {
				found.dpi = math.Round(float64(binary.BigEndian.Uint32(chunk)) * 0.0254)
			}
		case "iCCP":
			if separator := bytes.IndexByte(chunk, 0); separator >= 0 && separator+2 <= len(chunk) && chunk[separator+1] == 0 {
				if reader, err := zlib.NewReader(bytes.NewReader(chunk[separator+2:])); err == nil {
					found.profile, _ = io.ReadAll(io.LimitReader(reader, 16<<20))
				}
			}
		case "eXIf":
			found.orientation, _ = exifFields(chunk)
		}
		offset = start + length + 4
	}
	return found
}

func jpegMetadata(data []byte) metadata {
	var found metadata
	var jfif, exif float64
	var profile [][]byte
	for offset := 2; offset+4 <= len(data); {
		if data[offset] != 0xff {
			break
		}
		marker := data[offset+1]
		if marker == 0xff {
			offset++
			continue
		}
		if marker == 0xd8 || (marker >= 0xd0 && marker <= 0xd7) || marker == 0x01 {
			offset += 2
			continue
		}
		if marker == 0xda || marker == 0xd9 {
			break
		}
		length := int(binary.BigEndian.Uint16(data[offset+2:]))
		if length < 2 || offset+2+length > len(data) {
			break
		}
		segment := data[offset+4 : offset+2+length]
		switch {
		case marker == 0xe0 && len(segment) >= 14 && string(segment[:5]) == "JFIF\x00":
			density := float64(binary.BigEndian.Uint16(segment[8:]))
			switch segment[7] {
			case 1:
				jfif = density
			case 2:
				jfif = math.Round(density * 2.54)
			}
		case marker == 0xe1 && len(segment) > 6 && string(segment[:6]) == "Exif\x00\x00":
			found.orientation, exif = exifFields(segment[6:])
		case marker == 0xe2 && len(segment) > 14 && string(segment[:12]) == "ICC_PROFILE\x00":
			sequence, count := int(segment[12]), int(segment[13])
			if count > 0 && sequence >= 1 && sequence <= count {
				if profile == nil {
					profile = make([][]byte, count)
				}
				if sequence <= len(profile) {
					profile[sequence-1] = segment[14:]
				}
			}
		}
		offset += 2 + length
	}
	found.dpi = jfif
	if found.dpi == 0 {
		found.dpi = exif
	}
	if len(profile) > 0 {
		found.profile = bytes.Join(profile, nil)
	}
	return found
}

// exifFields reads the orientation and the horizontal resolution in dots per inch from a TIFF
// structure of EXIF tags.
func exifFields(tiff []byte) (orientation int, dpi float64) {
	if len(tiff) < 8 {
		return 0, 0
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0, 0
	}
	directory := int(order.Uint32(tiff[4:]))
	if directory < 8 || directory+2 > len(tiff) {
		return 0, 0
	}
	count := int(order.Uint16(tiff[directory:]))
	var resolution float64
	unit := 2
	for index := range count {
		entry := directory + 2 + index*12
		if entry+12 > len(tiff) {
			break
		}
		tag := order.Uint16(tiff[entry:])
		value := tiff[entry+8 : entry+12]
		switch tag {
		case 0x0112:
			orientation = int(order.Uint16(value))
		case 0x0128:
			unit = int(order.Uint16(value))
		case 0x011a:
			at := int(order.Uint32(value))
			if at >= 0 && at+8 <= len(tiff) {
				if denominator := order.Uint32(tiff[at+4:]); denominator != 0 {
					resolution = float64(order.Uint32(tiff[at:])) / float64(denominator)
				}
			}
		}
	}
	switch unit {
	case 2:
		dpi = math.Round(resolution)
	case 3:
		dpi = math.Round(resolution * 2.54)
	}
	if orientation < 1 || orientation > 8 {
		orientation = 0
	}
	return orientation, dpi
}

// withPhysical adds the pHYs chunk that carries the density, right after IHDR.
func withPhysical(encoded []byte, dpi float64) []byte {
	const afterHeader = len(pngSignature) + 8 + 13 + 4
	if len(encoded) < afterHeader || dpi <= 0 {
		return encoded
	}
	perMeter := uint32(math.Round(dpi / 0.0254))
	chunk := make([]byte, 0, 21)
	chunk = binary.BigEndian.AppendUint32(chunk, 9)
	chunk = append(chunk, "pHYs"...)
	chunk = binary.BigEndian.AppendUint32(chunk, perMeter)
	chunk = binary.BigEndian.AppendUint32(chunk, perMeter)
	chunk = append(chunk, 1)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	out := make([]byte, 0, len(encoded)+len(chunk))
	out = append(out, encoded[:afterHeader]...)
	out = append(out, chunk...)
	return append(out, encoded[afterHeader:]...)
}

// withDensity adds the JFIF segment that carries the density, right after SOI.
func withDensity(encoded []byte, dpi float64) []byte {
	if len(encoded) < 2 {
		return encoded
	}
	density := uint16(min(max(math.Round(dpi), 1), math.MaxUint16))
	segment := []byte{0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x01}
	segment = binary.BigEndian.AppendUint16(segment, density)
	segment = binary.BigEndian.AppendUint16(segment, density)
	segment = append(segment, 0, 0)
	out := make([]byte, 0, len(encoded)+len(segment))
	out = append(out, encoded[:2]...)
	out = append(out, segment...)
	return append(out, encoded[2:]...)
}
