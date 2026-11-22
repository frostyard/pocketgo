package state

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// Usage at a glance (ADR-0052): how full the open session's context is,
// what each session and thread used, and today's and the last 7 days'
// totals by provider. Every rule of it is here, with its test; the views
// only draw it.

// ContextTokens is the context an answer's usage measured: its total, as
// the provider reported it, or the sum of its input, its output and the
// cache it read and wrote when that is more (proto.Usage.Tokens). It is
// the measure PiG compacts a session on (ai.CalculateContextTokens): the
// answer's request and the answer itself, which the next request carries.
func ContextTokens(u proto.Usage) int64 { return u.Tokens() }

// MeterLevel is how near a session's context is to where it is compacted.
type MeterLevel int

const (
	// MeterCalm: below NearPercent of the limit, or no limit known.
	MeterCalm MeterLevel = iota
	// MeterNear: NearPercent of the limit or more.
	MeterNear
	// MeterFull: at the limit or past it, or past the window.
	MeterFull
)

// NearPercent is the share of its limit, in percent, at which a context
// is marked near: the standing session's compactAt where it has one below
// the window, else the model's context window.
const NearPercent = 80

// Why a context's fill is not known.
const (
	FillUnmeasured = "No answer has measured the context yet."
	FillCompacted  = "The context was compacted: the next answer measures it."
	FillReset      = "The session started over: the next answer measures it."
)

// ContextFill is how full a session's context is.
type ContextFill struct {
	// Known says an answer measured the context since it last changed
	// whole; Why says why not otherwise.
	Known bool
	Why   string
	// Tokens is the context the newest answer measured (ContextTokens),
	// and Measured that answer's usage.
	Tokens   int64
	Measured proto.Usage
	// Window is the session's model's context window, 0 when unknown;
	// CompactAt the standing session's threshold, 0 for none of its own
	// below a known window.
	Window    int
	CompactAt int
	// Percent is Tokens of Window, rounded down, -1 when the window is
	// unknown; Level how near the context is to its limit.
	Percent int
	Level   MeterLevel
}

// Fill is how full a session's context is, from its transcript, by id,
// and its live state: the newest answer whose usage measured a context,
// the reply being written included, against the model's window and the
// standing session's compactAt. A compaction or a reset after that answer
// changed the context whole, so it is not known until the next answer.
//
// The compactAt counts only below a known window, as pocketd compacts at
// it only then (team.ShouldCompact): with the model's window unknown, the
// meter claims no threshold (Codex review of #107, round 2).
func Fill(entries []proto.Entry, live proto.Live, window, compactAt int) ContextFill {
	f := ContextFill{Window: max(0, window), Percent: -1, Why: FillUnmeasured}
	if compactAt > 0 && compactAt < f.Window {
		f.CompactAt = compactAt
	}
	var measured *proto.Usage
	if p := live.Partial; p != nil && p.Usage != nil && ContextTokens(*p.Usage) > 0 {
		measured = p.Usage
	}
	for i := len(entries) - 1; i >= 0 && measured == nil; i-- {
		e := entries[i]
		switch {
		case e.Kind == proto.EntryCompaction:
			f.Why = FillCompacted
			return f
		case e.Kind == proto.EntryReset:
			f.Why = FillReset
			return f
		case e.Kind == proto.EntryAssistant && e.Usage != nil && ContextTokens(*e.Usage) > 0:
			measured = e.Usage
		}
	}
	if measured == nil {
		return f
	}
	f.Known, f.Why, f.Measured, f.Tokens = true, "", *measured, ContextTokens(*measured)
	if f.Window > 0 {
		f.Percent = int(f.Tokens * 100 / int64(f.Window))
	}
	limit := int64(f.CompactAt)
	if limit == 0 {
		limit = int64(f.Window)
	}
	switch {
	case limit == 0:
	case f.Tokens >= limit || (f.Window > 0 && f.Tokens >= int64(f.Window)):
		f.Level = MeterFull
	case f.Tokens*100 >= limit*NearPercent:
		f.Level = MeterNear
	}
	return f
}

// Thresholds are the tokens at which the meter turns near and full, as
// Level decides them; both 0 when no limit is known.
func (f ContextFill) Thresholds() (near, full float64) {
	limit := float64(f.CompactAt)
	if limit == 0 {
		limit = float64(f.Window)
	}
	return limit * NearPercent / 100, limit
}

// Label is the meter's text: the percent of the window, else the tokens
// when the window is unknown; when the fill is not known, "compacted" or
// "started over" for a context changed whole, and "" before any answer
// measured it.
func (f ContextFill) Label() string {
	switch {
	case f.Known && f.Percent >= 0:
		return strconv.Itoa(f.Percent) + "%"
	case f.Known:
		return FormatTokens(f.Tokens)
	case f.Why == FillCompacted:
		return "compacted"
	case f.Why == FillReset:
		return "started over"
	}
	return ""
}

