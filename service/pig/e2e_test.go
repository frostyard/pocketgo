package pig

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
	"github.com/frostyard/pocketgo/service/internal/approvals"
	"github.com/frostyard/pocketgo/service/internal/auth"
	"github.com/frostyard/pocketgo/service/internal/events"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// TestEndToEndOverHTTP wires the service as pocketd serve does and drives it
// only through the proto client: open the stream at its hello, create a
// session, send a message and see it come back with its submission, see the
// approval arrive as an event, approve it, and see the run finish with the
// session's usage.
func TestEndToEndOverHTTP(t *testing.T) {
	ctx := context.Background()
	dataDir, work := t.TempDir(), t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dataDir, "work.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_, token, err := auth.CreateDevice(ctx, s, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	hub, err := events.NewHub(256, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := api.New(s, hub, "e2e", nil, nil)
	manager := approvals.New(s, server.PublishApproval)
	server.SetApprovals(manager)
	engine, err := open(ctx, Options{
		Protected: []string{dataDir},
		DataDir:   dataDir, Approver: manager, NeedsApproval: approveChanges, Unsandboxed: true,
		OnEntry: server.PublishEntry, OnLive: server.PublishLive,
	}, fauxModels(toolTurn("bash", map[string]any{"command": "echo done > out.txt"}, "b1"), answer("Ran it.")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close(context.Background()) }()
	server.SetEngine(engine)
	engine.Resume()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, _ := proto.NewClient(httpServer.URL, token, nil)

	streamCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stream := make(chan proto.Event, 256)
	go func() {
		_ = client.Events(streamCtx, 0, func(e proto.Event) error { stream <- e; return nil })
	}()
	// Events are kept as they come: find looks for one from index from on,
	// so events of producers the protocol does not order (the watch's
	// entries and the tool hook's approvals) are found in either order.
	var seen []proto.Event
	find := func(what string, from int, match func(proto.Event) bool) (proto.Event, int) {
		t.Helper()
		for i := from; ; i++ {
			for i >= len(seen) {
				select {
				case e := <-stream:
					t.Logf("event %s %s", e.Type, summarize(e))
					seen = append(seen, e)
				case <-streamCtx.Done():
					t.Fatalf("no %s event", what)
				}
			}
			if match(seen[i]) {
				return seen[i], i
			}
		}
	}
	// await finds the next match after the last one awaited.
	next := 0
	await := func(what string, match func(proto.Event) bool) proto.Event {
		t.Helper()
		e, i := find(what, next, match)
		next = i + 1
		return e
	}

	// The hello also says the stream has subscribed before the first event.
	await("hello", func(e proto.Event) bool { return e.Type == proto.EventHello && e.Seq != 0 })
	models, err := client.Models(ctx)
	if err != nil || len(models) != 1 || models[0].ModelRef != fauxRef || len(models[0].ThinkingLevels) == 0 {
		t.Fatalf("models %+v %v", models, err)
	}
	session, err := client.CreateSession(ctx, proto.CreateSession{RequestID: proto.NewRequestID(), Cwd: work, Model: fauxRef, Title: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	await("session created", func(e proto.Event) bool { return e.Type == proto.EventSession && e.Session.ID == session.ID })
	sent, err := client.Send(ctx, session.ID, proto.SendMessage{RequestID: proto.NewRequestID(), Text: "write out.txt"})
	if err != nil {
		t.Fatal(err)
	}
	// The watch's entries, the tool hook's approval and the session event
	// either sends come in no set order: find each from the send on.
	sentAt := next
	_, entryAt := find("the message's entry", sentAt, func(e proto.Event) bool {
		return e.Type == proto.EventEntry && e.Entry.Kind == proto.EntryUser && e.Entry.SubmissionID == sent.SubmissionID
	})
	requested, approvalAt := find("approval requested", sentAt, func(e proto.Event) bool {
		return e.Type == proto.EventApproval && e.Approval.State == proto.ApprovalPending
	})
	_, waitingAt := find("session waiting", sentAt, func(e proto.Event) bool {
		return e.Type == proto.EventSession && e.Session.State == proto.SessionWaiting
	})
	next = max(entryAt, approvalAt, waitingAt) + 1
	if requested.Approval.Tool != "bash" || requested.Approval.SessionID != session.ID {
		t.Fatalf("approval %+v", requested.Approval)
	}
	if _, err := client.Decide(ctx, requested.Approval.ID, proto.Decide{RequestID: proto.NewRequestID(), Allow: true}); err != nil {
		t.Fatal(err)
	}
	final := await("answer entry", func(e proto.Event) bool {
		return e.Type == proto.EventEntry && e.Entry.Kind == proto.EntryAssistant && e.Entry.Text == "Ran it."
	})
	// The answer's session follows it, with the usage the answer added.
	await("the answer's session", func(e proto.Event) bool {
		return e.Type == proto.EventSession && e.Session.ID == session.ID && e.Session.Usage.Output > 0
	})
	await("session idle", func(e proto.Event) bool {
		return e.Type == proto.EventSession && e.Session.ID == session.ID && e.Session.State == proto.SessionIdle
	})
	if data, err := os.ReadFile(filepath.Join(work, "out.txt")); err != nil || string(data) != "done\n" {
		t.Fatalf("out.txt %q, %v", data, err)
	}
	detail, err := client.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(detail.Entries); got != "user,assistant,toolResult,assistant" || detail.Entries[len(detail.Entries)-1].ID != final.Entry.ID {
		t.Fatalf("detail kinds %s, last %+v", got, detail.Entries[len(detail.Entries)-1])
	}
	if detail.Session.Title != "e2e" || detail.Session.Usage.Output == 0 || detail.Live.Busy {
		t.Fatalf("detail session %+v live %+v", detail.Session, detail.Live)
	}
	_, err = client.Decide(ctx, requested.Approval.ID, proto.Decide{RequestID: proto.NewRequestID(), Allow: false})
	var e *proto.Error
	if !errors.As(err, &e) || e.Code != proto.ErrorCodeConflict {
		t.Fatalf("deciding again: %v", err)
	}
}

func summarize(e proto.Event) string {
	switch {
	case e.Entry != nil:
		return string(e.Entry.Kind) + " " + e.Entry.Text + " " + e.Entry.Error
	case e.Live != nil:
		return "busy=" + boolText(e.Live.Busy) + " failure=" + e.Live.Failure
	case e.Session != nil:
		return string(e.Session.State)
	case e.Approval != nil:
		return string(e.Approval.State)
	}
	return ""
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestEndToEndWithoutApprovalsOverHTTP drives pocketd's own rule through the
// protocol (ADR-0012): a bash call runs at once, the run finishes, no
// approval event arrives and the Inbox's list stays empty.
func TestEndToEndWithoutApprovalsOverHTTP(t *testing.T) {
	ctx := context.Background()
	dataDir, work := t.TempDir(), t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dataDir, "work.sqlite"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	_, token, err := auth.CreateDevice(ctx, s, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	hub, err := events.NewHub(256, time.Now(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := api.New(s, hub, "e2e", nil, nil)
	manager := approvals.New(s, server.PublishApproval)
	server.SetApprovals(manager)
	engine, err := open(ctx, Options{
		Protected: []string{dataDir},
		DataDir:   dataDir, Approver: manager, NeedsApproval: approvals.NeedsApproval, Unsandboxed: true,
		OnEntry: server.PublishEntry, OnLive: server.PublishLive,
	}, fauxModels(toolTurn("bash", map[string]any{"command": "echo done > out.txt"}, "b1"), answer("Ran it.")))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close(context.Background()) }()
	server.SetEngine(engine)
	engine.Resume()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client, _ := proto.NewClient(httpServer.URL, token, nil)

	streamCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stream := make(chan proto.Event, 256)
	go func() {
		_ = client.Events(streamCtx, 0, func(e proto.Event) error { stream <- e; return nil })
	}()
	var approvalsSeen []proto.Approval
	await := func(what string, match func(proto.Event) bool) {
		t.Helper()
		for {
			select {
			case e := <-stream:
				if e.Type == proto.EventApproval {
					approvalsSeen = append(approvalsSeen, *e.Approval)
				}
				if match(e) {
					return
				}
			case <-streamCtx.Done():
				t.Fatalf("no %s event", what)
			}
		}
	}

	await("hello", func(e proto.Event) bool { return e.Type == proto.EventHello })
	session, err := client.CreateSession(ctx, proto.CreateSession{RequestID: proto.NewRequestID(), Cwd: work, Model: fauxRef, Title: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send(ctx, session.ID, proto.SendMessage{RequestID: proto.NewRequestID(), Text: "write out.txt"}); err != nil {
		t.Fatal(err)
	}
	await("answer entry", func(e proto.Event) bool {
		return e.Type == proto.EventEntry && e.Entry.Kind == proto.EntryAssistant && e.Entry.Text == "Ran it."
	})
	await("session idle", func(e proto.Event) bool {
		return e.Type == proto.EventSession && e.Session.ID == session.ID && e.Session.State == proto.SessionIdle
	})
	if len(approvalsSeen) != 0 {
		t.Fatalf("approval events %+v", approvalsSeen)
	}
	if pending, err := client.Approvals(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending approvals %+v, %v", pending, err)
	}
	if data, err := os.ReadFile(filepath.Join(work, "out.txt")); err != nil || string(data) != "done\n" {
		t.Fatalf("out.txt %q, %v", data, err)
	}
}
