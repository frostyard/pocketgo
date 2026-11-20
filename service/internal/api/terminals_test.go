//go:build linux

package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
	"github.com/frostyard/pocketgo/service/internal/terminal"
)

// shells gives the harness's server real shells (/bin/sh) in its work
// folder, on its clock: a poll waits for output or its request, never for
// a timer, and an ended shell's SIGKILL runs at once. It returns the
// events the server sent.
func shells(h *harness) func() []proto.Event {
	var mu sync.Mutex
	var sent []proto.Event
	h.server.emit = func(e proto.Event) {
		mu.Lock()
		sent = append(sent, e)
		mu.Unlock()
		h.hub.Publish(e)
	}
	h.server.terms = terminal.New(terminal.Options{Shell: "/bin/sh", Home: h.work, Now: h.clock.Now,
		Env:   terminal.Environment([]string{"PATH=" + os.Getenv("PATH")}, h.work),
		After: func(time.Duration) <-chan time.Time { return nil },
		Timer: func(d time.Duration, f func()) func() {
			if d != terminal.Tick {
				go f()
			}
			return func() {}
		},
		OnChange: h.server.publishTerminals})
	return func() []proto.Event {
		mu.Lock()
		defer mu.Unlock()
		var out []proto.Event
		for _, e := range sent {
			if e.Type == proto.EventTerminals {
				out = append(out, e)
			}
		}
		return out
	}
}

// read polls shell id over the protocol until its output holds want.
func readShell(t *testing.T, h *harness, id string, from *int64, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var got []byte
	for !bytes.Contains(got, []byte(want)) {
		out, err := h.client.TerminalOutput(ctx, id, *from, *from, "tab-1", true)
		if err != nil {
			t.Fatalf("output: %v (after %q)", err, got)
		}
		got = append(got, out.Data...)
		*from = out.To
	}
}

func TestATerminalOverTheProtocol(t *testing.T) {
	h := newHarness(t)
	events := shells(h)
	ctx := context.Background()
	started, err := h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "t1", Rows: 30, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}
	// A retry answers the shell the first call started.
	again, err := h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "t1", Rows: 30, Cols: 100})
	if err != nil || again.ID != started.ID {
		t.Fatalf("a retry started %q, %v (first %q)", again.ID, err, started.ID)
	}
	list, err := h.client.Terminals(ctx)
	if err != nil || len(list) != 1 || list[0].ID != started.ID || list[0].Cwd != h.work || list[0].Title != filepath.Base(h.work) {
		t.Fatalf("listed %+v, %v", list, err)
	}
	if sent := events(); len(sent) != 1 || len(sent[0].Terminals) != 1 || sent[0].Terminals[0].ID != started.ID {
		t.Fatalf("the start sent %+v", sent)
	}
	if err := h.client.TerminalInput(ctx, started.ID, proto.TerminalInput{Tab: "tab-1", Seq: 1, Data: []byte("stty size\n")}); err != nil {
		t.Fatal(err)
	}
	var from int64
	readShell(t, h, started.ID, &from, "30 100")
	if err := h.client.ResizeTerminal(ctx, started.ID, proto.ResizeTerminal{Rows: 40, Cols: 120}); err != nil {
		t.Fatal(err)
	}
	if err := h.client.TerminalInput(ctx, started.ID, proto.TerminalInput{Tab: "tab-1", Seq: 2, Data: []byte("stty size\n")}); err != nil {
		t.Fatal(err)
	}
	readShell(t, h, started.ID, &from, "40 120")
	if err := h.client.EndTerminal(ctx, started.ID); err != nil {
		t.Fatal(err)
	}
	if sent := events(); len(sent) != 2 || len(sent[1].Terminals) != 0 {
		t.Fatalf("the end sent %+v", sent)
	}
	_, err = h.client.TerminalOutput(ctx, started.ID, from, from, "", true)
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	asError(t, h.client.EndTerminal(ctx, started.ID), http.StatusNotFound, proto.ErrorCodeNotFound)
	asError(t, h.client.TerminalInput(ctx, started.ID, proto.TerminalInput{Tab: "tab-1", Seq: 3}), http.StatusNotFound, proto.ErrorCodeNotFound)
	// The same request id after the shell ended starts no other: it is
	// not_found, and the client starts another with a new one.
	_, err = h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "t1"})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	if list, _ := h.client.Terminals(ctx); len(list) != 0 {
		t.Fatalf("a late retry started %+v", list)
	}
}

