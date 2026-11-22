package state

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// The context an answer measured is its total as the provider reported
// it, else the sum of its parts, as PiG compacts on it: the cache written
// counts, so a provider's first request, which writes the whole prompt to
// its cache, does not read near empty (ADR-0052).
func TestContextTokensIsWhatPiGCompactsOn(t *testing.T) {
	for _, c := range []struct {
		u    proto.Usage
		want int64
	}{
		{proto.Usage{}, 0},
		{proto.Usage{Input: 3, Output: 400, CacheRead: 0, CacheWrite: 90_000}, 90_403},
		{proto.Usage{Input: 3, Output: 400, CacheRead: 90_000, CacheWrite: 500}, 90_903},
		{proto.Usage{Input: 3, Output: 400, CacheRead: 90_000, Total: 95_000}, 95_000},
		{proto.Usage{Input: -5, Output: 2}, 2},
	} {
		if got := ContextTokens(c.u); got != c.want {
			t.Errorf("ContextTokens(%+v) = %d, want %d", c.u, got, c.want)
		}
	}
}

func usageOf(tokens int64) *proto.Usage { return &proto.Usage{Input: tokens} }

// The fill is the newest answer that measured a context, the reply being
// written first, against the window; a compaction or a reset after it
// leaves the fill unknown until the next answer.
func TestFillIsTheNewestMeasuredAnswer(t *testing.T) {
	answer := func(id int64, u *proto.Usage) proto.Entry {
		return proto.Entry{ID: id, Kind: proto.EntryAssistant, Usage: u}
	}
	user := proto.Entry{ID: 9, Kind: proto.EntryUser, Text: "go on"}
	tool := proto.Entry{ID: 10, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "c"}}
	for _, c := range []struct {
		name    string
		entries []proto.Entry
		live    proto.Live
		known   bool
		tokens  int64
		why     string
	}{
		{"none", nil, proto.Live{}, false, 0, FillUnmeasured},
		{"no usage", []proto.Entry{answer(1, nil), user}, proto.Live{}, false, 0, FillUnmeasured},
		{"the newest answer", []proto.Entry{answer(1, usageOf(10)), answer(2, usageOf(20)), user, tool}, proto.Live{}, true, 20, ""},
		{"an empty usage is skipped", []proto.Entry{answer(1, usageOf(10)), answer(2, &proto.Usage{})}, proto.Live{}, true, 10, ""},
		{"the reply being written", []proto.Entry{answer(1, usageOf(10))}, proto.Live{Partial: &proto.Entry{Kind: proto.EntryAssistant, Usage: usageOf(30)}}, true, 30, ""},
		{"a reply with no usage yet", []proto.Entry{answer(1, usageOf(10))}, proto.Live{Partial: &proto.Entry{Kind: proto.EntryAssistant}}, true, 10, ""},
		{"compacted", []proto.Entry{answer(1, usageOf(10)), {ID: 2, Kind: proto.EntryCompaction, Text: "summary"}}, proto.Live{}, false, 0, FillCompacted},
		{"started over", []proto.Entry{answer(1, usageOf(10)), {ID: 2, Kind: proto.EntryReset}}, proto.Live{}, false, 0, FillReset},
		{"measured after a compaction", []proto.Entry{answer(1, usageOf(10)), {ID: 2, Kind: proto.EntryCompaction}, answer(3, usageOf(5))}, proto.Live{}, true, 5, ""},
		{"a reply after a reset", []proto.Entry{{ID: 2, Kind: proto.EntryReset}}, proto.Live{Partial: &proto.Entry{Kind: proto.EntryAssistant, Usage: usageOf(7)}}, true, 7, ""},
	} {
		f := Fill(c.entries, c.live, 100, 0)
		if f.Known != c.known || f.Tokens != c.tokens || f.Why != c.why {
			t.Errorf("%s: %+v, want known %v tokens %d why %q", c.name, f, c.known, c.tokens, c.why)
		}
	}
}

