package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// usageSession makes a session on the harness's model whose transcript is
// entries.
func (h *harness) usageSession(key string, entries ...proto.Entry) int64 {
	h.t.Helper()
	s, err := h.client.CreateSession(context.Background(), proto.CreateSession{RequestID: key, Cwd: h.work, Model: model})
	if err != nil {
		h.t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if h.engine.transcripts == nil {
		h.engine.transcripts = map[int64][]proto.Entry{}
	}
	for i := range entries {
		entries[i].SessionID = s.ID
	}
	h.engine.transcripts[s.ID] = entries
	return s.ID
}

func answerAt(id int64, at time.Time, m *proto.ModelRef, u proto.Usage) proto.Entry {
	return proto.Entry{ID: id, Kind: proto.EntryAssistant, Model: m, Usage: &u, At: at}
}

// GET /v1/usage sums the sessions' answers by day in the service's zone,
// and by model, the session's for an answer that names none; a question,
// a tool's result and an answer before since count nowhere (rule 63).
func TestUsageSumsAnswersByDayAndModelInTheServicesZone(t *testing.T) {
	h := newHarness(t)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	h.server.SetZone(ny)
	deepseek := &proto.ModelRef{Provider: "deepseek", ID: "deepseek-v4"}
	opus := &proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	utc := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC) }
	h.usageSession("r1",
		answerAt(1, utc(10, 12), &model, proto.Usage{Input: 999, Total: 999}),
		proto.Entry{ID: 2, Kind: proto.EntryUser, Text: "Go on.", At: utc(18, 2)},
		// 22:00 on the 17th in New York, though the 18th in UTC.
		answerAt(3, utc(18, 3), &model, proto.Usage{Input: 100, Output: 10, Total: 110}),
		proto.Entry{ID: 4, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "c", Name: "bash", Text: "ok"}, At: utc(18, 3)},
		answerAt(5, utc(18, 15), deepseek, proto.Usage{Input: 40, Output: 10, Total: 50, Cost: 0.5}),
		answerAt(6, utc(18, 16), nil, proto.Usage{Output: 7, Total: 7}),
	)
	h.usageSession("r2",
		answerAt(1, utc(18, 20), opus, proto.Usage{Input: 5, CacheRead: 1000, Total: 1005}),
		answerAt(2, utc(18, 21), deepseek, proto.Usage{Input: 10, Total: 10, Cost: 0.25}),
	)
	r, err := h.client.Usage(context.Background(), "2026-11-12")
	if err != nil {
		t.Fatal(err)
	}
	want := proto.UsageReport{Zone: "America/New_York", Today: "2026-11-18", TodayEnds: utc(19, 5), Since: "2026-11-12",
		Days: []proto.UsageDay{
			{Day: "2026-11-17", Models: []proto.ModelUsage{{Model: model, Usage: proto.Usage{Input: 100, Output: 10, Total: 110}}}},
			{Day: "2026-11-18", Models: []proto.ModelUsage{
				{Model: *opus, Usage: proto.Usage{Input: 5, CacheRead: 1000, Total: 1005}},
				{Model: *deepseek, Usage: proto.Usage{Input: 50, Output: 10, Total: 60, Cost: 0.75}},
				{Model: model, Usage: proto.Usage{Output: 7, Total: 7}},
			}},
		}}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("report\n%+v\nwant\n%+v", r, want)
	}
}

// A since is a day, YYYY-MM-DD, at most 91 days before the service's
// today; any other is bad_request, and a day after today has no usage yet
// (rule 63).
func TestUsageTakesOnlyADayItCanSum(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, since := range []string{"", "yesterday", "2026-11-1", "2026-11-18T00:00:00Z", "2026-13-01", "2026-08-18"} {
		_, err := h.client.Usage(ctx, since)
		asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	}
	// The harness's today is 2026-11-18 in UTC: 91 days back is the 19th
	// of August.
	if r, err := h.client.Usage(ctx, "2026-08-19"); err != nil || r.Since != "2026-08-19" {
		t.Fatalf("the oldest since: %+v, %v", r, err)
	}
	if r, err := h.client.Usage(ctx, "2026-11-20"); err != nil || len(r.Days) != 0 || r.Days == nil {
		t.Fatalf("a since after today: %+v, %v", r, err)
	}
}

