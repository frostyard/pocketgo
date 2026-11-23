package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/files"
	"github.com/frostyard/pocketgo/service/internal/store"
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
	out := proto.FileRoots{SessionFolders: true, Home: s.filesCfg.Home, Roots: []proto.FileRoot{}}
	for _, h := range homes {
		out.Roots = append(out.Roots, proto.FileRoot{ID: h.ID, Name: h.Name, Path: h.Path, Private: h.Private})
	}
	writeJSON(w, http.StatusOK, out)
}

// viewerRoot resolves owner-configured links before handing a physical path to
// the sandbox. The sandbox pins each component without following more links.
func (s *Server) viewerRoot(ctx context.Context, id string) (files.Home, error) {
	if session, ok := proto.ParseSessionFilesRoot(id); ok {
		return s.sessionFileRoot(ctx, id, session)
	}
	homes, err := s.filesHomes()
	if err != nil {
		return files.Home{}, err
	}
	for _, h := range homes {
		if h.ID == id {
			real, err := filepath.EvalSymlinks(h.Path)
			if err != nil {
				return files.Home{}, err
			}
			return s.checkedRoot(id, real)
		}
	}
	return files.Home{}, files.ErrNotFound
}

func (s *Server) checkedRoot(id, p string) (files.Home, error) {
	roots := files.Roots([]files.Home{{ID: id, Path: p}}, s.filesCfg.Refused)
	if len(roots) == 0 {
		return files.Home{}, files.ErrNotFound
	}
	return roots[0], nil
}

func (s *Server) sessionFileRoot(ctx context.Context, id string, session int64) (files.Home, error) {
	row, err := s.store.Session(ctx, session)
	if err != nil {
		return files.Home{}, err
	}
	cwd := row.Cwd
	if !filepath.IsAbs(cwd) || cwd != filepath.Clean(cwd) || cwd == "/" {
		return files.Home{}, files.ErrNotFound
	}
	if _, err := s.checkedRoot(id, cwd); err != nil {
		return files.Home{}, err
	}
	anchors := []string{}
	if s.filesCfg.Home != "" && cwd != filepath.Clean(s.filesCfg.Home) {
		anchors = append(anchors, s.filesCfg.Home)
	}
	var siblings []string
	if s.profiles != nil {
		pp, _, err := s.profiles.Load()
		if err != nil {
			return files.Home{}, err
		}
		for _, p := range pp {
			anchors = append(anchors, p.Folders()...)
			for _, repo := range p.Repositories {
				if filepath.Dir(cwd) == filepath.Dir(repo) && strings.HasPrefix(cwd, repo+"--") {
					siblings = append(siblings, filepath.Dir(repo))
				}
			}
		}
	}
	if len(siblings) > 0 {
		owned, err := s.hasFileWorktree(ctx, row)
		if err != nil {
			return files.Home{}, err
		}
		if owned {
			anchors = append(anchors, siblings...)
		}
	}
	// Prefer the explicitly configured checkout or home when a broader
	// authorized directory also contains it. Only configured links may resolve.
	slices.SortStableFunc(anchors, func(a, b string) int { return len(b) - len(a) })
	for _, anchor := range anchors {
		if !filepath.IsAbs(anchor) {
			continue
		}
		real, err := filepath.EvalSymlinks(anchor)
		if err != nil {
			continue
		}
		// Sessions can record either spelling of an owner-configured link,
		// such as /home on a host whose physical directory is /var/home.
		for _, prefix := range []string{anchor, real} {
			rel, err := filepath.Rel(prefix, cwd)
			if err != nil {
				continue
			}
			rel, err = proto.CheckFilePath(rel)
			if err == nil {
				return s.checkedRoot(id, filepath.Join(real, rel))
			}
		}
	}
	return files.Home{}, files.ErrNotFound
}

// A generated sibling is eligible only if pocketd recorded the worktree.
func (s *Server) hasFileWorktree(ctx context.Context, row store.SessionRow) (bool, error) {
	owned, err := s.store.SessionWorktreeAt(ctx, row.Cwd)
	if owned || err != nil {
		return owned, err
	}
	specialist := row.ID
	if row.StartedBy != 0 {
		specialist = row.StartedBy
	}
	thread, err := s.store.ThreadOfSession(ctx, specialist)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return thread.Cwd == row.Cwd && thread.Branch != "", err
}

// readFiles validates a request, pins its profile root and bounds the sandbox work.
func (s *Server) readFiles(w http.ResponseWriter, r *http.Request, read func(context.Context, files.Home, string) (any, error)) {
	clean, err := proto.CheckFilePath(r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	id := r.URL.Query().Get("root")
	root, err := s.viewerRoot(r.Context(), id)
	if errors.Is(err, files.ErrNotFound) || errors.Is(err, os.ErrNotExist) || errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, proto.ErrorCodeNotFound, "File root unavailable: "+id)
		return
	}
	if err != nil {
		s.internal(w, "resolve file root", err)
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
