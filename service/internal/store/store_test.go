package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

var ctx = context.Background()

type clock struct{ t time.Time }

func (c *clock) now() time.Time { c.t = c.t.Add(time.Millisecond); return c.t }

var migratedTemplate = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "storetest-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "w.sqlite")
	s, err := Open(context.Background(), path, nil)
	if err != nil {
		return nil, err
	}
	if err := s.Close(); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path + "-wal"); err == nil {
		if info.Size() > 0 {
			return nil, fmt.Errorf("template has uncheckpointed WAL: %s", path)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return os.ReadFile(path)
})

func copyTemplate(t *testing.T, path string) {
	t.Helper()
	data, err := migratedTemplate()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "work.sqlite")
	copyTemplate(t, path)
	return openPath(t, path)
}

func openFresh(t *testing.T) (*Store, string) {
	t.Helper()
	return openPath(t, filepath.Join(t.TempDir(), "work.sqlite"))
}

func openPath(t *testing.T, path string) (*Store, string) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	s, err := Open(ctx, path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestOpenMigratesAndReopens(t *testing.T) {
	s, path := openFresh(t)
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
	s, path := openFresh(t)
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
	if err := s.TouchSession(ctx, 1, time.Now().Add(time.Hour)); err != nil {
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

func TestClaimRequestBindsKindAndReplays(t *testing.T) {
	s, _ := open(t)
	result, existed, err := s.ClaimRequest(ctx, "k", "abort:3")
	if err != nil || existed || result != "" {
		t.Fatalf("first claim %q %v %v", result, existed, err)
	}
	if err := s.CompleteRequest(ctx, "k", "done"); err != nil {
		t.Fatal(err)
	}
	result, existed, err = s.ClaimRequest(ctx, "k", "abort:3")
	if err != nil || !existed || result != "done" {
		t.Fatalf("retry %q %v %v", result, existed, err)
	}
	if _, _, err := s.ClaimRequest(ctx, "k", "abort:4"); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("another target: %v", err)
	}
}

func TestCreateSessionRecordsTheRequestWithTheRow(t *testing.T) {
	s, _ := open(t)
	if _, err := s.CreateSession(ctx, "k1", SessionRow{ID: 7, Title: "t", Cwd: "/w", Provider: "p", Model: "m",
		Profile: "odrade", Route: "profile-alias", CreatedBy: "d"}); err != nil {
		t.Fatal(err)
	}
	// A session keeps the profile it started from and its model's route.
	if row, err := s.Session(ctx, 7); err != nil || row.Profile != "odrade" || row.Route != "profile-alias" {
		t.Fatalf("session's profile and route %+v, %v", row, err)
	}
	result, existed, err := s.ClaimRequest(ctx, "k1", "createSession")
	if err != nil || !existed || result != "7" {
		t.Fatalf("request after create %q %v %v", result, existed, err)
	}
	// A claimed but incomplete create completes in the same transaction.
	if _, _, err := s.ClaimRequest(ctx, "k3", "createSession"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, "k3", SessionRow{ID: 8, Title: "t", Cwd: "/w", Provider: "p", Model: "m", CreatedBy: "d"}); err != nil {
		t.Fatal(err)
	}
	if result, _, _ := s.ClaimRequest(ctx, "k3", "createSession"); result != "8" {
		t.Fatalf("completed claim %q", result)
	}
}

func TestReserveSeqGrowsWhenTheClockMovesBack(t *testing.T) {
	s, _ := open(t)
	first, err := s.ReserveSeq(ctx, 1000, 100)
	if err != nil || first != 1000 {
		t.Fatalf("first %d %v", first, err)
	}
	second, err := s.ReserveSeq(ctx, 500, 100)
	if err != nil || second != 1100 {
		t.Fatalf("after a clock step back %d %v", second, err)
	}
	third, _ := s.ReserveSeq(ctx, 5000, 100)
	if third != 5000 {
		t.Fatalf("a later floor wins: %d", third)
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
	if has, _ := s.HasPendingApproval(ctx, 3); !has {
		t.Fatal("session 3 has a pending approval")
	}
	if ids, _ := s.PendingSessions(ctx); !ids[3] || len(ids) != 1 {
		t.Fatalf("pending sessions %v", ids)
	}
	decided, replayed, err := s.DecideApproval(ctx, "k1", first.ID, true, "laptop", "ok")
	if err != nil || replayed || decided.State != "allowed" || decided.DecidedAt == nil || decided.DecidedBy != "laptop" {
		t.Fatalf("decided %+v %v %v", decided, replayed, err)
	}
	retry, replayed, err := s.DecideApproval(ctx, "k1", first.ID, true, "laptop", "ok")
	if err != nil || !replayed || retry.State != "allowed" {
		t.Fatalf("retry %+v %v %v", retry, replayed, err)
	}
	if _, _, err := s.DecideApproval(ctx, "k2", first.ID, false, "phone", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("second decision: %v", err)
	}
	if _, existed, _ := s.ClaimRequest(ctx, "k2", "decide:"+first.ID); existed {
		t.Fatal("a refused decision left its request record")
	}
	if _, _, err := s.DecideApproval(ctx, "k1", "aother", true, "x", ""); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("a key reused for another approval: %v", err)
	}
	if _, _, err := s.DecideApproval(ctx, "k3", "nope", true, "x", ""); !errors.Is(err, ErrNotFound) {
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

func TestRetireApproval(t *testing.T) {
	s, _ := open(t)
	b, _, _ := s.RequestApproval(ctx, ApprovalRow{SessionID: 2, TaskID: 2, CallID: "y", Tool: "bash", Arguments: []byte(`{}`)})
	if row, err := s.RetireApproval(ctx, b.ID, "the task ended"); err != nil || row.Reason != "the task ended" || row.DecidedBy != "pocketd" || row.State != "denied" {
		t.Fatalf("retire one %+v %v", row, err)
	}
	if _, err := s.RetireApproval(ctx, b.ID, "again"); !errors.Is(err, ErrConflict) {
		t.Fatalf("retire twice: %v", err)
	}
}

func TestLookupRecordAndDeleteRequests(t *testing.T) {
	s, _ := open(t)
	if _, existed, err := s.LookupRequest(ctx, "k", "send:1"); existed || err != nil {
		t.Fatalf("a lookup of a new key: %v %v", existed, err)
	}
	if _, existed, _ := s.LookupRequest(ctx, "k", "send:1"); existed {
		t.Fatal("a lookup wrote a record")
	}
	_ = s.RecordRequest(ctx, "k", "send:1", "")
	if _, existed, err := s.LookupRequest(ctx, "k", "send:1"); !existed || err != nil {
		t.Fatalf("after record: %v %v", existed, err)
	}
	if _, _, err := s.LookupRequest(ctx, "k", "abort:1"); !errors.Is(err, ErrRequestReused) {
		t.Fatalf("another kind: %v", err)
	}
	_ = s.DeleteRequest(ctx, "k")
	if _, existed, _ := s.LookupRequest(ctx, "k", "abort:1"); existed {
		t.Fatal("a deleted key still recorded")
	}
}

func TestCreateSessionKeepsARowRecordedBefore(t *testing.T) {
	s, _ := open(t)
	if _, err := s.InsertSession(ctx, SessionRow{ID: 9, Title: "first", Cwd: "/w", Provider: "p", Model: "m", CreatedBy: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, "k", SessionRow{ID: 9, Title: "second", Cwd: "/w", Provider: "p", Model: "m", CreatedBy: "d"}); err != nil {
		t.Fatal(err)
	}
	row, _ := s.Session(ctx, 9)
	if row.Title != "first" {
		t.Fatalf("row %+v", row)
	}
	if result, _, _ := s.LookupRequest(ctx, "k", "createSession"); result != "9" {
		t.Fatalf("request %q", result)
	}
}

func TestOpenAtOnceMigratesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work.sqlite")
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			s, err := Open(ctx, path, nil)
			if err == nil {
				_ = s.Close()
			}
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent open: %v", err)
		}
	}
}

func TestOpenEscapesThePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "odd?name#here")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, filepath.Join(dir, "work.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := os.Stat(filepath.Join(dir, "work.sqlite")); err != nil {
		t.Fatalf("the database is not where the path says: %v", err)
	}
}