func TestStartTerminalTakesItsFolder(t *testing.T) {
	h := newHarness(t)
	shells(h)
	ctx := context.Background()
	dir := filepath.Join(h.work, "project")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.InsertSession(ctx, store.SessionRow{ID: 7, Title: "s", Cwd: dir, Provider: model.Provider, Model: model.ID}); err != nil {
		t.Fatal(err)
	}
	for i, request := range []proto.StartTerminal{
		{Folder: proto.TerminalSession, Session: 7},
		{Folder: proto.TerminalPath, Path: dir + "/"},
	} {
		request.RequestID = "f" + strconv.Itoa(i)
		started, err := h.client.StartTerminal(ctx, request)
		if err != nil {
			t.Fatalf("%+v: %v", request, err)
		}
		list, _ := h.client.Terminals(ctx)
		if got := list[len(list)-1]; got.ID != started.ID || got.Cwd != dir || got.Title != "project" {
			t.Fatalf("%+v started %+v", request, got)
		}
	}
	for _, request := range []proto.StartTerminal{
		{RequestID: "b1", Folder: "elsewhere"},
		{RequestID: "b2", Folder: proto.TerminalPath, Path: "project"},
		{RequestID: "b3", Folder: proto.TerminalPath, Path: filepath.Join(h.work, "missing")},
		{RequestID: "b4", Folder: proto.TerminalHome, Path: dir},
		{RequestID: "b5", Folder: proto.TerminalSession, Session: 7, Path: dir},
		{RequestID: "b6", Rows: 1},
		{RequestID: "b7", Cols: 1001},
		{},
	} {
		_, err := h.client.StartTerminal(ctx, request)
		asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	}
	_, err := h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "n1", Folder: proto.TerminalSession, Session: 99})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	// A request id of another request is conflict.
	if err := h.store.RecordRequest(ctx, requestKey(deviceRequest(h), "used"), "abort:1", ""); err != nil {
		t.Fatal(err)
	}
	_, err = h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "used"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	for i := range 2 {
		if _, err := h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "c" + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "c5"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

// deviceRequest is a request of the harness's device, for requestKey.
func deviceRequest(h *harness) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, base, nil)
	return r.WithContext(context.WithValue(r.Context(), deviceKey{}, store.Device{ID: h.device}))
}

func TestATerminalsCallsAreChecked(t *testing.T) {
	h := newHarness(t)
	shells(h)
	ctx := context.Background()
	started, err := h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token}
	output := proto.Path(proto.PathTerminalOutput, started.ID)
	for _, query := range []string{"", "?from=x", "?from=-1", "?from=3&ack=4", "?from=3&ack=y", "?from=0&tab=a.b"} {
		if response, e := h.raw("GET", output+query, "", headers); response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest {
			t.Errorf("output%s: %d %+v", query, response.StatusCode, e)
		}
	}
	for _, request := range []proto.TerminalInput{
		{Tab: "", Seq: 1}, {Tab: "tab-1", Seq: 0}, {Tab: "tab-1", Seq: 1, Data: make([]byte, proto.MaxTerminalInput+1)},
	} {
		asError(t, h.client.TerminalInput(ctx, started.ID, request), http.StatusBadRequest, proto.ErrorCodeBadRequest)
	}
	asError(t, h.client.ResizeTerminal(ctx, started.ID, proto.ResizeTerminal{}), http.StatusBadRequest, proto.ErrorCodeBadRequest)
	if response, e := h.raw("PUT", proto.PathTerminals, "", headers); response.StatusCode != http.StatusMethodNotAllowed || e.Code != proto.ErrorCodeBadRequest {
		t.Errorf("PUT: %d %+v", response.StatusCode, e)
	}
	// An exited shell takes no keys and no size.
	if err := h.client.TerminalInput(ctx, started.ID, proto.TerminalInput{Tab: "tab-1", Seq: 1, Data: []byte("exit 3\n")}); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var from int64
	for {
		out, err := h.client.TerminalOutput(waitCtx, started.ID, from, from, "", true)
		if err != nil {
			t.Fatal(err)
		}
		from = out.To
		if out.Exited {
			if out.ExitCode == nil || *out.ExitCode != 3 {
				t.Fatalf("exit code %v", out.ExitCode)
			}
			break
		}
	}
	asError(t, h.client.TerminalInput(ctx, started.ID, proto.TerminalInput{Tab: "tab-1", Seq: 2, Data: []byte("x")}), http.StatusConflict, proto.ErrorCodeConflict)
	asError(t, h.client.ResizeTerminal(ctx, started.ID, proto.ResizeTerminal{Rows: 30, Cols: 90}), http.StatusConflict, proto.ErrorCodeConflict)
	if list, _ := h.client.Terminals(ctx); len(list) != 1 || !list[0].Exited || *list[0].ExitCode != 3 {
		t.Fatalf("listed %+v", list)
	}
}

// Stop ends the owner's shells, so the shutdown waits on no poll, and no
// shell starts after it.
func TestStopEndsTheShells(t *testing.T) {
	h := newHarness(t)
	shells(h)
	ctx := context.Background()
	started, err := h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan error, 1)
	go func() {
		_, err := h.client.TerminalOutput(ctx, started.ID, 1<<40, 0, "", true)
		waiting <- err
	}()
	h.server.Stop()
	select {
	case err := <-waiting:
		asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	case <-time.After(20 * time.Second):
		t.Fatal("a poll outlives Stop")
	}
	if list, _ := h.client.Terminals(ctx); len(list) != 0 {
		t.Fatalf("listed after Stop: %+v", list)
	}
	_, err = h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "t2"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
}

