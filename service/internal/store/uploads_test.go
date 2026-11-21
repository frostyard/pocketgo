package store

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

// record claims and stores an upload of session under key.
func record(t *testing.T, s *Store, key string, row UploadRow, size int64, mime string) UploadRow {
	t.Helper()
	if _, _, _, err := s.ClaimUpload(ctx, key, "upload:"+strconv.FormatInt(row.Session, 10), row); err != nil {
		t.Fatal(err)
	}
	stored, err := s.FinishUpload(ctx, key, size, mime)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// An upload is claimed with its request, pending until its file is
// written, then stored: the same request finds it, another request's key
// for another kind is refused, and a message names only stored uploads
// of its own session (ADR-0029).
func TestAnUploadIsRecordedWithItsRequest(t *testing.T) {
	s, _ := open(t)
	for _, id := range []int64{7, 8} {
		if _, err := s.InsertSession(ctx, SessionRow{ID: id, Title: "t", Cwd: "/w", Provider: "p", Model: "m", CreatedBy: "d1"}); err != nil {
			t.Fatal(err)
		}
	}
	row := UploadRow{Session: 7, File: "a-b-shot.png", Name: "shot.png"}
	claimed, pending, existed, err := s.ClaimUpload(ctx, "u:d1:r1", "upload:7", row)
	if err != nil || !pending || existed || claimed.ID == 0 {
		t.Fatalf("ClaimUpload = %+v, %v, %v, %v", claimed, pending, existed, err)
	}
	// Pending, it is no upload yet, and its request has no result.
	if _, err := s.Upload(ctx, claimed.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a pending upload reads as %v", err)
	}
	if result, existed, err := s.LookupRequest(ctx, "u:d1:r1", "upload:7"); err != nil || !existed || result != "" {
		t.Fatalf("the pending request: %q, %v, %v", result, existed, err)
	}
	first, err := s.FinishUpload(ctx, "u:d1:r1", 12, "image/png")
	if err != nil || first.ID != claimed.ID || first.Size != 12 || first.Mime != "image/png" {
		t.Fatalf("FinishUpload = %+v, %v", first, err)
	}
	again, pending, existed, err := s.ClaimUpload(ctx, "u:d1:r1", "upload:7", UploadRow{Session: 7, File: "other", Name: "other"})
	if err != nil || pending || !existed || again.ID != first.ID || again.File != first.File {
		t.Fatalf("a retry claimed %+v, %v, %v, %v (want %+v)", again, pending, existed, err, first)
	}
	if _, _, _, err := s.ClaimUpload(ctx, "u:d1:r1", "upload:8", row); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("a request id reused for another session: %v", err)
	}
	other := record(t, s, "u:d1:r2", UploadRow{Session: 8, File: "c-d-log.txt", Name: "log.txt"}, 3, "text/plain")
	second := record(t, s, "u:d1:r3", UploadRow{Session: 7, File: "e-f-a.txt", Name: "a.txt"}, 1, "text/plain")

	got, err := s.SessionUploads(ctx, 7, []int64{second.ID, first.ID, second.ID})
	if err != nil || len(got) != 2 || got[0].ID != second.ID || got[1].ID != first.ID {
		t.Fatalf("SessionUploads = %+v, %v", got, err)
	}
	if _, err := s.SessionUploads(ctx, 7, []int64{first.ID, other.ID}); !errors.Is(err, ErrNotSessions) {
		t.Fatalf("another session's upload was taken: %v", err)
	}
	if _, err := s.SessionUploads(ctx, 7, []int64{999}); !errors.Is(err, ErrNotSessions) {
		t.Fatalf("an upload that is none was taken: %v", err)
	}
}

// A pending upload a stop left is found by its time and its key, and
// dropping it frees its request id; a stored upload stays (Codex review
// of #49, round 1).
func TestAPendingUploadIsFoundAndDropped(t *testing.T) {
	s, _ := open(t)
	if _, err := s.InsertSession(ctx, SessionRow{ID: 7, Title: "t", Cwd: "/w", Provider: "p", Model: "m", CreatedBy: "d1"}); err != nil {
		t.Fatal(err)
	}
	left, _, _, err := s.ClaimUpload(ctx, "u:d1:left", "upload:7", UploadRow{Session: 7, File: "a-b-cut.bin", Name: "cut.bin"})
	if err != nil {
		t.Fatal(err)
	}
	stored := record(t, s, "u:d1:done", UploadRow{Session: 7, File: "c-d-whole.bin", Name: "whole.bin"}, 5, "application/octet-stream")
	found, err := s.PendingUploads(ctx, s.now().Add(time.Hour))
	if err != nil || len(found) != 1 || found[0] != (PendingUpload{Key: "u:d1:left", Session: 7, File: "a-b-cut.bin"}) {
		t.Fatalf("PendingUploads = %+v, %v", found, err)
	}
	if none, err := s.PendingUploads(ctx, left.CreatedAt); err != nil || len(none) != 0 {
		t.Fatalf("an upload of this run is left: %+v, %v", none, err)
	}
	if p, err := s.PendingUploadOf(ctx, "u:d1:left"); err != nil || p.File != "a-b-cut.bin" {
		t.Fatalf("PendingUploadOf = %+v, %v", p, err)
	}
	// A drop of another file under the key changes nothing.
	if err := s.DropUpload(ctx, "u:d1:left", "x-y-other.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingUploadOf(ctx, "u:d1:left"); err != nil {
		t.Fatalf("a drop of another file dropped the upload: %v", err)
	}
	for key, file := range map[string]string{"u:d1:left": "a-b-cut.bin", "u:d1:done": "c-d-whole.bin"} {
		if err := s.DropUpload(ctx, key, file); err != nil {
			t.Fatal(err)
		}
	}
	if _, existed, err := s.LookupRequest(ctx, "u:d1:left", "upload:7"); err != nil || existed {
		t.Fatalf("the dropped upload's request id is held: %v, %v", existed, err)
	}
	if _, err := s.Upload(ctx, stored.ID); err != nil {
		t.Fatalf("a stored upload was dropped: %v", err)
	}
}
