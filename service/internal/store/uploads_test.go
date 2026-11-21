package store

import (
	"errors"
	"testing"
)

// An upload is recorded with the request that stored it, in one
// transaction: the same request answers the same upload and records none,
// another request's key for another kind is refused, and a message names
// only uploads of its own session (ADR-0029).
func TestAnUploadIsRecordedWithItsRequest(t *testing.T) {
	s, _ := open(t)
	for _, id := range []int64{7, 8} {
		if _, err := s.InsertSession(ctx, SessionRow{ID: id, Title: "t", Cwd: "/w", Provider: "p", Model: "m", CreatedBy: "d1"}); err != nil {
			t.Fatal(err)
		}
	}
	row := UploadRow{Session: 7, File: "a-b-shot.png", Name: "shot.png", Mime: "image/png", Size: 12}
	first, existed, err := s.RecordUpload(ctx, "u:d1:r1", "upload:7", row)
	if err != nil || existed || first.ID == 0 || first.File != row.File || first.CreatedAt.IsZero() {
		t.Fatalf("RecordUpload = %+v, %v, %v", first, existed, err)
	}
	again, existed, err := s.RecordUpload(ctx, "u:d1:r1", "upload:7", UploadRow{Session: 7, File: "other", Name: "other", Mime: "text/plain", Size: 1})
	if err != nil || !existed || again != first {
		t.Fatalf("a retry recorded %+v, %v, %v (want %+v)", again, existed, err, first)
	}
	if _, _, err := s.RecordUpload(ctx, "u:d1:r1", "upload:8", row); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("a request id reused for another session: %v", err)
	}
	if result, existed, err := s.LookupRequest(ctx, "u:d1:r1", "upload:7"); err != nil || !existed || result == "" {
		t.Fatalf("the request's record: %q, %v, %v", result, existed, err)
	}
	other, _, err := s.RecordUpload(ctx, "u:d1:r2", "upload:8", UploadRow{Session: 8, File: "c-d-log.txt", Name: "log.txt", Mime: "text/plain", Size: 3})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.RecordUpload(ctx, "u:d1:r3", "upload:7", UploadRow{Session: 7, File: "e-f-a.txt", Name: "a.txt", Mime: "text/plain", Size: 1})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.SessionUploads(ctx, 7, []int64{second.ID, first.ID, second.ID})
	if err != nil || len(got) != 2 || got[0] != second || got[1] != first {
		t.Fatalf("SessionUploads = %+v, %v", got, err)
	}
	if _, err := s.SessionUploads(ctx, 7, []int64{first.ID, other.ID}); !errors.Is(err, ErrNotSessions) {
		t.Fatalf("another session's upload was taken: %v", err)
	}
	if _, err := s.SessionUploads(ctx, 7, []int64{999}); !errors.Is(err, ErrNotSessions) {
		t.Fatalf("an upload that is none was taken: %v", err)
	}
}