// Codex review of #41, round 1: a shell that started is recorded with its
// request though its client left meanwhile, so a retry finds it rather
// than running a login again.
func TestAStartIsRecordedThoughItsClientLeft(t *testing.T) {
	h := newHarness(t)
	shells(h)
	ctx, cancel := context.WithCancel(context.Background())
	h.server.afterTerminalStart = cancel
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, proto.PathTerminals, strings.NewReader(`{"requestId":"t1"}`))
	request.Header.Set(proto.HeaderProtocol, strconv.Itoa(proto.Version))
	request.Header.Set("Authorization", "Bearer "+h.token)
	h.server.Handler().ServeHTTP(httptest.NewRecorder(), request)
	list, err := h.client.Terminals(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("listed %+v, %v", list, err)
	}
	id, recorded, err := h.store.LookupRequest(context.Background(), requestKey(deviceRequest(h), "t1"), startTerminalKind)
	if err != nil || !recorded || id != list[0].ID {
		t.Fatalf("the start is recorded as %q (%v, %v), want %q", id, recorded, err, list[0].ID)
	}
	h.server.afterTerminalStart = nil
	again, err := h.client.StartTerminal(context.Background(), proto.StartTerminal{RequestID: "t1"})
	if err != nil || again.ID != list[0].ID {
		t.Fatalf("the retry: %q, %v", again.ID, err)
	}
}

// Codex review of #41, round 2: a start's request is recorded before its
// shell starts, so one that cannot be recorded runs no login at all, and
// a retry after a stop never runs a second.
func TestAStartThatCannotBeRecordedRunsNoShell(t *testing.T) {
	h := newHarness(t)
	events := shells(h)
	h.server.recordTerminalStart = func(context.Context, string, string, string) error { return errors.New("the disk is full") }
	_, err := h.client.StartTerminal(context.Background(), proto.StartTerminal{RequestID: "t1"})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	if sent := events(); len(sent) != 0 {
		t.Fatalf("a shell started, and went out as %+v", sent)
	}
	if list, _ := h.client.Terminals(context.Background()); len(list) != 0 {
		t.Fatalf("listed %+v", list)
	}
	// A start that fails frees its request id.
	h.server.recordTerminalStart = nil
	_, err = h.client.StartTerminal(context.Background(), proto.StartTerminal{RequestID: "t2", Folder: proto.TerminalPath, Path: "relative"})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	if _, recorded, _ := h.store.LookupRequest(context.Background(), requestKey(deviceRequest(h), "t2"), startTerminalKind); recorded {
		t.Fatal("a start refused keeps its request id")
	}
	for i := range 4 {
		if _, err := h.client.StartTerminal(context.Background(), proto.StartTerminal{RequestID: "c" + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = h.client.StartTerminal(context.Background(), proto.StartTerminal{RequestID: "full"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if _, recorded, _ := h.store.LookupRequest(context.Background(), requestKey(deviceRequest(h), "full"), startTerminalKind); recorded {
		t.Fatal("a start pocketd refused keeps its request id")
	}
}

// Codex review of #41, round 2: wait=0 answers at once with what pocketd
// keeps, so a client reads a shell's history apart from what comes live.
func TestAPollWithoutWaitAnswersAtOnce(t *testing.T) {
	h := newHarness(t)
	shells(h)
	started, err := h.client.StartTerminal(context.Background(), proto.StartTerminal{RequestID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.client.TerminalOutput(context.Background(), started.ID, 1<<40, 0, "tab-1", false)
	if err != nil || out.From != out.To || out.Exited {
		t.Fatalf("a poll without wait: %+v, %v", out, err)
	}
	headers := map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + h.token}
	if response, e := h.raw("GET", proto.Path(proto.PathTerminalOutput, started.ID)+"?from=0&wait=2", "", headers); response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest {
		t.Fatalf("wait=2: %d %+v", response.StatusCode, e)
	}
}

// Codex review of #41, round 3: a start whose shell did not start, and
// whose request could not be freed, is never answered as a success: a
// retry finds no shell running, and is not_found.
func TestAStartThatFailedIsNoSuccessOnItsRetry(t *testing.T) {
	h := newHarness(t)
	shells(h)
	ctx := context.Background()
	for i := range 4 {
		if _, err := h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "c" + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	h.server.freeTerminalStart = func(context.Context, string) error { return errors.New("the disk is busy") }
	_, err := h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "full"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	_, err = h.client.StartTerminal(ctx, proto.StartTerminal{RequestID: "full"})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
}
