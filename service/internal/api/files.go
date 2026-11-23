package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/files"
)

// FilesConfig names the service home and the paths no agent home may expose.
type FilesConfig struct {
	Home    string
	Refused []string
}

// SetFiles gives the viewer its root exclusions before serving (ADR-0066).
// A relative refused path is made absolute, so it is compared with the
// absolute homes; one that cannot be is kept as written.
func (s *Server) SetFiles(cfg FilesConfig) {
	cfg.Refused = slices.Clone(cfg.Refused)
	for i, p := range cfg.Refused {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			cfg.Refused[i] = abs
		}
	}
	s.filesCfg = cfg
}

// filesTimeout bounds one sandbox read of an agent's home.
const filesTimeout = 15 * time.Second

// filesHomes reads valid profiles and filters out homes touching private service paths.
func (s *Server) filesHomes() ([]files.Home, error) {
	homes := []files.Home{}
	if s.profiles != nil {
		profiles, _, err := s.profiles.Load()
		if err != nil {
			return nil, err
		}
		for _, p := range profiles {
			name := p.Name
			if name == "" {
				name = p.ID
			}
			homes = append(homes, files.Home{ID: p.ID, Name: name, Path: p.Home, Private: p.Private})
		}
	}
	return files.Roots(homes, s.filesCfg.Refused), nil
}

// fileRoots lists the agents' homes (rule 70).
func (s *Server) fileRoots(w http.ResponseWriter, r *http.Request) {
	homes, err := s.filesHomes()
	if err != nil {
		s.internal(w, "read profiles", err)
		return
	}
	out := proto.FileRoots{Home: s.filesCfg.Home, Roots: []proto.FileRoot{}}
	for _, h := range homes {
		out.Roots = append(out.Roots, proto.FileRoot{ID: h.ID, Name: h.Name, Path: h.Path, Private: h.Private})
	}
	writeJSON(w, http.StatusOK, out)
}

// readFiles validates a request, pins its profile root and bounds the sandbox work.
func (s *Server) readFiles(w http.ResponseWriter, r *http.Request, read func(context.Context, files.Home, string) (any, error)) {
	clean, err := proto.CheckFilePath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	homes, err := s.filesHomes()
	if err != nil {
		s.internal(w, "read profiles", err)
		return
	}
	id := r.URL.Query().Get("root")
	var root files.Home
	found := false
	for _, h := range homes {
		if h.ID == id {
			root = h
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "no agent home "+id)
		return
	}
	select {
	case s.filesSlots <- struct{}{}:
		defer func() { <-s.filesSlots }()
	case <-r.Context().Done():
		return
	}
	ctx, cancel := s.withTimeout(r.Context(), filesTimeout)
	defer cancel()
	out, err := read(ctx, root, clean)
	switch {
	case errors.Is(err, files.ErrBadPath):
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
	case errors.Is(err, files.ErrNotFound) || errors.Is(err, files.ErrOutside) || errors.Is(err, files.ErrHidden) || errors.Is(err, files.ErrNotFolder) || errors.Is(err, files.ErrNotFile):
		name := id
		if clean != "" {
			name += "/" + clean
		}
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, name+": "+err.Error())
	case err != nil:
		s.internal(w, "read the agent's files", err)
	default:
		writeJSON(w, http.StatusOK, out)
	}
}

// listFiles lists a folder below the selected agent's home (rule 71).
func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	s.readFiles(w, r, func(ctx context.Context, root files.Home, clean string) (any, error) {
		folder, err := s.engine.ListFiles(ctx, root.Path, clean, proto.MaxFolderEntries, proto.MaxFolderNames)
		if err != nil {
			return nil, err
		}
		out := proto.FileFolder{Root: root.ID, Path: clean, Real: folder.Path, Entries: []proto.FileEntry{}, More: folder.More, Partial: folder.Partial}
		for _, e := range folder.Entries {
			out.Entries = append(out.Entries, proto.FileEntry{Name: e.Name, Kind: e.Kind, Link: e.Link, Size: e.Size, Modified: e.Modified, Withheld: e.Withheld})
		}
		return out, nil
	})
}

// readFile prepares bounded text or image content without following a client path elsewhere.
func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	s.readFiles(w, r, func(ctx context.Context, root files.Home, clean string) (any, error) {
		file, err := s.engine.ReadFile(ctx, root.Path, clean, proto.MaxFileImage)
		if err != nil {
			return nil, err
		}
		out := proto.FileContent{Root: root.ID, Path: clean, Real: file.Path, Size: file.Size, Modified: file.Modified}
		name := path.Base(file.Path)
		switch {
		case file.Over:
			out.Kind = proto.FileLarge
			out.Note = "over " + fileBytes(proto.MaxFileImage) + " bytes: not shown"
		case files.IsImageName(name):
			img, note, err := s.prepareImage(file.Data)
			if err != nil {
				out.Kind = proto.FileBinary
				out.Note = "not shown: " + err.Error()
			} else {
				out.Kind = proto.FileImage
				out.Image = img.Data
				out.MimeType = img.MimeType
				out.Note = note
			}
		case len(file.Data) > proto.MaxFileText:
			out.Kind = proto.FileLarge
			out.Note = "over " + fileBytes(proto.MaxFileText) + " bytes: not shown"
		case bytes.IndexByte(file.Data, 0) >= 0 || !utf8.Valid(file.Data):
			out.Kind = proto.FileBinary
			out.Note = "not shown: not UTF-8 text"
		case files.IsMarkdownName(name):
			out.Kind = proto.FileMarkdown
			out.Text = string(file.Data)
		default:
			out.Kind = proto.FileText
			out.Text = string(file.Data)
		}
		return out, nil
	})
}

// fileBytes formats a byte bound with thousands separators.
func fileBytes(n int) string {
	digits := strconv.Itoa(n)
	first := len(digits) % 3
	if first == 0 {
		first = 3
	}
	var out strings.Builder
	out.WriteString(digits[:first])
	for i := first; i < len(digits); i += 3 {
		_, _ = fmt.Fprintf(&out, ",%s", digits[i:i+3])
	}
	return out.String()
}