type codedError int

func (e codedError) Error() string { return "sqlite error" }
func (e codedError) Code() int     { return int(e) }

func TestRetryBusy(t *testing.T) {
	busy := codedError(5 | 2<<8) // SQLITE_BUSY_SNAPSHOT
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		err := retryBusy(ctx, func(context.Context) error {
			calls++
			if calls < 3 {
				return busy
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Fatalf("immediate conflicts: %v after %d calls", err, calls)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		other := codedError(1)
		if err := retryBusy(ctx, func(context.Context) error { calls++; return other }); err != other || calls != 1 {
			t.Fatalf("a non-busy error was retried: %v after %d calls", err, calls)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		err := retryBusy(ctx, func(context.Context) error {
			calls++
			time.Sleep(5 * time.Second) // waited out busy_timeout
			return busy
		})
		if err != busy || calls != 1 {
			t.Fatalf("a slow conflict was retried: %v after %d calls", err, calls)
		}
	})
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0
		err := retryBusy(ctx, func(context.Context) error { calls++; return busy })
		if err != busy || calls < 2 || time.Since(start) > busyBudget {
			t.Fatalf("persistent conflicts: %v after %d calls and %v", err, calls, time.Since(start))
		}
	})
}

// Sessions from before profiles get the route owner: their owner named
// every model.
func TestMigrationThreeGivesOldSessionsTheOwnerRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.sqlite")
	saved := migrations
	migrations = saved[:2]
	old, err := Open(ctx, path, nil)
	if err != nil {
		migrations = saved
		t.Fatal(err)
	}
	_, err = old.db.ExecContext(ctx, `INSERT INTO sessions (id, title, cwd, provider, model, thinking, created_by, created_at, updated_at)
		VALUES (4, 't', '/w', 'p', 'm', '', 'd', 0, 0)`)
	_ = old.Close()
	migrations = saved
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if row, err := s.Session(ctx, 4); err != nil || row.Route != "owner" || row.Profile != "" {
		t.Fatalf("an old session after the migration %+v, %v", row, err)
	}
}
