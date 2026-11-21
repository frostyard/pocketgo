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
		if i%4 == 3 {
			img.Pix[i] = 0xff // opaque, as a photo is
		}
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
// whatever type it was uploaded as; and with no note. A GIF goes as its
// first frame, made PNG or JPEG, and its note says so.
func TestAnImageThatFitsGoesAsItIs(t *testing.T) {
	limits := imageLimits{side: 20, base64: 1 << 20, pixels: 1000}
	for _, format := range []string{"png", "jpeg"} {
		data := encoded(t, flat(20, 10), format)
		got, note, err := prepareImage(data, limits)
		if err != nil || note != "" || got.MimeType != "image/"+format || !bytes.Equal(got.Data, data) {
			t.Errorf("%s: %s, %q, %v", format, got.MimeType, note, err)
		}
	}
	got, note, err := prepareImage(encoded(t, flat(20, 10), "gif"), limits)
	if err != nil || (got.MimeType != "image/png" && got.MimeType != "image/jpeg") || !strings.HasPrefix(note, "[Image: shown to you as ") {
		t.Errorf("gif: %s, %q, %v", got.MimeType, note, err)
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
	img := noise(64, 64)
	pngSize := len(encoded(t, img, "png"))
	limits = imageLimits{side: 80, base64: pngSize, pixels: 100000}
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

// An animation decodes its first frame alone: its later frames, however
// many fit in 5 MiB, take no memory (Codex review of #49, round 2).
func TestAnAnimationDecodesItsFirstFrameAlone(t *testing.T) {
	frame := image.NewPaletted(image.Rect(0, 0, 500, 500), color.Palette{color.White, color.Black})
	anim := &gif.GIF{}
	for range 200 {
		anim.Image = append(anim.Image, frame)
		anim.Delay = append(anim.Delay, 1)
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, anim); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, err := prepareImage(b.Bytes(), imageLimits{side: 2000, base64: 1 << 20, pixels: 1_000_000})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	// Every frame decoded would take 50 MB.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 10<<20 {
		t.Fatalf("the animation allocated %d bytes", allocated)
	}
}

// A GIF's first frame keeps its place on the GIF's canvas: the image the
// model gets is the canvas, and its note maps coordinates back to it
// (Codex review of #49, round 2).
func TestAGIFsFirstFrameKeepsItsCanvas(t *testing.T) {
	palette := color.Palette{color.White, color.Black}
	frame := image.NewPaletted(image.Rect(10, 5, 20, 10), palette)
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, &gif.GIF{Image: []*image.Paletted{frame}, Delay: []int{0},
		Config: image.Config{ColorModel: palette, Width: 40, Height: 20}}); err != nil {
		t.Fatal(err)
	}
	got, note, err := prepareImage(b.Bytes(), imageLimits{side: 20, base64: 1 << 20, pixels: 1000})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(got.Data))
	if err != nil || cfg.Width != 20 || cfg.Height != 10 || !strings.Contains(note, "original 40x20") {
		t.Fatalf("the frame went as %dx%d, %q, %v", cfg.Width, cfg.Height, note, err)
	}
}

// What a GIF's first frame leaves of its canvas shows the GIF's background
// color (Codex review of #49, round 3).
func TestAGIFsCanvasTakesItsBackground(t *testing.T) {
	palette := color.Palette{color.White, color.Black}
	frame := image.NewPaletted(image.Rect(10, 5, 20, 10), palette)
	for i := range frame.Pix {
		frame.Pix[i] = 1 // black
	}
	var b bytes.Buffer
	if err := gif.EncodeAll(&b, &gif.GIF{Image: []*image.Paletted{frame}, Delay: []int{0}, BackgroundIndex: 0,
		Config: image.Config{ColorModel: palette, Width: 40, Height: 20}}); err != nil {
		t.Fatal(err)
	}
	got, _, err := prepareImage(b.Bytes(), imageLimits{side: 40, base64: 1 << 20, pixels: 1000})
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(got.Data))
	if err != nil {
		t.Fatal(err)
	}
	if r, g, bl, a := img.At(0, 0).RGBA(); r < 0xf000 || g < 0xf000 || bl < 0xf000 || a != 0xffff {
		t.Fatalf("the canvas outside the frame is %v", img.At(0, 0))
	}
	if r, _, _, _ := img.At(15, 7).RGBA(); r > 0x1000 {
		t.Fatalf("the frame is %v", img.At(15, 7))
	}
}

// An image that is not opaque goes as PNG, never as JPEG, which drops its
// alpha: black lettering on a transparent ground would be black on black
// (Codex review of #49, round 7).
func TestATransparentImageGoesAsPNG(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 400, 200))
	r := rand.New(rand.NewPCG(3, 4))
	for i := 0; i < len(img.Pix); i += 4 {
		if r.IntN(3) == 0 { // black lettering, scattered
			img.Pix[i+3] = 0xff
		}
	}
	limits := imageLimits{side: 100, base64: 1 << 20, pixels: 100000}
	got, _, err := prepareImage(encoded(t, img, "png"), limits)
	if err != nil || got.MimeType != "image/png" {
		t.Fatalf("a transparent image went as %s, %v", got.MimeType, err)
	}
}
