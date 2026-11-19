package pig

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
// only through the proto client: create a session, send a message, see the
// approval arrive as an event, approve it, and see the run finish.
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
	await := func(what string, match func(proto.Event) bool) proto.Event {
		t.Helper()
		for {
			select {
			case e := <-stream:
				t.Logf("event %s %s", e.Type, summarize(e))
				if match(e) {
					return e
				}
			case <-streamCtx.Done():
				t.Fatalf("no %s event", what)
			}
		}
	}

	models, err := client.Models(ctx)
	if err != nil || len(models) != 1 || models[0].ModelRef != fauxRef {
		t.Fatalf("models %+v %v", models, err)
	}
	for hub.Subscribers() == 0 { // the stream has subscribed before the first event
		runtime.Gosched()
	}
	session, err := client.CreateSession(ctx, proto.CreateSession{RequestID: proto.NewRequestID(), Cwd: work, Model: fauxRef, Title: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	await("session created", func(e proto.Event) bool { return e.Type == proto.EventSession && e.Session.ID == session.ID })
	if _, err := client.Send(ctx, session.ID, proto.SendMessage{RequestID: proto.NewRequestID(), Text: "write out.txt"}); err != nil {
		t.Fatal(err)
	}
	requested := await("approval requested", func(e proto.Event) bool {
		return e.Type == proto.EventApproval && e.Approval.State == proto.ApprovalPending
	})
	await("session waiting", func(e proto.Event) bool {
		return e.Type == proto.EventSession && e.Session.State == proto.SessionWaiting
	})
	if requested.Approval.Tool != "bash" || requested.Approval.SessionID != session.ID {
		t.Fatalf("approval %+v", requested.Approval)
	}
	if _, err := client.Decide(ctx, requested.Approval.ID, proto.Decide{RequestID: proto.NewRequestID(), Allow: true}); err != nil {
		t.Fatal(err)
	}
	final := await("answer entry", func(e proto.Event) bool {
		return e.Type == proto.EventEntry && e.Entry.Kind == proto.EntryAssistant && e.Entry.Text == "Ran it."
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
