package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

var ctx = context.Background()

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Millisecond); return c.t }

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "work.sqlite")
	c := &clock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	s, err := Open(ctx, path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestOpenMigratesAndReopens(t *testing.T) {
	s, path := open(t)
	var version int
	if err := s.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("version %d, %v", version, err)
	}
	if _, err := s.InsertSession(ctx, SessionRow{ID: 5, Title: "t", Cwd: "/w", Provider: "p", Model: "m", CreatedBy: "d1"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	again, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	row, err := again.Session(ctx, 5)
	if err != nil || row.Title != "t" {
		t.Fatalf("session after reopen %+v, %v", row, err)
	}
}

func TestOpenRefusesANewerSchema(t *testing.T) {
	s, path := open(t)
	if _, err := s.db.Exec(`UPDATE schema_version SET version = ?`, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(ctx, path, nil); err == nil {
		t.Fatal("a newer schema opened")
	}
}

func TestDevices(t *testing.T) {
	s, _ := open(t)
	d, err := s.CreateDevice(ctx, "laptop", "h1")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.DeviceByTokenHash(ctx, "h1"); err != nil || got.ID != d.ID {
		t.Fatalf("by hash %+v, %v", got, err)
	}
	if _, err := s.CreateDevice(ctx, "laptop", "h2"); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if err := s.RevokeDevice(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeviceByTokenHash(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked device still signs in: %v", err)
	}
	if err := s.RevokeDevice(ctx, "laptop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
	devices, _ := s.Devices(ctx)
	if len(devices) != 1 || devices[0].RevokedAt == nil {
		t.Fatalf("devices %+v", devices)
	}
}

func TestSessionsOrderAndTouch(t *testing.T) {
	s, _ := open(t)
	for _, id := range []int64{1, 2} {
		if _, err := s.InsertSession(ctx, SessionRow{ID: id, Title: "s", Cwd: "/w", Provider: "p", Model: "m", CreatedBy: "d"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TouchSession(ctx, 1); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.Sessions(ctx)
	if len(rows) != 2 || rows[0].ID != 1 {
		t.Fatalf("most recently updated first: %+v", rows)
	}
	if _, err := s.Session(ctx, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
}

func TestRequestsKeepTheFirstResult(t *testing.T) {
	s, _ := open(t)
	if _, _, ok, _ := s.Request(ctx, "k"); ok {
		t.Fatal("new key found")
	}
	_ = s.SaveRequest(ctx, "k", "session", []byte(`{"id":1}`))
	_ = s.SaveRequest(ctx, "k", "session", []byte(`{"id":2}`))
	kind, result, ok, err := s.Request(ctx, "k")
	if !ok || err != nil || kind != "session" || string(result) != `{"id":1}` {
		t.Fatalf("request %q %s %v %v", kind, result, ok, err)
	}
}

func TestApprovalsAreIdempotentPerCallAndDecidedOnce(t *testing.T) {
	s, _ := open(t)
	row := ApprovalRow{SessionID: 3, TaskID: 13, CallID: "w1", Tool: "write", Arguments: []byte(`{"path":"a"}`)}
	first, created, err := s.RequestApproval(ctx, row)
	if err != nil || !created || first.State != "pending" || first.ID != ApprovalID(13, "w1") {
		t.Fatalf("first %+v %v %v", first, created, err)
	}
	again, created, err := s.RequestApproval(ctx, row)
	if err != nil || created || again.ID != first.ID || !again.RequestedAt.Equal(first.RequestedAt) {
		t.Fatalf("the same call asked again: %+v %v %v", again, created, err)
	}
	pending, _ := s.PendingApprovals(ctx)
	if len(pending) != 1 {
		t.Fatalf("pending %+v", pending)
	}
	decided, err := s.DecideApproval(ctx, first.ID, true, "laptop", "ok")
	if err != nil || decided.State != "allowed" || decided.DecidedAt == nil || decided.DecidedBy != "laptop" {
		t.Fatalf("decided %+v %v", decided, err)
	}
	if _, err := s.DecideApproval(ctx, first.ID, false, "phone", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("second decision: %v", err)
	}
	if _, err := s.DecideApproval(ctx, "nope", true, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown approval: %v", err)
	}
	asked, created, _ := s.RequestApproval(ctx, row)
	if created || asked.State != "allowed" {
		t.Fatalf("a decided call asked again returns the decision: %+v", asked)
	}
	if pending, _ := s.PendingApprovals(ctx); len(pending) != 0 {
		t.Fatalf("pending after decision %+v", pending)
	}
}