// Each answer is read once: a later report reads each session on after the
// newest entry the last one read, from the day that one began at, and
// reads a session from the start again only for an earlier since; a
// session not updated since since is not read (ADR-0052).
func TestUsageReadsEachAnswerOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	today := h.clock.Now()
	id := h.usageSession("r1", answerAt(1, today, &model, proto.Usage{Output: 1, Total: 1}))
	if _, err := h.client.Usage(ctx, "2026-11-15"); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	h.engine.transcripts[id] = append(h.engine.transcripts[id], answerAt(2, today, &model, proto.Usage{Output: 2, Total: 2}))
	h.engine.mu.Unlock()
	r, err := h.client.Usage(ctx, "2026-11-17")
	if err != nil || len(r.Days) != 1 || r.Days[0].Models[0].Usage.Output != 3 {
		t.Fatalf("report %+v, %v", r, err)
	}
	if _, err := h.client.Usage(ctx, "2026-11-10"); err != nil {
		t.Fatal(err)
	}
	from := func(day int) time.Time { return time.Date(2026, 11, day, 0, 0, 0, 0, time.UTC) }
	want := []usageRead{{id: id, after: 0, since: from(15)}, {id: id, after: 1, since: from(15)}, {id: id, after: 0, since: from(10)}}
	h.engine.mu.Lock()
	got := h.engine.usageReads
	h.engine.mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reads %+v, want %+v", got, want)
	}
	// The session's time is the harness's now: a since after it skips it.
	if _, err := h.client.Usage(ctx, "2026-11-19"); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.usageReads) != len(want) {
		t.Fatalf("a session not updated since since was read: %+v", h.engine.usageReads)
	}
}

// A read that fails fails the report with nothing kept of it, and the next
// report reads the session as the first would have (ADR-0052).
func TestAUsageReadThatFailsIsReadAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.usageSession("r1", answerAt(1, h.clock.Now(), &model, proto.Usage{Output: 4, Total: 4}))
	h.engine.mu.Lock()
	h.engine.usageReadErr = errors.New("database is locked")
	h.engine.mu.Unlock()
	_, err := h.client.Usage(ctx, "2026-11-18")
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	r, err := h.client.Usage(ctx, "2026-11-18")
	if err != nil || len(r.Days) != 1 || r.Days[0].Models[0].Usage.Output != 4 {
		t.Fatalf("report %+v, %v", r, err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if last := h.engine.usageReads[len(h.engine.usageReads)-1]; last.id != id || last.after != 0 {
		t.Fatalf("the read after a failure %+v", last)
	}
}

// The sums kept of a session hold no day older than a report can ask for:
// a pocketd that runs for months keeps at most proto.MaxUsageDays of them.
func TestUsageKeepsNoDayOlderThanAReportAsksFor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.usageSession("r1",
		answerAt(1, time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC), &model, proto.Usage{Output: 1, Total: 1}),
		answerAt(2, h.clock.Now(), &model, proto.Usage{Output: 2, Total: 2}))
	if _, err := h.client.Usage(ctx, "2026-08-19"); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(24 * time.Hour)
	if err := h.store.TouchSession(ctx, id, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	r, err := h.client.Usage(ctx, "2026-08-20")
	if err != nil || len(r.Days) != 1 || r.Days[0].Day != "2026-11-18" || r.Today != "2026-11-19" {
		t.Fatalf("report %+v, %v", r, err)
	}
	h.server.usageMu.Lock()
	defer h.server.usageMu.Unlock()
	held := h.server.usageSums[id]
	if _, ok := held.days["2026-08-19"]; ok || !held.from.Equal(time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("kept %+v from %s", held.days, held.from)
	}
}
