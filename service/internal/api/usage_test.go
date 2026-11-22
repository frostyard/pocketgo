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
// a tool's result and an answer before since count nowhere (rule 64).
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
// (rule 64).
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
// reads a session from the start again only for an earlier since
// (ADR-0052).
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
}

// A session whose time lags its transcript, as a stop between an answer's
// commit and the write of its session's time leaves it, is read for the
// answer all the same (Codex review of #107, round 2).
func TestASessionWhoseTimeLagsItsTranscriptIsRead(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.usageSession("r1", answerAt(1, h.clock.Now(), &model, proto.Usage{Output: 1, Total: 1}))
	// A day on, an answer is in the transcript, and the session's time
	// still says the day before.
	h.clock.Advance(24 * time.Hour)
	h.engine.mu.Lock()
	h.engine.transcripts[id] = append(h.engine.transcripts[id], answerAt(2, h.clock.Now().Add(-time.Hour), &model, proto.Usage{Output: 2, Total: 2}))
	h.engine.mu.Unlock()
	r, err := h.client.Usage(ctx, "2026-11-19")
	if err != nil || len(r.Days) != 1 || r.Days[0].Day != "2026-11-19" || r.Days[0].Models[0].Usage.Output != 2 {
		t.Fatalf("report %+v, %v", r, err)
	}
}