// The meter is near from 80% of its limit and full at it: a standing
// session's compactAt below the window, else the window; a compactAt at or
// past the window, which pocketd refuses, counts as none, and no limit
// known leaves it calm, with no percent.
func TestTheMetersLevels(t *testing.T) {
	at := func(tokens int64) []proto.Entry {
		return []proto.Entry{{ID: 1, Kind: proto.EntryAssistant, Usage: usageOf(tokens)}}
	}
	for _, c := range []struct {
		name              string
		tokens            int64
		window, compactAt int
		level             MeterLevel
		percent           int
		near, full        float64
	}{
		{"calm", 159_999, 200_000, 0, MeterCalm, 79, 160_000, 200_000},
		{"near the window", 160_000, 200_000, 0, MeterNear, 80, 160_000, 200_000},
		{"at the window", 200_000, 200_000, 0, MeterFull, 100, 160_000, 200_000},
		{"past the window", 230_000, 200_000, 0, MeterFull, 115, 160_000, 200_000},
		{"calm below compactAt", 95_999, 200_000, 120_000, MeterCalm, 47, 96_000, 120_000},
		{"near compactAt", 96_000, 200_000, 120_000, MeterNear, 48, 96_000, 120_000},
		{"at compactAt", 120_000, 200_000, 120_000, MeterFull, 60, 96_000, 120_000},
		{"a compactAt past the window", 170_000, 200_000, 250_000, MeterNear, 85, 160_000, 200_000},
		{"compactAt with no window", 100_000, 0, 120_000, MeterNear, -1, 96_000, 120_000},
		{"no limit", 100_000, 0, 0, MeterCalm, -1, 0, 0},
	} {
		f := Fill(at(c.tokens), proto.Live{}, c.window, c.compactAt)
		near, full := f.Thresholds()
		if f.Level != c.level || f.Percent != c.percent || near != c.near || full != c.full {
			t.Errorf("%s: level %v percent %d thresholds %v/%v, want %v %d %v/%v", c.name, f.Level, f.Percent, near, full, c.level, c.percent, c.near, c.full)
		}
	}
}

// The meter's label is the percent of the window, or the tokens with no
// window; its tooltip gives the exact numbers and where pocketd compacts.
func TestTheMetersLabelAndTooltip(t *testing.T) {
	u := proto.Usage{Input: 1_234, Output: 10_000, CacheRead: 110_000, CacheWrite: 2_222}
	entries := []proto.Entry{{ID: 1, Kind: proto.EntryAssistant, Usage: &u}}
	f := Fill(entries, proto.Live{}, 200_000, 160_000)
	if got := f.Label(); got != "61%" {
		t.Errorf("label %q", got)
	}
	want := "Context: 123,456 tokens of 200,000 (61%), as the last answer counted it: input 1,234, cache read 110,000, cache write 2,222, output 10,000." +
		" pocketd compacts this session at 160,000 tokens."
	if got := f.Tooltip(); got != want {
		t.Errorf("tooltip\n%q\nwant\n%q", got, want)
	}
	f = Fill(entries, proto.Live{}, 0, 0)
	if got := f.Label(); got != "123k" {
		t.Errorf("label with no window %q", got)
	}
	if got := f.Tooltip(); got != "Context: 123,456 tokens, as the last answer counted it: input 1,234, cache read 110,000, cache write 2,222, output 10,000. The model's context window is unknown." {
		t.Errorf("tooltip with no window %q", got)
	}
	f = Fill(nil, proto.Live{}, 200_000, 0)
	if f.Label() != "" || f.Tooltip() != FillUnmeasured {
		t.Errorf("unknown fill: %q %q", f.Label(), f.Tooltip())
	}
	f = Fill(append(entries, proto.Entry{ID: 2, Kind: proto.EntryCompaction}), proto.Live{}, 200_000, 0)
	if f.Label() != "compacted" || f.Tooltip() != FillCompacted {
		t.Errorf("compacted fill: %q %q", f.Label(), f.Tooltip())
	}
	f = Fill(append(entries, proto.Entry{ID: 2, Kind: proto.EntryReset}), proto.Live{}, 200_000, 0)
	if f.Label() != "started over" || f.Tooltip() != FillReset {
		t.Errorf("reset fill: %q %q", f.Label(), f.Tooltip())
	}
}

// The open session's fill takes its model's window from the models list,
// and its profile's compactAt only for that profile's standing session.
func TestFillOfTheOpenSession(t *testing.T) {
	var d Data
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	d.SetModels(d.Begin(), []proto.Model{{ModelRef: opus, ContextWindow: 200_000}, {ModelRef: proto.ModelRef{Provider: "x", ID: "y"}, ContextWindow: 1}})
	d.SetProfiles(d.Begin(), []proto.Profile{{ID: "odrade", Standing: true, CompactAt: 120_000}})
	standing := proto.Session{ID: 1, Model: opus, Profile: "odrade", Standing: true}
	plain := proto.Session{ID: 2, Model: opus, Profile: "odrade"}
	d.SetSessions(d.Begin(), []proto.Session{standing, plain})
	entries := []proto.Entry{{ID: 1, Kind: proto.EntryAssistant, Usage: usageOf(100_000)}}
	if f := d.FillOf(1); f.Known {
		t.Errorf("a session not open has a fill: %+v", f)
	}
	d.SetOpen(1)
	d.SetDetail(d.Begin(), proto.SessionDetail{Session: standing, Entries: entries})
	if f := d.FillOf(1); !f.Known || f.Window != 200_000 || f.CompactAt != 120_000 || f.Level != MeterNear || f.Percent != 50 {
		t.Errorf("standing %+v", f)
	}
	d.SetOpen(2)
	d.SetDetail(d.Begin(), proto.SessionDetail{Session: plain, Entries: entries})
	if f := d.FillOf(2); !f.Known || f.CompactAt != 0 || f.Level != MeterCalm {
		t.Errorf("plain %+v", f)
	}
}