// Tooltip says the fill in exact numbers: the tokens of the window, the
// answer's counts, and where pocketd compacts the session.
func (f ContextFill) Tooltip() string {
	if !f.Known {
		return f.Why
	}
	u := f.Measured
	var b strings.Builder
	b.WriteString("Context: " + GroupDigits(f.Tokens) + " tokens")
	if f.Window > 0 {
		b.WriteString(" of " + GroupDigits(int64(f.Window)) + " (" + strconv.Itoa(f.Percent) + "%)")
	}
	fmt.Fprintf(&b, ", as the last answer counted it: input %s, cache read %s, cache write %s, output %s.",
		GroupDigits(u.Input), GroupDigits(u.CacheRead), GroupDigits(u.CacheWrite), GroupDigits(u.Output))
	switch {
	case f.CompactAt > 0:
		b.WriteString(" pocketd compacts this session at " + GroupDigits(int64(f.CompactAt)) + " tokens.")
	case f.Window == 0:
		b.WriteString(" The model's context window is unknown.")
	}
	return b.String()
}

// FillOf is how full the open session id's context is (Fill), against
// its model's window as the models list says it, and its profile's
// compactAt when it is that profile's standing session. A session not
// open has no transcript here, so its fill is not known.
func (d *Data) FillOf(id int64) ContextFill {
	o := d.open
	if o == nil || o.ID != id {
		return ContextFill{Percent: -1, Why: FillUnmeasured}
	}
	s, _ := d.Session(id)
	window := 0
	for _, m := range d.models {
		if m.ModelRef == s.Model {
			window = m.ContextWindow
		}
	}
	compactAt := 0
	if s.Standing && s.Profile != "" {
		for _, p := range d.profiles {
			if p.ID == s.Profile {
				compactAt = p.CompactAt
			}
		}
	}
	return Fill(o.Entries, o.Live, window, compactAt)
}

// Tokens is the tokens a usage counts: its total, else the sum of its
// parts (proto.Usage.Tokens).
func Tokens(u proto.Usage) int64 { return u.Tokens() }

// ShowsCost says whether a usage's cost shows: only when it is more than
// zero. A model on a subscription costs nothing per token, so its usage
// shows tokens alone.
func ShowsCost(u proto.Usage) bool { return u.Cost > 0 }

