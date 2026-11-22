package pocketdtest

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// The fake reads a start's and a cancel's body as pocketd does: an unknown
// field, data after the object or a request id over its bound is
// bad_request, and changes nothing (Codex review of #123, round 3).
func TestTheFakeReadsSignInBodiesStrictly(t *testing.T) {
	s := New()
	h := &http.Client{Transport: s.Transport()}
	post := func(path, body string) int {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, s.URL+path, strings.NewReader(body))
		request.Header.Set(proto.HeaderProtocol, strconv.Itoa(proto.Version))
		request.Header.Set("Authorization", "Bearer "+Token)
		response, err := h.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	start := proto.Path(proto.PathSignIn, "github-copilot")
	for _, body := range []string{`{"requestId":"s1","extra":1}`, `{"requestId":"s1"}{}`, `{"requestId":"` + strings.Repeat("a", proto.MaxRequestIDSize+1) + `"}`} {
		if status := post(start, body); status != http.StatusBadRequest {
			t.Fatalf("a start with %s answered %d", body, status)
		}
	}
	if rows := s.SignIns(); rows[0].Flow != nil {
		t.Fatalf("a refused start made %+v", rows[0].Flow)
	}
	if status := post(start, `{"requestId":"s1"}`); status != http.StatusCreated {
		t.Fatalf("a start answered %d", status)
	}
	if status := post(proto.Path(proto.PathCancelSignIn, "flow-1"), `{"requestId":"c1","extra":1}`); status != http.StatusBadRequest {
		t.Fatalf("a cancel with an unknown field answered %d", status)
	}
	if rows := s.SignIns(); rows[0].Flow.State != proto.FlowWaiting {
		t.Fatalf("a refused cancel left %+v", rows[0].Flow)
	}
}
