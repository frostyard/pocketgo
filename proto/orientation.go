package proto

import (
	"encoding/binary"
	"image"
	"image/draw"
)

// The orientation of an attached photo (rule 16a): pocketd turns a JPEG
// upright by its EXIF orientation before a model gets it, since models do
// not read EXIF, and so does its fake in the desktop's tests, so those
// tests charge an image what pocketd sends.

// exifOrientationTag is the TIFF tag of the EXIF orientation.
const exifOrientationTag = 0x0112

// JPEGOrientation is the EXIF orientation a JPEG names, 1 to 8: how its
// stored pixels turn and mirror to stand as a viewer shows them. It is 1,
// upright as stored, for data that is no JPEG, has no EXIF before its
// first scan, or names no orientation or one out of range, and for EXIF it
// cannot read whole. Only the first EXIF segment counts.
func JPEGOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	for i := 2; i+1 < len(data); {
		if data[i] != 0xFF {
			return 1
		}
		marker := data[i+1]
		switch {
		case marker == 0xFF: // a fill byte before the marker
			i++
			continue
		case marker == 0xD9 || marker == 0xDA: // the image's end, or its first scan: no EXIF past it
			return 1
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7): // markers with no segment
			i += 2
			continue
		}
		if i+4 > len(data) {
			return 1
		}
		length := int(data[i+2])<<8 | int(data[i+3])
		if length < 2 || i+2+length > len(data) {
			return 1
		}
		segment := data[i+4 : i+2+length]
		if marker == 0xE1 && len(segment) >= 6 && string(segment[:6]) == "Exif\x00\x00" {
			return tiffOrientation(segment[6:])
		}
		i += 2 + length
	}
	return 1
}

// tiffOrientation is the orientation the first IFD of an EXIF segment's
// TIFF structure names: a SHORT or a LONG of one value, 1 to 8; else 1.
func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(tiff[2:]) != 42 {
		return 1
	}
	ifd := uint64(order.Uint32(tiff[4:]))
	if ifd+2 > uint64(len(tiff)) {
		return 1
	}
	entries := uint64(order.Uint16(tiff[ifd:]))
	for k := range entries {
		at := ifd + 2 + k*12
		if at+12 > uint64(len(tiff)) {
			return 1
		}
		entry := tiff[at : at+12]
		if order.Uint16(entry) != exifOrientationTag {
			continue
		}
		var value uint32
		switch kind, count := order.Uint16(entry[2:]), order.Uint32(entry[4:]); {
		case kind == 3 && count == 1: // SHORT, left in the value's field
			value = uint32(order.Uint16(entry[8:]))
		case kind == 4 && count == 1: // LONG
			value = order.Uint32(entry[8:])
		default:
			return 1
		}
		if value < 1 || value > 8 {
			return 1
		}
		return int(value)
	}
	return 1
}

// Upright is img turned and mirrored as EXIF orientation says, so that
// its first row is its top and its first column its left side, as a
// viewer shows it: orientations 5 to 8 swap its sides. It is img itself
// for 1, or a value out of 1 to 8; else a new image of img's size, which
// a caller turns after it shrinks one, to keep its memory near the
// smaller image's.
func Upright(img image.Image, orientation int) image.Image {
	if orientation < 2 || orientation > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	src, ok := img.(*image.RGBA)
	if !ok {
		src = image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(src, src.Rect, img, b.Min, draw.Src)
	}
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range h {
		for x := range w {
			// Where the stored pixel (x, y) stands upright: EXIF names the
			// sides the stored first row and first column show.
			var dx, dy int
			switch orientation {
			case 2: // first row the top, first column the right side
				dx, dy = w-1-x, y
			case 3: // the bottom, the right side
				dx, dy = w-1-x, h-1-y
			case 4: // the bottom, the left side
				dx, dy = x, h-1-y
			case 5: // the left side, the top
				dx, dy = y, x
			case 6: // the right side, the top
				dx, dy = h-1-y, x
			case 7: // the right side, the bottom
				dx, dy = h-1-y, w-1-x
			case 8: // the left side, the bottom
				dx, dy = y, w-1-x
			}
			s := src.PixOffset(src.Rect.Min.X+x, src.Rect.Min.Y+y)
			d := dst.PixOffset(dx, dy)
			copy(dst.Pix[d:d+4], src.Pix[s:s+4])
		}
	}
	return dst
}

// UprightSize is the size of a w×h image turned upright by orientation:
// orientations 5 to 8 swap its sides.
func UprightSize(w, h, orientation int) (int, int) {
	if orientation >= 5 && orientation <= 8 {
		return h, w
	}
	return w, h
}
