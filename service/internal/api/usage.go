package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/store"
)

// Usage at a glance (ADR-0052): GET /v1/usage sums the usage of the
// sessions' answers, their assistant entries, by day in the service's time
// zone and by model (protocol rule 63). It reads PiG's transcripts alone,
// keeps nothing in the work database, and keeps in memory the sums of what
// it read, so each answer is read once while pocketd runs.

// UsageEntry is the usage of one answer of a session: its entry's id, its
// time, its model, and the tokens and cost it used.
type UsageEntry struct {
	ID    int64
	At    time.Time
	Model proto.ModelRef
	Usage proto.Usage
}

// sessionUsage is what GET /v1/usage keeps of one session: the sums of its
// answers through entry through, by day and model, of the days from from,
// the start of a day in the service's zone, on. An answer that names no
// model is kept under the zero model, which a report reads as the
// session's model then (rule 63), so no sum depends on when it was read.
type sessionUsage struct {
	from    time.Time
	through int64
	days    usageDays
}

// usageDays are usage sums by day (proto.UsageDayLayout) and model.
type usageDays map[string]map[proto.ModelRef]proto.Usage

// add counts e on its day in zone, under its model; an answer that used
// nothing counts nowhere.
func (d usageDays) add(e UsageEntry, zone *time.Location) {
	if e.Usage == (proto.Usage{}) {
		return
	}
	d.addTo(e.At.In(zone).Format(proto.UsageDayLayout), e.Model, e.Usage)
}

func (d usageDays) addTo(day string, model proto.ModelRef, u proto.Usage) {
	if d[day] == nil {
		d[day] = map[proto.ModelRef]proto.Usage{}
	}
	d[day][model] = d[day][model].Add(u)
}

// prune drops the days before first.
func (d usageDays) prune(first string) {
	for day := range d {
		if day < first {
			delete(d, day)
		}
	}
}

