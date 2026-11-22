package pocketdtest

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// The fake answers a session's changes and a file's diff as pocketd does
// (rules 63 and 64): a session given none is in no checkout, and a path no
// change can have is bad_request before the session or the file is looked
// at, even one with a diff set (Codex review of #109, round 1).
func TestTheFakeAnswersChangesAsPocketdDoes(t *testing.T) {
	s := New()
	s.AddSession(proto.Session{ID: 2, Title: "T7", UpdatedAt: Now})
	s.SetFileDiff(proto.FileDiff{SessionID: 2, Path: "../x", Diff: "+x\n"})
	s.SetFileDiff(proto.FileDiff{SessionID: 2, Path: "a.go", Diff: "+a\n"})
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got, err := client.Changes(ctx, 2); err != nil || got.Repository || got.Files == nil {
		t.Fatalf("changes of a session given none: %+v %v", got, err)
	}
	status := func(err error) (int, proto.ErrorCode) {
		var pe *proto.Error
		if !errors.As(err, &pe) {
			return 0, ""
		}
		return pe.Status, pe.Code
	}
	for _, tc := range []struct {
		session int64
		path    string
		status  int
	}{
		{2, "../x", http.StatusBadRequest}, {9, "", http.StatusBadRequest}, {9, "/etc/passwd", http.StatusBadRequest},
		{9, "a.go", http.StatusNotFound}, {2, "b.go", http.StatusNotFound},
	} {
		_, err := client.ChangesFile(ctx, tc.session, tc.path)
		if got, code := status(err); got != tc.status || (got == http.StatusBadRequest && code != proto.ErrorCodeBadRequest) {
			t.Errorf("session %d, %q: %v", tc.session, tc.path, err)
		}
	}
	if got, err := client.ChangesFile(ctx, 2, "a.go"); err != nil || got.Diff != "+a\n" {
		t.Fatalf("a.go: %+v %v", got, err)
	}
}
