package api

import (
	"net/http"

	"github.com/frostyard/pocketgo/proto"
)

// listPrompts answers GET /v1/prompts (ADR-0051): the owner's saved
// prompts, read from the profile folder at each request, so a change to
// one shows without a restart, as profiles.Folder.Prompts checks them. A
// file that breaks a rule lists its name and problem, and the others
// still list; a folder that cannot be read lists none, and says why. The
// service running without profiles has none. It changes nothing.
func (s *Server) listPrompts(w http.ResponseWriter, _ *http.Request) {
	out := proto.Prompts{Prompts: []proto.Prompt{}}
	if s.profiles == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	list, err := s.profiles.Prompts()
	if err != nil {
		out.Problem = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Problem = list.Problem
	for _, p := range list.List {
		out.Prompts = append(out.Prompts, proto.Prompt{Name: p.Name, Description: p.Description, ArgumentHint: p.ArgumentHint,
			Text: p.Text, Problem: p.Problem})
	}
	writeJSON(w, http.StatusOK, out)
}
