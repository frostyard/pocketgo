package proto

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// tinyJPEG is a small JPEG with no EXIF.
func tinyJPEG(t testing.TB) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 8, 4)), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// exifEntry is one TIFF IFD entry.
type exifEntry struct {
	tag, kind uint16
	count     uint32
	value     [4]byte
}

// tiffOrder reads and appends in one byte order.
type tiffOrder interface {
	binary.ByteOrder
	binary.AppendByteOrder
}

// byteOrder is the byte order a TIFF header names: "II" or "MM".
func byteOrder(order string) tiffOrder {
	if order == "MM" {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

// exifSegment is an APP1 segment of EXIF whose first IFD holds entries,
// in the byte order order.
func exifSegment(order string, entries ...exifEntry) []byte {
	bo := byteOrder(order)
	tiff := []byte(order)
	tiff = bo.AppendUint16(tiff, 42)
	tiff = bo.AppendUint32(tiff, 8)
	tiff = bo.AppendUint16(tiff, uint16(len(entries)))
	for _, e := range entries {
		tiff = bo.AppendUint16(tiff, e.tag)
		tiff = bo.AppendUint16(tiff, e.kind)
		tiff = bo.AppendUint32(tiff, e.count)
		tiff = append(tiff, e.value[:]...)
	}
	tiff = bo.AppendUint32(tiff, 0) // no next IFD
	payload := append([]byte("Exif\x00\x00"), tiff...)
	return append([]byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}, payload...)
}

// orientationEntry is the orientation entry for value, as a SHORT (kind 3)
// or a LONG (kind 4), in order.
func orientationEntry(order string, kind uint16, value uint32) exifEntry {
	e := exifEntry{tag: 0x0112, kind: kind, count: 1}
	if kind == 3 {
		byteOrder(order).PutUint16(e.value[:], uint16(value))
	} else {
		byteOrder(order).PutUint32(e.value[:], value)
	}
	return e
}

// withSegments is data, a JPEG, with segments right after its start.
func withSegments(data []byte, segments ...[]byte) []byte {
	out := append([]byte{}, data[:2]...)
	for _, s := range segments {
		out = append(out, s...)
	}
	return append(out, data[2:]...)
}

// A JPEG's EXIF orientation is read from its first EXIF segment, in either
// byte order, as a SHORT or a LONG, after an XMP segment and other tags.
func TestJPEGOrientationReadsTheFirstEXIF(t *testing.T) {
	base := tinyJPEG(t)
	xmpName := "http://ns.adobe.com/xap/1.0/\x00"
	xmp := append([]byte{0xFF, 0xE1, 0x00, byte(len(xmpName) + 2)}, xmpName...)
	maker := exifEntry{tag: 0x010F, kind: 2, count: 4, value: [4]byte{'a', 'b', 'c', 0}}
	for _, order := range []string{"II", "MM"} {
		for _, kind := range []uint16{3, 4} {
			for o := uint32(1); o <= 8; o++ {
				data := withSegments(base, xmp, exifSegment(order, maker, orientationEntry(order, kind, o)))
				if got := JPEGOrientation(data); got != int(o) {
					t.Errorf("%s, kind %d, orientation %d: read %d", order, kind, o, got)
				}
				if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
					t.Fatalf("the test's JPEG does not decode: %v", err)
				}
			}
		}
	}
	// A second EXIF segment does not count.
	data := withSegments(base, exifSegment("II", maker), exifSegment("II", orientationEntry("II", 3, 6)))
	if got := JPEGOrientation(data); got != 1 {
		t.Errorf("a second EXIF segment's orientation was read: %d", got)
	}
	// Fill bytes and a marker with no segment before the EXIF are passed.
	six := exifSegment("MM", orientationEntry("MM", 3, 6))
	padded := append([]byte{0xFF, 0xD8, 0xFF, 0xFF, 0xFF, 0x01}, append(six, base[2:]...)...)
	if got := JPEGOrientation(padded); got != 6 {
		t.Errorf("after fill bytes and a TEM marker: %d", got)
	}
}

// Anything but a readable orientation of 1 to 8, before the first scan,
// is upright: no JPEG, no EXIF, a value out of range or of another type,
// a segment cut short, a TIFF header or IFD that does not hold.
func TestJPEGOrientationIsUprightWithoutOne(t *testing.T) {
	base := tinyJPEG(t)
	var p bytes.Buffer
	if err := png.Encode(&p, image.NewGray(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	six := exifSegment("MM", orientationEntry("MM", 3, 6))
	whole := withSegments(base, six)
	cut := whole[:2+len(six)-3]
	// Offsets in a segment: 4 bytes of marker and length, 6 of "Exif\0\0",
	// then the TIFF header: its order (2), magic (2) and IFD offset (4).
	badMagic := exifSegment("II", orientationEntry("II", 3, 6))
	badMagic[4+6+2] = 43
	farIFD := exifSegment("II", orientationEntry("II", 3, 6))
	binary.LittleEndian.PutUint32(farIFD[4+6+4:], 1<<30)
	manyEntries := exifSegment("II", orientationEntry("II", 3, 6))
	binary.LittleEndian.PutUint16(manyEntries[4+6+8:], 0xFFFF)
	manyEntries[4+6+10] = 0 // the first entry is no orientation, and the rest lie past the end
	sos := bytes.Index(base, []byte{0xFF, 0xDA})
	afterScan := append(append(append([]byte{}, base[:sos+2]...), 0x00, 0x02), six...)
	for name, data := range map[string][]byte{
		"empty":            nil,
		"a PNG":            p.Bytes(),
		"no EXIF":          base,
		"zero":             withSegments(base, exifSegment("II", orientationEntry("II", 3, 0))),
		"nine":             withSegments(base, exifSegment("II", orientationEntry("II", 3, 9))),
		"a BYTE":           withSegments(base, exifSegment("II", exifEntry{tag: 0x0112, kind: 1, count: 1, value: [4]byte{6}})),
		"two values":       withSegments(base, exifSegment("II", exifEntry{tag: 0x0112, kind: 3, count: 2, value: [4]byte{6, 0, 6, 0}})),
		"cut short":        cut,
		"a wrong magic":    withSegments(base, badMagic),
		"an IFD past it":   withSegments(base, farIFD),
		"entries past it":  withSegments(base, manyEntries),
		"after the scan":   afterScan,
		"no marker":        append([]byte{0xFF, 0xD8, 0x00}, six...),
		"a length of zero": append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x00}, six...),
	} {
		if got := JPEGOrientation(data); got != 1 {
			t.Errorf("%s: orientation %d", name, got)
		}
	}
}

