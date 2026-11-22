package store

import (
	"errors"
	"testing"
)

// A rename writes the title with its request's record, in one transaction
// (ADR-0058): a retry renames nothing again, the key reused for another
// kind is refused, a session that is not there leaves no record, and the
// session's updated_at stays, so the list keeps its place.
func TestRenameSessionWritesTheTitleWithItsRequest(t *testing.T) {
	s, _ := open(t)
	for _, id := range []int64{1, 2} {
		if _, err := s.InsertSession(ctx, SessionRow{ID: id, Title: "Session", Cwd: "/w", Provider: "p", Model: "m", CreatedBy: "d"}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.Sessions(ctx)
	if err != nil || len(before) != 2 || before[0].ID != 2 {
		t.Fatalf("sessions %+v, %v", before, err)
	}
	replayed, err := s.RenameSession(ctx, "k1", "title:1:Garden", 1, "Garden")
	if err != nil || replayed {
		t.Fatalf("rename %v %v", replayed, err)
	}
	after, err := s.Sessions(ctx)
	if err != nil || len(after) != 2 || after[0].ID != 2 || after[1].Title != "Garden" || !after[1].UpdatedAt.Equal(before[1].UpdatedAt) {
		t.Fatalf("after the rename %+v, %v", after, err)
	}
	// A retry renames nothing again, whatever the title is now.
	if _, err := s.RenameSession(ctx, "k2", "title:1:Beds", 1, "Beds"); err != nil {
		t.Fatal(err)
	}
	if replayed, err := s.RenameSession(ctx, "k1", "title:1:Garden", 1, "Garden"); err != nil || !replayed {
		t.Fatalf("retry %v %v", replayed, err)
	}
	if row, _ := s.Session(ctx, 1); row.Title != "Beds" {
		t.Fatalf("a retry renamed the session again: %q", row.Title)
	}
	if _, err := s.RenameSession(ctx, "k1", "title:2:Garden", 2, "Garden"); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("the key for another session: %v", err)
	}
	if _, err := s.RenameSession(ctx, "k3", "title:9:Gone", 9, "Gone"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a session that is not there: %v", err)
	}
	if _, existed, err := s.LookupRequest(ctx, "k3", "title:9:Gone"); err != nil || existed {
		t.Fatalf("a failed rename kept its record: %v %v", existed, err)
	}
}