func TestFormatTokens(t *testing.T) {
	for n, want := range map[int64]string{
		-3: "0", 0: "0", 999: "999", 1_000: "1.0k", 1_049: "1.0k", 1_050: "1.1k", 1_234: "1.2k", 9_949: "9.9k", 9_950: "10k",
		12_345: "12k", 345_678: "346k", 999_499: "999k", 999_500: "1.0M", 3_400_000: "3.4M", 9_950_000: "10M", 123_456_789: "123M",
		999_500_000: "1.0B", 12_300_000_000: "12B", 1_234_567_000_000: "1235B",
	} {
		if got := FormatTokens(n); got != want {
			t.Errorf("FormatTokens(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestFormatCost(t *testing.T) {
	for c, want := range map[float64]string{
		0: "$0.00", -1: "$0.00", 0.004: "<$0.01", 0.005: "$0.01", 0.42: "$0.42", 1.5: "$1.50", 12.345: "$12.35", 99.994: "$99.99",
		99.996: "$100", 123.4: "$123", 9_999.4: "$9999", 12_345: "$12k",
	} {
		if got := FormatCost(c); got != want {
			t.Errorf("FormatCost(%v) = %q, want %q", c, got, want)
		}
	}
	if got := GroupDigits(1_234_567); got != "1,234,567" {
		t.Errorf("GroupDigits = %q", got)
	}
}

// Cost shows only when it is more than zero: a model on a subscription
// shows tokens alone.
func TestACostOfZeroIsHidden(t *testing.T) {
	for _, c := range []struct {
		u           proto.Usage
		line, short string
	}{
		{proto.Usage{}, "", ""},
		{proto.Usage{Input: 1_000, Output: 200, Total: 1_200}, "1.2k tokens", "1.2k tokens"},
		{proto.Usage{Input: 1_000, Output: 200, Total: 1_200, Cost: 0.42}, "1.2k tokens · $0.42", "$0.42"},
		{proto.Usage{Total: 5, Cost: 0.0001}, "5 tokens · <$0.01", "<$0.01"},
	} {
		if !ShowsCost(c.u) != (c.u.Cost <= 0) {
			t.Errorf("ShowsCost(%+v) = %v", c.u, ShowsCost(c.u))
		}
		if got := UsageLine(c.u); got != c.line {
			t.Errorf("UsageLine(%+v) = %q, want %q", c.u, got, c.line)
		}
		if got := UsageShort(c.u); got != c.short {
			t.Errorf("UsageShort(%+v) = %q, want %q", c.u, got, c.short)
		}
	}
}

// A thread's usage is its specialist's session's and its worker's, the
// session that one started.
func TestAThreadsUsageIsItsSessionsAndItsWorkers(t *testing.T) {
	var d Data
	d.SetSessions(d.Begin(), []proto.Session{
		{ID: 1, Usage: proto.Usage{Total: 100}},
		{ID: 2, StartedBy: 1, Route: "worker", Usage: proto.Usage{Total: 50, Cost: 0.2}},
		{ID: 3, Usage: proto.Usage{Total: 7}},
	})
	if got := d.ThreadUsage(proto.Thread{ID: 1, Session: 1}); got != (proto.Usage{Total: 150, Cost: 0.2}) {
		t.Errorf("thread usage %+v", got)
	}
	if got := d.ThreadUsage(proto.Thread{ID: 2}); got != (proto.Usage{}) {
		t.Errorf("a thread with no session %+v", got)
	}
}

func usageDay(day string, models ...proto.ModelUsage) proto.UsageDay {
	return proto.UsageDay{Day: day, Models: models}
}

func modelUsage(provider string, total int64, cost float64) proto.ModelUsage {
	return proto.ModelUsage{Model: proto.ModelRef{Provider: provider, ID: provider + "-1"}, Usage: proto.Usage{Total: total, Cost: cost}}
}

// Today's total is the report's today, and the week's the 7 days through
// it, both by provider; once the report's day ended, today has no usage
// and the week moves on by the days that ended.
func TestTotalsAreTodayAndTheWeekByProvider(t *testing.T) {
	ends := time.Date(2026, 11, 22, 5, 0, 0, 0, time.UTC)
	r := proto.UsageReport{Zone: "America/New_York", Today: "2026-11-21", TodayEnds: ends, Since: "2026-11-13", Days: []proto.UsageDay{
		usageDay("2026-11-14", modelUsage("anthropic", 1_000, 0)),
		usageDay("2026-11-15", modelUsage("anthropic", 100, 0), modelUsage("deepseek", 10, 0.1)),
		usageDay("2026-11-21", modelUsage("deepseek", 20, 0.2), modelUsage("anthropic", 200, 0),
			proto.ModelUsage{Model: proto.ModelRef{Provider: "anthropic", ID: "claude-haiku"}, Usage: proto.Usage{Total: 5}}),
	}}
	today, week := Totals(r, ends.Add(-time.Hour))
	wantToday := UsageTotal{Usage: proto.Usage{Total: 225, Cost: 0.2}, Providers: []ProviderUsage{
		{"anthropic", proto.Usage{Total: 205}}, {"deepseek", proto.Usage{Total: 20, Cost: 0.2}}}}
	if !reflect.DeepEqual(today, wantToday) {
		t.Errorf("today %+v, want %+v", today, wantToday)
	}
	if week.Usage.Total != 335 || len(week.Providers) != 2 || week.Providers[1].Usage.Total != 30 {
		t.Errorf("week %+v", week)
	}
	if got := today.ByProvider(); got != "anthropic: 205 tokens\ndeepseek: 20 tokens · $0.20" {
		t.Errorf("by provider %q", got)
	}
	if got := today.Line("Today"); got != "Today 225 tokens · $0.20" {
		t.Errorf("line %q", got)
	}
	if got := today.Tooltip("Today", r.Zone); got != "Today in America/New_York, by provider:\nanthropic: 205 tokens\ndeepseek: 20 tokens · $0.20" {
		t.Errorf("tooltip %q", got)
	}
	if got := (UsageTotal{}).Line("Today"); got != "Today 0 tokens" {
		t.Errorf("an empty total's line %q", got)
	}
	if got := (UsageTotal{}).Tooltip("Today", ""); got != "Today: nothing used." {
		t.Errorf("an empty total's tooltip %q", got)
	}
	// The day ended with no usage since: today is the 22nd, empty, and the
	// week starts on the 16th, without the 15th.
	today, week = Totals(r, ends)
	if today.Used() || week.Usage.Total != 225 {
		t.Errorf("after the day's end: today %+v, week %+v", today, week)
	}
	// Two days on, the 21st is still in the week.
	today, week = Totals(r, ends.Add(25*time.Hour))
	if today.Used() || week.Usage.Total != 225 {
		t.Errorf("two days on: today %+v, week %+v", today, week)
	}
	if today, week := Totals(proto.UsageReport{}, ends); today.Used() || week.Used() {
		t.Errorf("no report: %+v %+v", today, week)
	}
}

// The totals show once a report came whose week used something.
func TestTheTotalsShowOnceTheWeekUsedSomething(t *testing.T) {
	var d Data
	if _, _, shown := d.UsageTotals(pocketdtest.Now); shown {
		t.Error("totals shown before a report")
	}
	d.SetUsage(d.Begin(), proto.UsageReport{Today: "2026-11-18", Days: []proto.UsageDay{}})
	if _, _, shown := d.UsageTotals(pocketdtest.Now); shown {
		t.Error("totals shown for a week that used nothing")
	}
	// The answer of a fetch begun before one that answered is dropped.
	older := d.BeginUsage()
	newest := d.BeginUsage()
	d.SetUsage(newest, proto.UsageReport{Today: "2026-11-18", Days: []proto.UsageDay{usageDay("2026-11-18", modelUsage("deepseek", 10, 0))}})
	d.SetUsage(older, proto.UsageReport{Today: "2026-11-18"})
	if today, _, shown := d.UsageTotals(pocketdtest.Now); !shown || today.Usage.Total != 10 {
		t.Errorf("totals %+v %v", today, shown)
	}
	d.SetUsageError(d.BeginUsage(), "timeout")
	if st, err := d.UsageStatus(); st != ListFailed || err != "timeout" {
		t.Errorf("status %v %q", st, err)
	}
	if today, _, shown := d.UsageTotals(pocketdtest.Now); !shown || today.Usage.Total != 10 {
		t.Errorf("a failed fetch dropped the totals: %+v %v", today, shown)
	}
}

// The client asks for the totals from 8 days back in its own zone, so the
// week the service's zone ends today is in the report, whichever day it
// is there.
func TestUsageSince(t *testing.T) {
	if got := UsageSince(time.Date(2026, 11, 21, 23, 0, 0, 0, time.UTC)); got != "2026-11-13" {
		t.Errorf("UsageSince = %q", got)
	}
}

// A session event that changes what the session used fetches the totals
// again; one that does not, as a title's change, fetches nothing.
func TestTheTotalsAreFetchedAgainWhenASessionsUsageChanges(t *testing.T) {
	h := newHarness(t)
	s := session(1, "Work", -time.Minute)
	h.srv.AddSession(s)
	h.connect(t)
	if r := h.store.Data.UsageReport(); r.Today != "2026-11-18" || r.Since != "2026-11-10" {
		t.Fatalf("report %+v", r)
	}
	before := len(h.srv.RequestsTo(http.MethodGet, proto.PathUsage))
	s.Title = "Renamed"
	ev := h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &s})
	h.loop.RunUntil(t, func() bool { return h.store.lastSeq == ev.Seq })
	h.settle(t)
	if got := len(h.srv.RequestsTo(http.MethodGet, proto.PathUsage)); got != before {
		t.Fatalf("a session event that changed no usage fetched the totals: %d fetches, want %d", got, before)
	}
	h.srv.SetUsage(proto.UsageReport{Days: []proto.UsageDay{usageDay("2026-11-18", modelUsage("deepseek", 1_200, 0.42))}})
	s.Usage = proto.Usage{Total: 1_200, Cost: 0.42}
	h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &s})
	h.loop.RunUntil(t, func() bool { today, _, shown := h.store.UsageTotals(); return shown && today.Usage.Total == 1_200 })
	h.settle(t)
	if got := len(h.srv.RequestsTo(http.MethodGet, proto.PathUsage)); got != before+1 {
		t.Errorf("%d fetches after a usage change, want %d", got, before+1)
	}
}