// A session kept by no report and not updated since the oldest day a
// since may name is not read: no report can ask for its answers.
func TestASessionIdleSinceTheOldestDayIsNotRead(t *testing.T) {
	h := newHarness(t)
	h.usageSession("r1", answerAt(1, h.clock.Now(), &model, proto.Usage{Output: 1, Total: 1}))
	h.clock.Advance(proto.MaxUsageDays * 24 * time.Hour)
	if _, err := h.client.Usage(context.Background(), "2027-02-18"); err != nil {
		t.Fatal(err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.usageReads) != 0 {
		t.Fatalf("an idle session was read: %+v", h.engine.usageReads)
	}
}

// An answer's cost is priced at the report, by the provider of the model
// it counts under: an answer that names none counts its session's model's,
// and a provider's sign-in as it is now decides, also for sums kept from
// before (Codex review of #107, round 2).
func TestAnAnswersCostIsPricedAtTheReportByItsProvider(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.usageSession("r1", answerAt(1, h.clock.Now(), nil, proto.Usage{Output: 1, Total: 1, Cost: 0.5}))
	h.engine.mu.Lock()
	h.engine.unmetered = map[string]bool{model.Provider: true}
	h.engine.mu.Unlock()
	if r, err := h.client.Usage(ctx, "2026-11-18"); err != nil || r.Days[0].Models[0].Usage.Cost != 0 || r.Days[0].Models[0].Usage.Total != 1 {
		t.Fatalf("a subscription's report %+v, %v", r, err)
	}
	h.engine.mu.Lock()
	h.engine.unmetered = nil
	h.engine.mu.Unlock()
	if r, err := h.client.Usage(ctx, "2026-11-18"); err != nil || r.Days[0].Models[0].Usage.Cost != 0.5 {
		t.Fatalf("a metered provider's report from the sums kept %+v, %v", r, err)
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

// Answers with and without a reported total sum to every token of both
// (Codex review of #107, round 1).
func TestUsageSumsAnswersWithAndWithoutTotals(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	h.usageSession("r1", answerAt(1, now, &model, proto.Usage{Input: 40}), answerAt(2, now, &model, proto.Usage{Total: 60}))
	r, err := h.client.Usage(context.Background(), "2026-11-18")
	if err != nil || len(r.Days) != 1 || r.Days[0].Models[0].Usage.Total != 100 {
		t.Fatalf("report %+v, %v", r, err)
	}
}

// The sums kept of a session that aged out of every report go, though no
// report reads the session again (Codex review of #107, round 1).
func TestUsageForgetsASessionThatAgedOut(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.usageSession("r1", answerAt(1, h.clock.Now(), &model, proto.Usage{Output: 1, Total: 1}))
	if _, err := h.client.Usage(ctx, "2026-11-18"); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(proto.MaxUsageDays * 24 * time.Hour)
	if _, err := h.client.Usage(ctx, "2027-02-18"); err != nil {
		t.Fatal(err)
	}
	h.server.usageMu.Lock()
	defer h.server.usageMu.Unlock()
	if held, ok := h.server.usageSums[id]; ok {
		t.Fatalf("a session no report can ask for is kept: %+v", held)
	}
}

// A report whose client gave up reads no other session (Codex review of
// #107, round 1).
func TestAUsageReportItsClientGaveUpOnReadsNoMore(t *testing.T) {
	h := newHarness(t)
	h.usageSession("r1", answerAt(1, h.clock.Now(), &model, proto.Usage{Output: 1, Total: 1}))
	h.usageSession("r2", answerAt(1, h.clock.Now(), &model, proto.Usage{Output: 2, Total: 2}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The client gives up while the first session is read.
	h.engine.onUsageRead = func(int64) { cancel() }
	b := boundsOf(h.clock.Now(), time.UTC)
	if _, err := h.server.sumUsage(ctx, dayStart(b.today, time.UTC), b); !errors.Is(err, context.Canceled) {
		t.Fatalf("sumUsage = %v", err)
	}
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if len(h.engine.usageReads) != 1 {
		t.Fatalf("a report given up on read on: %+v", h.engine.usageReads)
	}
}

// A day whose midnight the zone's clocks skip starts as they move on:
// America/Santiago's 2026-09-06 starts at 01:00, and today is that day,
// not the one before (Codex review of #107, round 1).
func TestADayWhoseMidnightIsSkippedStartsAsItsClocksMoveOn(t *testing.T) {
	h := newHarness(t)
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	h.server.SetZone(santiago)
	h.clock.mu.Lock()
	h.clock.now = time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	h.clock.mu.Unlock()
	h.usageSession("r1",
		answerAt(1, time.Date(2026, 9, 6, 3, 30, 0, 0, time.UTC), &model, proto.Usage{Output: 1, Total: 1}), // 23:30 on the 5th
		answerAt(2, time.Date(2026, 9, 6, 4, 30, 0, 0, time.UTC), &model, proto.Usage{Output: 2, Total: 2}), // 01:30 on the 6th
	)
	r, err := h.client.Usage(context.Background(), "2026-09-06")
	if err != nil {
		t.Fatal(err)
	}
	if r.Today != "2026-09-06" || !r.TodayEnds.Equal(time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)) || len(r.Days) != 1 ||
		r.Days[0].Day != "2026-09-06" || r.Days[0].Models[0].Usage.Total != 2 {
		t.Fatalf("report %+v", r)
	}
	if got := dayStart(time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC), santiago); !got.Equal(time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("the day starts at %s", got)
	}
}

// An answer that names no model counts under its session's model as it is
// when the report is made, whether or not its sum was kept from before
// (Codex review of #107, round 1).
func TestAnAnswerWithNoModelCountsUnderItsSessionsModelNow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.usageSession("r1", answerAt(1, h.clock.Now(), nil, proto.Usage{Output: 1, Total: 1}))
	if r, err := h.client.Usage(ctx, "2026-11-18"); err != nil || r.Days[0].Models[0].Model != model {
		t.Fatalf("report %+v, %v", r, err)
	}
	other := proto.ModelRef{Provider: "faux", ID: "faux-2"}
	if err := h.store.SetSessionModel(ctx, id, other.Provider, other.ID, "", "owner"); err != nil {
		t.Fatal(err)
	}
	if r, err := h.client.Usage(ctx, "2026-11-18"); err != nil || len(r.Days[0].Models) != 1 || r.Days[0].Models[0].Model != other {
		t.Fatalf("report after the model changed %+v, %v", r, err)
	}
}
