package uploads

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
)

func encoded(t *testing.T, img image.Image, format string) []byte {
	t.Helper()
	var b bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&b, img)
	case "jpeg":
		err = jpeg.Encode(&b, img, &jpeg.Options{Quality: 95})
	case "gif":
		err = gif.Encode(&b, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// flat is a w×h image of one color, which compresses to almost nothing.
func flat(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 0x80
	}
	return img
}

// noise is a w×h image of random pixels, which compresses badly.
func noise(w, h int) image.Image {
	r := rand.New(rand.NewPCG(1, 2))
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(r.IntN(256))
	}
	return img
}

// A PNG, JPEG, GIF or WebP of at most 5 MiB goes to a model; other types
// and larger files are only named.
func TestForModel(t *testing.T) {
	for _, c := range []struct {
		mime string
		size int64
		want bool
	}{
		{"image/png", MaxInlineImage, true},
		{"image/jpeg", 1, true},
		{"image/gif", 1, true},
		{"image/webp", 1, true},
		{"image/png", MaxInlineImage + 1, false},
		{"image/heic", 1, false},
		{"image/bmp", 1, false},
		{"application/pdf", 1, false},
	} {
		if got := ForModel(c.mime, c.size); got != c.want {
			t.Errorf("ForModel(%q, %d) = %v", c.mime, c.size, got)
		}
	}
}

// An image within the limits goes as it is, with the type its bytes say,
// whatever type it was uploaded as; and with no note.
func TestAnImageThatFitsGoesAsItIs(t *testing.T) {
	limits := imageLimits{side: 20, base64: 1 << 20, pixels: 1000}
	for _, format := range []string{"png", "jpeg", "gif"} {
		data := encoded(t, flat(20, 10), format)
		got, note, err := prepareImage(data, limits)
		if err != nil || note != "" || got.MimeType != "image/"+format || !bytes.Equal(got.Data, data) {
			t.Errorf("%s: %s, %q, %v", format, got.MimeType, note, err)
		}
	}
}

// A larger image is fitted within the side, its proportions kept, and its
// note says the size it was and the factor back to it; one whose PNG is
// too large goes as JPEG, and its note says so too.
func TestALargerImageIsFittedAndSaysHow(t *testing.T) {
	limits := imageLimits{side: 20, base64: 1 << 20, pixels: 100000}
	got, note, err := prepareImage(encoded(t, flat(80, 40), "png"), limits)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(got.Data))
	if err != nil || cfg.Width != 20 || cfg.Height != 10 || "image/"+format != got.MimeType {
		t.Fatalf("fitted to %dx%d %s (%s), %v", cfg.Width, cfg.Height, format, got.MimeType, err)
	}
	if want := "[Image: original 80x40, displayed at 20x10. Multiply coordinates by 4.00 to map to original image.]"; note != want && note != "[Image: shown to you as JPEG.] "+want {
		t.Fatalf("note %q", note)
	}

	// Noise whose PNG is above the limit, and whose JPEG is below it.
	img := noise(16, 16)
	pngSize := len(encoded(t, img, "png"))
	limits = imageLimits{side: 20, base64: pngSize, pixels: 100000}
	got, note, err = prepareImage(encoded(t, img, "png"), limits)
	if err != nil || got.MimeType != "image/jpeg" || note != "[Image: shown to you as JPEG.]" {
		t.Fatalf("noise went as %s, %q, %v", got.MimeType, note, err)
	}
}