// Changes of usage that come while the totals' fetch runs share one more
// fetch after it.
func TestUsageChangesShareOneFetch(t *testing.T) {
	h := newHarness(t)
	s := session(1, "Work", -time.Minute)
	h.srv.AddSession(s)
	h.connect(t)
	before := len(h.srv.RequestsTo(http.MethodGet, proto.PathUsage))
	release := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathUsage)
	s.Usage.Total = 1
	h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &s})
	h.loop.RunUntil(t, func() bool { return h.srv.Holding(http.MethodGet, proto.PathUsage) })
	var last proto.Event
	for i := range 5 {
		s.Usage.Total = int64(i + 2)
		last = h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &s})
	}
	h.loop.RunUntil(t, func() bool { return h.store.lastSeq == last.Seq })
	release()
	h.settle(t)
	if got := len(h.srv.RequestsTo(http.MethodGet, proto.PathUsage)) - before; got != 2 {
		t.Errorf("%d usage fetches for 6 changes during one fetch, want 2", got)
	}
}

// A stream that comes back fetches the totals again when their fetch
// failed, or when the report's day ended by the client's clock, which
// a missed change would have fetched; a report whose day runs is kept.
func TestAReconnectFetchesTotalsThatFailedOrWhoseDayEnded(t *testing.T) {
	h := newHarness(t)
	h.connect(t)
	reconnect := func() int {
		t.Helper()
		before := len(h.srv.RequestsTo(http.MethodGet, proto.PathUsage))
		h.srv.DropStreams()
		h.loop.RunUntil(t, func() bool { return len(h.clock.Waiting()) == 1 })
		h.clock.Fire()
		h.loop.RunUntil(t, func() bool { return h.store.Conn.State == Connected })
		h.settle(t)
		return len(h.srv.RequestsTo(http.MethodGet, proto.PathUsage)) - before
	}
	if got := reconnect(); got != 0 {
		t.Errorf("a reconnect fetched a current report %d times", got)
	}
	h.now = h.store.Data.UsageReport().TodayEnds
	if got := reconnect(); got != 1 {
		t.Errorf("a reconnect after the day's end fetched the report %d times, want 1", got)
	}
	h.store.Data.SetUsageError(h.store.Data.BeginUsage(), "timeout")
	h.now = pocketdtest.Now
	if got := reconnect(); got != 1 {
		t.Errorf("a reconnect after a failed fetch fetched the report %d times, want 1", got)
	}
}
