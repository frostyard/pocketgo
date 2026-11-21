package pocketdtest

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif" // GIFs decode as pocketd decodes them
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // and WebPs
)

// UploadsDir is where the fake says it keeps the owner's uploads (rule
// 10a); it keeps their bytes in memory.
const UploadsDir = "/home/team/.local/share/pocketgo-uploads"

// uploadRoutes serves the owner's uploads.
func (s *Server) uploadRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+proto.PathUploads, s.uploadFile)
}

// uploadFile keeps a file the owner attaches to a message, once a
// request id, as pocketd does: a worker's or a cross-review's session
// takes none.
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	requestID := r.URL.Query().Get("requestId")
	if requestID == "" {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "requestId is required")
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "the upload stopped before it ended")
		return
	}
	id := pathID(r)
	// The request id is looked up under the lock that records its upload,
	// as pocketd holds a request's key: two tries of one id store one
	// upload (Codex review of #49, round 5).
	s.mu.Lock()
	defer s.mu.Unlock()
	prior, seen := s.replies[requestID]
	if s.afterReplayLookup != nil {
		s.afterReplayLookup()
	}
	if seen {
		if prior.op != r.Method+" "+r.URL.Path {
			writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "the requestId was used for a different request")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(prior.body)
		return
	}
	sess := s.sessions[id]
	if sess == nil {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no such session")
		return
	}
	if route := sess.detail.Session.Route; route == "cross-review" || strings.HasPrefix(route, "worker") {
		writeError(w, http.StatusConflict, proto.ErrorCodeConflict, "this session takes no upload")
		return
	}
	// The name and type pocketd keeps (Codex review of #49, round 8).
	name := proto.SafeUploadName(r.URL.Query().Get("name"))
	mimeType := proto.UploadMime(name, r.Header.Get("Content-Type"))
	s.nextUpload++
	up := proto.Upload{ID: s.nextUpload, SessionID: id, Name: name, MimeType: mimeType, Size: int64(len(data)),
		Path: fmt.Sprintf("%s/%d/%d-%s", UploadsDir, id, s.nextUpload, name)}
	s.uploads = append(s.uploads, up)
	if s.asImage == nil {
		s.asImage = map[int64]int{}
	}
	s.asImage[up.ID] = imageCost(mimeType, data)
	s.rememberLocked(r, requestID, up)
	writeJSON(w, http.StatusCreated, up)
}

// Uploads are the files uploaded, oldest first.
func (s *Server) Uploads() []proto.Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.uploads)
}

// attachedLocked are the files of message attachments to session, and how
// many images the message carries, whatever model the session runs, as
// pocketd counts them (rule 10a; Codex review of #49, round 6); ok is
// false when one is no upload of the session. s.mu is held.
func (s *Server) attachedLocked(session int64, attachments []int64) (files []proto.AttachedFile, images int, ok bool) {
	seen := map[int64]bool{}
	// A message's images take at most 24 MiB of base64, in the order it
	// names them, as pocketd's do (uploads.MaxMessageImages; Codex review
	// of #49, round 10).
	inline := 0
	for _, id := range attachments {
		if seen[id] {
			continue
		}
		seen[id] = true
		i := slices.IndexFunc(s.uploads, func(u proto.Upload) bool { return u.ID == id && u.SessionID == session })
		if i < 0 {
			return nil, 0, false
		}
		up := s.uploads[i]
		files = append(files, proto.AttachedFile{Path: up.Path, Name: up.Name, MimeType: up.MimeType, Size: up.Size})
		if cost := s.asImage[up.ID]; cost > 0 && inline+cost <= maxMessageImages {
			inline += cost
			images++
		}
	}
	return files, images, true
}

// goesAsImage says whether a message carries an upload of mimeType and
// data as an image, as pocketd's rule 10a says: a PNG, JPEG, GIF or WebP
// type, at most 5,242,880 bytes, whose bytes are an image of those
// formats, of at most 50,000,000 pixels, that decodes whole (a GIF its
// first frame), as pocketd decodes it (Codex review of #49, round 9).
func goesAsImage(mimeType string, data []byte) bool {
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return false
	}
	if len(data) > 5<<20 {
		return false
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg" && format != "gif" && format != "webp") ||
		config.Width < 1 || config.Height < 1 || int64(config.Width)*int64(config.Height) > 50_000_000 {
		return false
	}
	_, _, err = image.Decode(bytes.NewReader(data))
	return err == nil
}

// The limits of pocketd's images (uploads): a message's images, an
// image's base64, and the side an image goes at as it is.
const (
	maxMessageImages = 24 << 20
	maxImageBase64   = 4.5 * (1 << 20)
	maxImageSide     = 2000
)

// imageCost is the base64 an upload of mimeType and data takes of a
// message's images, 0 when it goes as no image (goesAsImage): its own
// when pocketd sends it as it is; else that of the image as pocketd fits
// it, a GIF its first frame (fitImage); and pocketd's bound for one that
// never fits (Codex review of #49, rounds 10 to 12).
func imageCost(mimeType string, data []byte) int {
	if !goesAsImage(mimeType, data) {
		return 0
	}
	cost := base64.StdEncoding.EncodedLen(len(data))
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err == nil && format != "gif" && config.Width <= maxImageSide && config.Height <= maxImageSide && cost < maxImageBase64 {
		return cost
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return maxImageBase64
	}
	return fitImage(img)
}

// fitImage is the base64 of img as pocketd fits it (uploads.PrepareImage):
// within maxImageSide, as PNG or JPEG at quality 80, the smaller first,
// then as JPEG at 70, 55 and 40, a JPEG only of an opaque image; else a
// quarter smaller each time, until one is below maxImageBase64.
func fitImage(img image.Image) int {
	w0, h0 := img.Bounds().Dx(), img.Bounds().Dy()
	w, h := w0, h0
	if w > maxImageSide || h > maxImageSide {
		scale := min(float64(maxImageSide)/float64(w), float64(maxImageSide)/float64(h))
		w, h = max(1, int(math.Round(float64(w)*scale))), max(1, int(math.Round(float64(h)*scale)))
	}
	o, ok := img.(interface{ Opaque() bool })
	opaque := ok && o.Opaque()
	for w >= 1 && h >= 1 {
		scaled := img
		if w != w0 || h != h0 {
			fitted := image.NewRGBA(image.Rect(0, 0, w, h))
			draw.ApproxBiLinear.Scale(fitted, fitted.Bounds(), img, img.Bounds(), draw.Src, nil)
			scaled = fitted
		}
		var sizes []int
		var out bytes.Buffer
		if png.Encode(&out, scaled) == nil {
			sizes = append(sizes, out.Len())
		}
		if opaque {
			for _, quality := range []int{80, 70, 55, 40} {
				out.Reset()
				if jpeg.Encode(&out, scaled, &jpeg.Options{Quality: quality}) == nil {
					sizes = append(sizes, out.Len())
				}
			}
		}
		if len(sizes) > 1 && sizes[1] < sizes[0] {
			sizes[0], sizes[1] = sizes[1], sizes[0] // the smaller of PNG and JPEG at 80 first
		}
		for _, size := range sizes {
			if cost := base64.StdEncoding.EncodedLen(size); cost < maxImageBase64 {
				return cost
			}
		}
		if w == 1 && h == 1 {
			break
		}
		w, h = max(1, w*3/4), max(1, h*3/4)
	}
	return maxImageBase64
}
