package uploads

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// The limits of an image a model gets, pi-coding-agent's (pi-pocket's
// model-images.ts): a file of at most MaxInlineImage bytes goes, at most
// MaxSide pixels a side and below MaxBase64 bytes of base64. MaxPixels
// bounds what pocketd decodes at all, so one upload cannot take its
// memory.
const (
	MaxInlineImage = 5 << 20
	MaxSide        = 2000
	MaxBase64      = 4.5 * 1024 * 1024
	MaxPixels      = 50_000_000
)

// jpegQualities are the qualities a fitted image is tried at as JPEG, in
// order: the first beside PNG, the smaller of the two first.
var jpegQualities = []int{80, 70, 55, 40}

// formats are the image formats a model takes inline, by the name Go's
// decoders give them, with their types.
var formats = map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}

// ForModel says whether an upload of type mime and size bytes goes to a
// model that takes images as an image too.
func ForModel(mime string, size int64) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return size <= MaxInlineImage
	}
	return false
}

// Image is an image as a model gets it.
type Image struct {
	Data     []byte
	MimeType string
}

// Why an image does not go to the model; each is the end of the note on
// its file's line ([Image: not shown to you: <why>.]).
var (
	ErrUndecodable = errors.New("it is not an image pocketd can decode")
	ErrTooManyPx   = errors.New("it has more than " + strconv.Itoa(MaxPixels) + " pixels")
	ErrUnfit       = errors.New("it could not be made small enough")
)

// prepareMu makes one image at a time: decoding one takes up to hundreds
// of megabytes for a moment (pi-pocket made two at once for that reason).
var prepareMu sync.Mutex

// PrepareImage is data as the model gets it, with the note for its file's
// line: "" when it goes as it is. Its format is the one its bytes have, a
// PNG, JPEG, GIF or WebP. One within MaxSide a side and below MaxBase64 of
// base64 goes as it is; another is fitted within MaxSide a side, as PNG or
// JPEG at the first quality, the smaller first, then as JPEG at the lower
// ones, then a quarter smaller each time, until it is below MaxBase64. The
// note says when its format and when its size changed, as
// pi-coding-agent's do.
func PrepareImage(data []byte) (Image, string, error) {
	return prepareImage(data, imageLimits{side: MaxSide, base64: MaxBase64, pixels: MaxPixels})
}

// imageLimits are the limits PrepareImage fits an image to; tests use
// small ones.
type imageLimits struct{ side, base64, pixels int }

func prepareImage(data []byte, limits imageLimits) (Image, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	mime, ok := formats[format]
	if err != nil || !ok {
		return Image{}, "", ErrUndecodable
	}
	if int64(cfg.Width)*int64(cfg.Height) > int64(limits.pixels) {
		return Image{}, "", ErrTooManyPx
	}
	if cfg.Width <= limits.side && cfg.Height <= limits.side && base64.StdEncoding.EncodedLen(len(data)) < limits.base64 {
		return Image{Data: data, MimeType: mime}, "", nil
	}
	prepareMu.Lock()
	defer prepareMu.Unlock()
	src, err := decode(format, data)
	if err != nil {
		return Image{}, "", ErrUndecodable
	}
	w0, h0 := src.Bounds().Dx(), src.Bounds().Dy()
	w, h := fit(w0, h0, limits.side)
	for w >= 1 && h >= 1 {
		scaled := src
		if w != w0 || h != h0 {
			dst := image.NewNRGBA(image.Rect(0, 0, w, h))
			draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
			scaled = dst
		}
		out, ok, err := firstFitting(scaled, limits.base64)
		if err != nil {
			return Image{}, "", err
		}
		if ok {
			return out, imageNote(mime, out.MimeType, w0, h0, w, h), nil
		}
		if w == 1 && h == 1 {
			break
		}
		w, h = max(1, w*3/4), max(1, h*3/4)
	}
	return Image{}, "", ErrUnfit
}

// decode decodes data of format.
func decode(format string, data []byte) (image.Image, error) {
	r := bytes.NewReader(data)
	switch format {
	case "png":
		return png.Decode(r)
	case "jpeg":
		return jpeg.Decode(r)
	case "gif":
		return gif.Decode(r)
	case "webp":
		return webp.Decode(r)
	}
	return nil, ErrUndecodable
}

// fit is w×h within side pixels a side, its proportions kept: as it is
// when it fits.
func fit(w, h, side int) (int, int) {
	if w <= side && h <= side {
		return w, h
	}
	scale := math.Min(float64(side)/float64(w), float64(side)/float64(h))
	return max(1, int(math.Round(float64(w)*scale))), max(1, int(math.Round(float64(h)*scale)))
}

// firstFitting encodes img as PNG and as JPEG at the first quality, and
// gives the first of the two, the smaller first, below limit bytes of
// base64; else the first lower JPEG quality that is.
func firstFitting(img image.Image, limit int) (Image, bool, error) {
	var p bytes.Buffer
	if err := png.Encode(&p, img); err != nil {
		return Image{}, false, err
	}
	candidates := []Image{{Data: p.Bytes(), MimeType: "image/png"}}
	var j bytes.Buffer
	if err := jpeg.Encode(&j, img, &jpeg.Options{Quality: jpegQualities[0]}); err == nil {
		jp := Image{Data: j.Bytes(), MimeType: "image/jpeg"}
		if len(jp.Data) < len(p.Bytes()) {
			candidates = []Image{jp, candidates[0]}
		} else {
			candidates = append(candidates, jp)
		}
	}
	for _, c := range candidates {
		if base64.StdEncoding.EncodedLen(len(c.Data)) < limit {
			return c, true, nil
		}
	}
	for _, quality := range jpegQualities[1:] {
		var b bytes.Buffer
		if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: quality}); err != nil {
			continue
		}
		if base64.StdEncoding.EncodedLen(b.Len()) < limit {
			return Image{Data: b.Bytes(), MimeType: "image/jpeg"}, true, nil
		}
	}
	return Image{}, false, nil
}

// imageNote says what the model got that the file is not: its format, and
// its size with the factor that maps its coordinates back, in
// pi-coding-agent's words. "" when neither changed.
func imageNote(was, is string, w0, h0, w, h int) string {
	var notes []string
	if is != was {
		notes = append(notes, "[Image: shown to you as "+strings.ToUpper(strings.TrimPrefix(is, "image/"))+".]")
	}
	if w != w0 || h != h0 {
		notes = append(notes, fmt.Sprintf("[Image: original %dx%d, displayed at %dx%d. Multiply coordinates by %.2f to map to original image.]",
			w0, h0, w, h, float64(w0)/float64(w)))
	}
	return strings.Join(notes, " ")
}

// NotShown is the note on the line of an image that did not go to the
// model, saying why.
func NotShown(why error) string {
	text := strings.Join(strings.Fields(why.Error()), " ")
	return "[Image: not shown to you: " + strings.TrimSuffix(text, ".") + ".]"
}