// Whatever its bytes, a JPEG's orientation is 1 to 8, and reading it never
// panics.
func FuzzJPEGOrientation(f *testing.F) {
	base := tinyJPEG(f)
	f.Add(base)
	f.Add(withSegments(base, exifSegment("II", orientationEntry("II", 3, 6))))
	f.Add(withSegments(base, exifSegment("MM", orientationEntry("MM", 4, 8))))
	f.Fuzz(func(t *testing.T, data []byte) {
		if o := JPEGOrientation(data); o < 1 || o > 8 {
			t.Fatalf("orientation %d", o)
		}
	})
}

// Upright turns and mirrors an image as EXIF says: each orientation names
// the sides the stored first row and first column show (EXIF 2.32, tag
// 0x0112). The corners are checked against those sides, not against the
// turn's own arithmetic.
func TestUprightTurnsAsEXIFSays(t *testing.T) {
	const w, h = 3, 2
	stored := image.NewRGBA(image.Rect(0, 0, w, h))
	red, green, blue := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}, color.RGBA{0, 0, 255, 255}
	stored.Set(0, 0, red)     // the first row's first pixel
	stored.Set(w-1, 0, green) // the first row's last
	stored.Set(0, h-1, blue)  // the first column's last
	opposite := map[string]string{"top": "bottom", "bottom": "top", "left": "right", "right": "left"}
	for o, sides := range map[int][2]string{
		1: {"top", "left"}, 2: {"top", "right"}, 3: {"bottom", "right"}, 4: {"bottom", "left"},
		5: {"left", "top"}, 6: {"right", "top"}, 7: {"right", "bottom"}, 8: {"left", "bottom"},
	} {
		row, column := sides[0], sides[1]
		got := Upright(stored, o)
		uw, uh := w, h
		if row == "left" || row == "right" {
			uw, uh = h, w
		}
		if b := got.Bounds(); b.Dx() != uw || b.Dy() != uh {
			t.Errorf("orientation %d: %dx%d, want %dx%d", o, b.Dx(), b.Dy(), uw, uh)
			continue
		}
		if sw, sh := UprightSize(w, h, o); sw != uw || sh != uh {
			t.Errorf("UprightSize(%d) = %dx%d, want %dx%d", o, sw, sh, uw, uh)
		}
		// The corner where two sides meet, in the upright image.
		corner := func(a, b string) image.Point {
			var p image.Point
			for _, side := range []string{a, b} {
				switch side {
				case "right":
					p.X = uw - 1
				case "bottom":
					p.Y = uh - 1
				}
			}
			return p
		}
		for _, c := range []struct {
			name string
			at   image.Point
			want color.RGBA
		}{
			{"the first row's first pixel", corner(row, column), red},
			{"the first row's last", corner(row, opposite[column]), green},
			{"the first column's last", corner(opposite[row], column), blue},
		} {
			if px := color.RGBAModel.Convert(got.At(c.at.X, c.at.Y)); px != c.want {
				t.Errorf("orientation %d: %s is %v at %v, want %v", o, c.name, px, c.at, c.want)
			}
		}
	}
	// Upright, or out of range, the image is the same.
	for _, o := range []int{0, 1, 9} {
		if Upright(stored, o) != image.Image(stored) {
			t.Errorf("orientation %d turned the image", o)
		}
	}
	// An image of another kind, or not at the origin, turns the same.
	gray := image.NewGray(image.Rect(10, 20, 13, 22))
	gray.SetGray(10, 20, color.Gray{200})
	if got := Upright(gray, 6); got.Bounds() != image.Rect(0, 0, 2, 3) || color.GrayModel.Convert(got.At(1, 0)).(color.Gray).Y != 200 {
		t.Errorf("a gray image off the origin turned to %v, %v at the top right", got.Bounds(), got.At(1, 0))
	}
	offset := image.NewRGBA(image.Rect(5, 5, 8, 7))
	offset.Set(5, 5, red)
	if got := Upright(offset, 3); color.RGBAModel.Convert(got.At(2, 1)) != red {
		t.Errorf("an RGBA image off the origin turned with %v at the bottom right", got.At(2, 1))
	}
}