// FormatTokens writes a count of tokens short: as it is below 1,000, then
// in thousands (k), millions (M) and billions (B), with one decimal below
// 10 of the unit ("1.2k", "3.4M") and none from 10 on ("12k", "345k"),
// rounded half up; a count that rounds to 1,000 of a unit is written in
// the next ("1.0M", not "1000k").
func FormatTokens(n int64) string {
	n = max(0, n)
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	units := []struct {
		size   int64
		suffix string
	}{{1_000, "k"}, {1_000_000, "M"}, {1_000_000_000, "B"}}
	for i, u := range units {
		if tenths := (n/u.size)*10 + (n%u.size*10+u.size/2)/u.size; tenths < 100 {
			return strconv.FormatInt(tenths/10, 10) + "." + strconv.FormatInt(tenths%10, 10) + u.suffix
		}
		if whole := n/u.size + (n%u.size+u.size/2)/u.size; whole < 1000 || i == len(units)-1 {
			return strconv.FormatInt(whole, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// FormatCost writes a cost in dollars: in cents below $100 ("$0.42",
// "$12.30"), "<$0.01" for a cost above zero that rounds to no cent, whole
// dollars from $100 on ("$123"), and in thousands from $10,000 on
// ("$12k").
func FormatCost(c float64) string {
	if math.IsNaN(c) || c <= 0 {
		return "$0.00"
	}
	cents := math.Round(c * 100)
	switch {
	case cents < 1:
		return "<$0.01"
	case cents < 10_000:
		return fmt.Sprintf("$%d.%02d", int64(cents)/100, int64(cents)%100)
	}
	if dollars := math.Round(c); dollars < 10_000 {
		return fmt.Sprintf("$%d", int64(dollars))
	}
	return "$" + FormatTokens(int64(math.Round(c)))
}

// GroupDigits writes n with its thousands apart: "123,456".
func GroupDigits(n int64) string {
	s := strconv.FormatInt(max(0, n), 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// UsageLine says what a usage used: its tokens, and its cost when it shows
// ("1.2M tokens · $0.42"); "" when it used nothing.
func UsageLine(u proto.Usage) string {
	tokens := Tokens(u)
	if tokens == 0 && !ShowsCost(u) {
		return ""
	}
	line := FormatTokens(tokens) + " tokens"
	if ShowsCost(u) {
		line += " · " + FormatCost(u.Cost)
	}
	return line
}

// UsageShort says a usage in one word: its cost when it shows, else its
// tokens ("$0.42", "12k tokens"); "" when it used nothing.
func UsageShort(u proto.Usage) string {
	switch {
	case ShowsCost(u):
		return FormatCost(u.Cost)
	case Tokens(u) > 0:
		return FormatTokens(Tokens(u)) + " tokens"
	}
	return ""
}

// ThreadUsage is what a thread's work used: its specialist's session and
// the sessions that session started, its worker's (protocol rule 23a), as
// the sessions list has them. A thread with no session yet used nothing.
func (d *Data) ThreadUsage(th proto.Thread) proto.Usage {
	var u proto.Usage
	if th.Session == 0 {
		return u
	}
	for _, s := range d.sessions {
		if s.ID == th.Session || s.StartedBy == th.Session {
			u = u.Add(s.Usage)
		}
	}
	return u
}

// ProviderUsage is one provider's usage.
type ProviderUsage struct {
	Provider string
	Usage    proto.Usage
}

// UsageTotal is a span's usage, in all and by provider, by the provider's
// name.
type UsageTotal struct {
	Usage     proto.Usage
	Providers []ProviderUsage
}

// WeekDays is how many days the week's total sums: today and the 6 before
// it, in the service's zone.
const WeekDays = 7

// Totals are today's usage and the week's, today and the WeekDays-1 days
// before it, from a report, each by provider, as of now: today is the
// report's, the day in the service's zone when it answered, until the
// report's day ends. A report whose day ended is still the newest only when
// no session used anything since, since each change of a session's usage
// fetches it again: today has no usage then, and the week moves on by the
// days that ended, 24 hours each.
func Totals(r proto.UsageReport, now time.Time) (today, week UsageTotal) {
	end, err := time.Parse(proto.UsageDayLayout, r.Today)
	if err != nil {
		return today, week
	}
	if !r.TodayEnds.IsZero() && !now.Before(r.TodayEnds) {
		end = end.AddDate(0, 0, 1+int(now.Sub(r.TodayEnds)/(24*time.Hour)))
	}
	last := end.Format(proto.UsageDayLayout)
	first := end.AddDate(0, 0, -(WeekDays - 1)).Format(proto.UsageDayLayout)
	todays, weeks := map[string]proto.Usage{}, map[string]proto.Usage{}
	for _, day := range r.Days {
		if day.Day < first || day.Day > last {
			continue
		}
		for _, m := range day.Models {
			weeks[m.Model.Provider] = weeks[m.Model.Provider].Add(m.Usage)
			if day.Day == last {
				todays[m.Model.Provider] = todays[m.Model.Provider].Add(m.Usage)
			}
		}
	}
	return totalOf(todays), totalOf(weeks)
}

func totalOf(by map[string]proto.Usage) UsageTotal {
	var t UsageTotal
	for provider, u := range by {
		t.Usage = t.Usage.Add(u)
		t.Providers = append(t.Providers, ProviderUsage{Provider: provider, Usage: u})
	}
	slices.SortFunc(t.Providers, func(a, b ProviderUsage) int { return strings.Compare(a.Provider, b.Provider) })
	return t
}

// Used says a total used something: tokens, or a cost.
func (t UsageTotal) Used() bool { return UsageLine(t.Usage) != "" }

// Amount says a total: "1.2M tokens · $3.40", or "0 tokens" when it used
// nothing.
func (t UsageTotal) Amount() string {
	if line := UsageLine(t.Usage); line != "" {
		return line
	}
	return "0 tokens"
}

// Line says a total after its label: "Today 1.2M tokens · $3.40".
func (t UsageTotal) Line(label string) string { return label + " " + t.Amount() }

// Tooltip says a total by provider, under what it sums and in which zone
// its days are: "Today in America/New_York, by provider:" and a line each.
func (t UsageTotal) Tooltip(what, zone string) string {
	head := what
	if zone != "" {
		head += " in " + zone
	}
	if by := t.ByProvider(); by != "" {
		return head + ", by provider:\n" + by
	}
	return head + ": nothing used."
}

// ByProvider says a total's providers' usage, one per line in the order
// Totals gives them ("deepseek: 1.2M tokens · $0.42"); "" for none.
func (t UsageTotal) ByProvider() string {
	lines := make([]string, 0, len(t.Providers))
	for _, p := range t.Providers {
		if line := UsageLine(p.Usage); line != "" {
			lines = append(lines, p.Provider+": "+line)
		}
	}
	return strings.Join(lines, "\n")
}

// UsageSince is the day the client asks for the totals from: 8 days
// before now, in the client's own zone, so the report covers the week the
// service's zone ends today, whichever day it is there.
func UsageSince(now time.Time) string {
	return now.AddDate(0, 0, -(WeekDays + 1)).Format(proto.UsageDayLayout)
}

// BeginUsage starts a fetch of the usage report: it comes whole, and the
// answer of a fetch begun later wins.
func (d *Data) BeginUsage() Fetch { return d.Begin() }

// SetUsage records fetch f's report, unless a fetch begun later already
// answered.
func (d *Data) SetUsage(f Fetch, r proto.UsageReport) {
	if !d.usageList.answer(f) {
		return
	}
	d.rev++
	d.usageList.status, d.usageList.err = ListLoaded, ""
	r.Days = slices.Clone(r.Days)
	d.usage = r
}

// SetUsageError records that the report's fetch f failed, unless a fetch
// begun later already answered; the report an earlier one brought stays.
func (d *Data) SetUsageError(f Fetch, err string) {
	if d.usageList.answer(f) {
		d.rev++
		d.usageList.status, d.usageList.err = ListFailed, err
	}
}

// UsageStatus is where the usage report stands, and why its last fetch
// failed.
func (d *Data) UsageStatus() (ListStatus, string) { return d.usageList.status, d.usageList.err }

// UsageReport is the newest report, zero before one came.
func (d *Data) UsageReport() proto.UsageReport { return d.usage }

// UsageTotals are today's and the week's totals of the newest report as
// of now (Totals), and whether to show them: a report came, and the week
// used something.
func (d *Data) UsageTotals(now time.Time) (today, week UsageTotal, shown bool) {
	if d.usage.Today == "" {
		return today, week, false
	}
	today, week = Totals(d.usage, now)
	return today, week, week.Used()
}

// oweUsage notes an answer's entry that used something: its session's
// event, which follows it (protocol rule 16b), fetches the totals again,
// once for the entry and the summary it changed.
func (d *Data) oweUsage(e proto.Entry) {
	if e.Kind != proto.EntryAssistant || e.Usage == nil || UsageLine(*e.Usage) == "" {
		return
	}
	if d.usageOwed == nil {
		d.usageOwed = map[int64]bool{}
	}
	d.usageOwed[e.SessionID] = true
}

// usageChanged says a session event changes the totals: an answer came to
// the session since its last event, or what it used differs from the
// summary the client has. The answer counts whatever the summary says, as
// a list fetched meanwhile may hold the new summary already (Codex review
// of #107, round 1).
func (d *Data) usageChanged(s proto.Session) bool {
	owed := d.usageOwed[s.ID]
	delete(d.usageOwed, s.ID)
	held, ok := d.Session(s.ID)
	if !ok {
		return owed || s.Usage != (proto.Usage{})
	}
	return owed || held.Usage != s.Usage
}

// takeUsageMoved says, once, that a session's model changed since it was
// last asked, whatever brought the change: an event, a list, a detail or
// an action's answer. The totals count an answer that names no model under
// its session's model now (protocol rule 64), so they are fetched again
// (Codex review of #107, round 2).
func (d *Data) takeUsageMoved() bool {
	moved := d.usageMoved
	d.usageMoved = false
	return moved
}

// refetchUsage follows a session event that changes the totals: one fetch
// at a time, and one more after it for every change that came while it
// ran. A fetch's answer that a change came after is older than it: it is
// not shown, and the fetch that follows shows the totals (Codex review of
// #107, round 1).
func (s *Store) refetchUsage() {
	s.usageEvents++
	if s.fetching["usage"] > 0 {
		s.usageDirty = true
		return
	}
	s.loadUsage()
}

// loadUsage fetches the usage report from UsageSince on.
func (s *Store) loadUsage() {
	f := s.Data.BeginUsage()
	since := UsageSince(s.now())
	events := s.usageEvents
	var report proto.UsageReport
	s.do("Loading the usage", func(ctx context.Context, c *proto.Client) (err error) {
		report, err = c.Usage(ctx, since)
		return err
	}, s.track("usage", func(err error) {
		if events == s.usageEvents {
			if err != nil {
				s.Data.SetUsageError(f, err.Error())
			} else {
				s.Data.SetUsage(f, report)
			}
		}
		if s.usageDirty && s.fetching["usage"] == 0 {
			s.usageDirty = false
			s.loadUsage()
		}
	}))
}

// usageStale says the report's day ended by the client's clock: a stream
// that comes back asks for the totals again, since it may have missed the
// changes that would have.
func (s *Store) usageStale() bool {
	r := s.Data.UsageReport()
	return !r.TodayEnds.IsZero() && !s.now().Before(r.TodayEnds)
}

// UsageTotals are today's and the week's totals of the newest report, as
// of the store's clock (Data.UsageTotals).
func (s *Store) UsageTotals() (today, week UsageTotal, shown bool) {
	return s.Data.UsageTotals(s.now())
}