// civilDay is the date t falls on in zone, as midnight in UTC: a date
// with no instant of its own, which AddDate moves by whole days.
func civilDay(t time.Time, zone *time.Location) time.Time {
	y, m, d := t.In(zone).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// dayStart is the first instant of date day (civilDay) in zone. A zone
// whose clocks skip midnight that day, as America/Santiago's on
// 2026-09-06, starts it as the clocks move on: time.Date would name an
// instant of the day before (Codex review of #107, round 1).
func dayStart(day time.Time, zone *time.Location) time.Time {
	y, m, d := day.Date()
	t := time.Date(y, m, d, 0, 0, 0, 0, zone)
	if ty, tm, td := t.Date(); ty != y || tm != m || td != d {
		if _, end := t.ZoneBounds(); !end.IsZero() {
			t = end
		}
	}
	return t
}

// usageBounds are the days a report sums, in zone, as of now: today, its
// first instant's next day's (when today ends), and the oldest day a
// since may name, proto.MaxUsageDays-1 days before today.
type usageBounds struct {
	today, oldest           time.Time // dates (civilDay)
	todayEnds, oldestStarts time.Time // instants
}

func boundsOf(now time.Time, zone *time.Location) usageBounds {
	today := civilDay(now, zone)
	oldest := today.AddDate(0, 0, -(proto.MaxUsageDays - 1))
	return usageBounds{today: today, oldest: oldest, todayEnds: dayStart(today.AddDate(0, 0, 1), zone), oldestStarts: dayStart(oldest, zone)}
}

// errUsageSince is a since GET /v1/usage cannot take.
var errUsageSince = errors.New("since must be a day, YYYY-MM-DD")

// usageSince is day raw as GET /v1/usage takes its since (rule 63): a day
// written as proto.UsageDayLayout writes it, at most proto.MaxUsageDays-1
// days before today; and its first instant in zone. A day after today is
// taken: it has no usage yet.
func usageSince(raw string, b usageBounds, zone *time.Location) (day, start time.Time, err error) {
	day, err = time.Parse(proto.UsageDayLayout, raw)
	if err != nil || day.Format(proto.UsageDayLayout) != raw {
		return time.Time{}, time.Time{}, errUsageSince
	}
	if day.Before(b.oldest) {
		return time.Time{}, time.Time{}, fmt.Errorf("since is at most %d days before today, %s: %s is earlier", proto.MaxUsageDays-1,
			b.oldest.Format(proto.UsageDayLayout), raw)
	}
	return day, dayStart(day, zone), nil
}

// usageReport is the report of days from since (a date) through today,
// from the sums of days, in zone (rule 63): the days with usage, oldest
// first, each with its models' usage by provider, then by model id.
func usageReport(days usageDays, since time.Time, b usageBounds, zone *time.Location) proto.UsageReport {
	r := proto.UsageReport{Zone: zone.String(), Today: b.today.Format(proto.UsageDayLayout), TodayEnds: b.todayEnds.UTC(),
		Since: since.Format(proto.UsageDayLayout), Days: []proto.UsageDay{}}
	for day, models := range days {
		if day < r.Since || day > r.Today {
			continue
		}
		out := proto.UsageDay{Day: day}
		for model, u := range models {
			out.Models = append(out.Models, proto.ModelUsage{Model: model, Usage: u})
		}
		sort.Slice(out.Models, func(i, j int) bool {
			a, b := out.Models[i].Model, out.Models[j].Model
			if a.Provider != b.Provider {
				return a.Provider < b.Provider
			}
			return a.ID < b.ID
		})
		r.Days = append(r.Days, out)
	}
	sort.Slice(r.Days, func(i, j int) bool { return r.Days[i].Day < r.Days[j].Day })
	return r
}

// usage answers GET /v1/usage (rule 63). It reads only, so a request its
// client gave up on stops (Codex review of #107, round 1).
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	b := boundsOf(now, s.zone)
	since, start, err := usageSince(r.URL.Query().Get("since"), b, s.zone)
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	days, err := s.sumUsage(r.Context(), start, b)
	if err != nil {
		if r.Context().Err() == nil {
			s.internal(w, "sum the usage", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, usageReport(days, since, b, s.zone))
}

// sumUsage sums the answers of the sessions since since, an instant, by
// day and model. An answer that names no model counts under its session's
// model, and its cost only when that model's provider is metered
// (Engine.Metered), both as they are now: the sums kept are as PiG keeps
// the answers, so no report depends on when they were read (Codex review
// of #107, round 2). It reads each session's answers once: it keeps their
// sums, and reads on after the newest entry it read; a session it holds
// from a day after since is read again from since.
//
// It reads every session kept or updated since the oldest day a since may
// name, its transcript, not its time, deciding whether it has an answer
// since since: a stop between an answer's commit and the write of its
// session's time leaves the time behind the transcript (Codex review of
// #107, round 2). It stops when ctx ends.
func (s *Server) sumUsage(ctx context.Context, since time.Time, b usageBounds) (usageDays, error) {
	rows, err := s.store.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.usageSums == nil {
		s.usageSums = map[int64]*sessionUsage{}
	}
	s.pruneUsage(rows, b)
	metered := map[string]bool{}
	priced := func(model proto.ModelRef, u proto.Usage) proto.Usage {
		m, ok := metered[model.Provider]
		if !ok {
			m = s.engine.Metered(model.Provider)
			metered[model.Provider] = m
		}
		if !m {
			u.Cost = 0
		}
		return u
	}
	sum := usageDays{}
	for _, row := range rows {
		if _, held := s.usageSums[row.ID]; !held && row.UpdatedAt.Before(b.oldestStarts) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		held, err := s.sessionUsage(ctx, row.ID, since)
		if err != nil {
			return nil, err
		}
		session := proto.ModelRef{Provider: row.Provider, ID: row.Model}
		for day, models := range held.days {
			for model, u := range models {
				if model == (proto.ModelRef{}) {
					model = session
				}
				sum.addTo(day, model, priced(model, u))
			}
		}
	}
	return sum, nil
}

// pruneUsage drops what no report can ask for: every kept day before the
// oldest one a since may name, and the sums kept of a session left with
// none that was not updated since then, whether or not a report reads it
// (Codex review of #107, round 1).
func (s *Server) pruneUsage(rows []store.SessionRow, b usageBounds) {
	updated := make(map[int64]time.Time, len(rows))
	for _, row := range rows {
		updated[row.ID] = row.UpdatedAt
	}
	first := b.oldest.Format(proto.UsageDayLayout)
	for id, held := range s.usageSums {
		held.days.prune(first)
		if held.from.Before(b.oldestStarts) {
			held.from = b.oldestStarts
		}
		if len(held.days) == 0 && updated[id].Before(b.oldestStarts) {
			delete(s.usageSums, id)
		}
	}
}

// sessionUsage brings the sums kept of session id up to its newest entry,
// from since on at least. Sums whose read fails stay as they were.
func (s *Server) sessionUsage(ctx context.Context, id int64, since time.Time) (*sessionUsage, error) {
	held := s.usageSums[id]
	if held == nil || since.Before(held.from) {
		held = &sessionUsage{from: since, days: usageDays{}}
	}
	entries, newest, err := s.engine.UsageEntries(ctx, id, held.through, held.from)
	if err != nil {
		return nil, fmt.Errorf("usage of session %d: %w", id, err)
	}
	for _, e := range entries {
		held.days.add(e, s.zone)
	}
	held.through = max(held.through, newest)
	s.usageSums[id] = held
	return held, nil
}
