package pocketdtest

import (
	"net/http"
	"slices"

	"github.com/frostyard/pocketgo/proto"
)

// SetFileRoots replaces the homes the fake viewer lists.
func (s *Server) SetFileRoots(r proto.FileRoots) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Roots = slices.Clone(r.Roots)
	s.fileRoots = r
}

// SetFileFolder replaces one folder's listing, copying its entries.
func (s *Server) SetFileFolder(f proto.FileFolder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fileFolders == nil {
		s.fileFolders = map[string]proto.FileFolder{}
	}
	f.Entries = slices.Clone(f.Entries)
	s.fileFolders[f.Root+"\x00"+f.Path] = f
}

// SetFileContent replaces one file's content, copying its image bytes.
func (s *Server) SetFileContent(c proto.FileContent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fileContents == nil {
		s.fileContents = map[string]proto.FileContent{}
	}
	c.Image = slices.Clone(c.Image)
	s.fileContents[c.Root+"\x00"+c.Path] = c
}

// filesRoutes answers viewer reads with pocketd's validation and refusal order.
func (s *Server) filesRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+proto.PathFiles, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		roots := s.fileRoots
		if roots.Roots == nil {
			roots.Roots = []proto.FileRoot{}
		}
		writeJSON(w, http.StatusOK, roots)
	})
	for _, route := range []string{proto.PathFilesList, proto.PathFilesRead} {
		mux.HandleFunc("GET "+route, func(w http.ResponseWriter, r *http.Request) {
			p, err := proto.CheckFilePath(r.URL.Query().Get("path"))
			if err != nil {
				writeError(w, 400, proto.ErrorCodeBadRequest, err.Error())
				return
			}
			root := r.URL.Query().Get("root")
			s.mu.Lock()
			defer s.mu.Unlock()
			session, scoped := proto.ParseSessionFilesRoot(root)
			hasSession := scoped && s.sessions[session] != nil
			if !hasSession && !slices.ContainsFunc(s.fileRoots.Roots, func(h proto.FileRoot) bool { return h.ID == root }) {
				writeError(w, 404, proto.ErrorCodeNotFound, "no agent home "+root)
				return
			}
			key := root + "\x00" + p
			if route == proto.PathFilesList {
				if f, ok := s.fileFolders[key]; ok {
					if f.Entries == nil {
						f.Entries = []proto.FileEntry{}
					}
					writeJSON(w, 200, f)
					return
				}
			} else if c, ok := s.fileContents[key]; ok {
				writeJSON(w, 200, c)
				return
			}
			name := root
			if p != "" {
				name += "/" + p
			}
			writeError(w, 404, proto.ErrorCodeNotFound, name+": nothing is there")
		})
	}
}