// An image that cannot go says why: not an image, a format a model does
// not take, too many pixels (read from its header alone), or none small
// enough.
func TestAnImageThatCannotGoSaysWhy(t *testing.T) {
	limits := imageLimits{side: 20, base64: 1 << 20, pixels: 1000}
	if _, _, err := prepareImage([]byte("%PDF-1.7 not an image"), limits); !errors.Is(err, ErrUndecodable) {
		t.Errorf("a PDF: %v", err)
	}
	bmp := append([]byte("BM"), make([]byte, 60)...)
	if _, _, err := prepareImage(bmp, limits); !errors.Is(err, ErrUndecodable) {
		t.Errorf("a BMP: %v", err)
	}
	// A PNG header that claims 100000×100000 pixels and holds none.
	if _, _, err := PrepareImage(pngHeader(100000, 100000)); !errors.Is(err, ErrTooManyPx) {
		t.Errorf("a huge PNG: %v", err)
	}
	// Noise that no size within the limit makes small enough.
	if _, _, err := prepareImage(encoded(t, noise(40, 40), "png"), imageLimits{side: 20, base64: 8, pixels: 100000}); !errors.Is(err, ErrUnfit) {
		t.Errorf("noise below 8 bytes: %v", err)
	}
	if got := NotShown(ErrTooManyPx); got != "[Image: not shown to you: it has more than 50000000 pixels.]" {
		t.Errorf("NotShown = %q", got)
	}
	if got := NotShown(errors.New("two\nlines.")); got != "[Image: not shown to you: two lines.]" {
		t.Errorf("NotShown keeps its note on one line: %q", got)
	}
}

// pngHeader is a PNG's signature and IHDR chunk for w×h, with no pixels.
func pngHeader(w, h uint32) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6 // 8 bits, RGBA
	_ = binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

// A model's whole limits fit a large screenshot to 2000 pixels a side.
func TestTheModelsLimits(t *testing.T) {
	img := image.NewPaletted(image.Rect(0, 0, 4000, 1000), color.Palette{color.White, color.Black})
	got, note, err := PrepareImage(encoded(t, img, "png"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got.Data))
	if err != nil || cfg.Width != 2000 || cfg.Height != 500 || !strings.Contains(note, "Multiply coordinates by 2.00") {
		t.Fatalf("%dx%d, %q, %v", cfg.Width, cfg.Height, note, err)
	}
}

// A resize keeps its memory near the image's own: a narrow, tall image
// would take x/image's kernels a float buffer of the target's width by
// its whole height (Codex review of #49, round 1).
func TestAResizeKeepsItsMemoryBounded(t *testing.T) {
	tall := encoded(t, image.NewGray(image.Rect(0, 0, 4, 400_000)), "png")
	limits := imageLimits{side: 20, base64: 1 << 20, pixels: 2_000_000}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, _, err := prepareImage(tall, limits)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(got.Data)); err != nil || cfg.Width != 1 || cfg.Height != 20 {
		t.Fatalf("fitted to %+v, %v", cfg, err)
	}
	// The decoded image is 1.6 MB; CatmullRom took over 25 MB.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 10<<20 {
		t.Fatalf("the resize allocated %d bytes", allocated)
	}
}

// An image whose header reads well but whose pixels do not is not shown,
// even one small enough to go as it is: the provider would refuse the
// whole message (Codex review of #49, round 1).
func TestATruncatedImageIsNotShown(t *testing.T) {
	limits := imageLimits{side: 2000, base64: 1 << 20, pixels: 1 << 20}
	for _, format := range []string{"png", "jpeg", "gif"} {
		whole := encoded(t, noise(64, 64), format)
		if _, _, err := prepareImage(whole, limits); err != nil {
			t.Fatalf("whole %s: %v", format, err)
		}
		if _, _, err := prepareImage(whole[:len(whole)/2], limits); !errors.Is(err, ErrUndecodable) {
			t.Errorf("half a %s went: %v", format, err)
		}
	}
}

// Each side of a fitted image is rounded on its own, so the note gives
// each its factor when they differ (Codex review of #49, round 1).
func TestEachSideHasItsFactor(t *testing.T) {
	_, note, err := prepareImage(encoded(t, flat(1, 40), "png"), imageLimits{side: 20, base64: 1 << 20, pixels: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if want := "Multiply x coordinates by 1.00 and y coordinates by 2.00 to map to original image."; !strings.Contains(note, want) {
		t.Fatalf("note %q", note)
	}
}
