package pocketdtest

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// The fake renames as pocketd does (ADR-0058; Codex review of #118, round
// 1): a session it creates from a private profile that is no supervisor's
// carries renamable and renames; a team agent's and a private
// supervisor's do not. A retry publishes the session as it is again, and a
// body with no title is refused.
func TestTheFakeRenamesAPrivateAgentsSessionAsPocketdDoes(t *testing.T) {
	s := New()
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	s.AddModel(proto.Model{ModelRef: opus, Name: "Claude Opus 5.5"})
	s.AddProfile(proto.Profile{ID: "chani", Role: proto.RoleAgent, Home: "/home/chani", Private: true, Runs: opus})
	s.AddProfile(proto.Profile{ID: "alia", Role: proto.RoleSupervisor, Home: "/home/alia", Private: true, Runs: opus})
	s.AddProfile(proto.Profile{ID: "vera", Role: proto.RoleAgent, Home: "/home/vera", Runs: opus})
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	made := map[string]proto.Session{}
	for _, p := range []string{"chani", "alia", "vera"} {
		got, err := client.CreateSession(ctx, proto.CreateSession{RequestID: p, Profile: p})
		if err != nil {
			t.Fatal(err)
		}
		made[p] = got
	}
	if !made["chani"].Renamable || made["alia"].Renamable || made["vera"].Renamable {
		t.Fatalf("renamable: chani %v, alia %v, vera %v", made["chani"].Renamable, made["alia"].Renamable, made["vera"].Renamable)
	}
	var pe *proto.Error
	if _, err := client.RenameSession(ctx, made["vera"].ID, proto.RenameSession{RequestID: "v", Title: "Renamed"}); !errors.As(err, &pe) || pe.Code != proto.ErrorCodeBadRequest {
		t.Fatalf("a team agent's session: %v", err)
	}
	id := made["chani"].ID
	events := func() int {
		s.mu.Lock()
		defer s.mu.Unlock()
		n := 0
		for _, e := range s.kept {
			if e.Type == proto.EventSession && e.Session.ID == id {
				n++
			}
		}
		return n
	}
	before := events()
	if got, err := client.RenameSession(ctx, id, proto.RenameSession{RequestID: "r", Title: " Garden plan "}); err != nil || got.Title != "Garden plan" {
		t.Fatalf("the rename: %+v, %v", got, err)
	}
	if got, err := client.RenameSession(ctx, id, proto.RenameSession{RequestID: "r", Title: " Garden plan "}); err != nil || got.Title != "Garden plan" {
		t.Fatalf("the retry: %+v, %v", got, err)
	}
	if n := events() - before; n != 2 {
		t.Fatalf("the rename and its retry published %d session events, want 2", n)
	}
	request, err := http.NewRequest(http.MethodPost, s.URL+proto.Path(proto.PathTitle, id), strings.NewReader(`{"requestId":"n"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+Token)
	request.Header.Set(proto.HeaderProtocol, strconv.Itoa(proto.Version))
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Transport: s.Transport()}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a body with no title: %d", response.StatusCode)
	}
	if detail, _ := s.Session(id); detail.Session.Title != "Garden plan" {
		t.Fatalf("a body with no title renamed the session %q", detail.Session.Title)
	}
}
