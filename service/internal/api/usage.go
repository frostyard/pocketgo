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
// the start of a day in the service's zone, on.
type sessionUsage struct {
	from    time.Time
	through int64
	days    usageDays
}

// usageDays are usage sums by day (proto.UsageDayLayout) and model.
type usageDays map[string]map[proto.ModelRef]proto.Usage

// add counts e on its day in zone, under its model, else under the
// session's model when the entry names none; an answer that used nothing
// counts nowhere.
func (d usageDays) add(e UsageEntry, session proto.ModelRef, zone *time.Location) {
	if e.Usage == (proto.Usage{}) {
		return
	}
	model := e.Model
	if model == (proto.ModelRef{}) {
		model = session
	}
	day := e.At.In(zone).Format(proto.UsageDayLayout)
	if d[day] == nil {
		d[day] = map[proto.ModelRef]proto.Usage{}
	}
	d[day][model] = d[day][model].Add(e.Usage)
}

// usageDay is the start of the day t falls on in zone.
func usageDay(t time.Time, zone *time.Location) time.Time {
	y, m, d := t.In(zone).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, zone)
}

// errUsageSince is a since GET /v1/usage cannot take.
var errUsageSince = errors.New("since must be a day, YYYY-MM-DD")

// usageSince is the start of day raw in zone, as GET /v1/usage takes its
// since (rule 63): a day written as proto.UsageDayLayout writes it, at most
// proto.MaxUsageDays-1 days before today, the day now falls on in zone. A
// day after today is taken: it has no usage yet.
func usageSince(raw string, now time.Time, zone *time.Location) (time.Time, error) {
	day, err := time.Parse(proto.UsageDayLayout, raw)
	if err != nil || day.Format(proto.UsageDayLayout) != raw {
		return time.Time{}, errUsageSince
	}
	since := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, zone)
	if oldest := usageDay(now, zone).AddDate(0, 0, -(proto.MaxUsageDays - 1)); since.Before(oldest) {
		return time.Time{}, fmt.Errorf("since is at most %d days before today, %s: %s is earlier", proto.MaxUsageDays-1,
			oldest.Format(proto.UsageDayLayout), raw)
	}
	return since, nil
}

// usageReport is the report of days from since through today, the day now
// falls on in zone, from the sums of days and the zone (rule 63): the days
// with usage, oldest first, each with its models' usage by provider, then
// by model id.
func usageReport(days usageDays, since, now time.Time, zone *time.Location) proto.UsageReport {
	today := usageDay(now, zone)
	r := proto.UsageReport{Zone: zone.String(), Today: today.Format(proto.UsageDayLayout),
		TodayEnds: time.Date(today.Year(), today.Month(), today.Day()+1, 0, 0, 0, 0, zone).UTC(),
		Since:     since.Format(proto.UsageDayLayout), Days: []proto.UsageDay{}}
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

// usage answers GET /v1/usage (rule 63).
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	since, err := usageSince(r.URL.Query().Get("since"), now, s.zone)
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	days, err := s.sumUsage(context.WithoutCancel(r.Context()), since, now)
	if err != nil {
		s.internal(w, "sum the usage", err)
		return
	}
	writeJSON(w, http.StatusOK, usageReport(days, since, now, s.zone))
}

// sumUsage sums the answers of every session updated since since, by day
// and model. It reads each session's answers once: it keeps their sums,
// and reads on after the newest entry it read; a session it holds from a
// day after since is read again from since. A session not updated since
// since has no answer since then, as its time is its newest entry's (rule
// 16b), and is not read.
func (s *Server) sumUsage(ctx context.Context, since, now time.Time) (usageDays, error) {
	rows, err := s.store.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.usageSums == nil {
		s.usageSums = map[int64]*sessionUsage{}
	}
	oldest := usageDay(now, s.zone).AddDate(0, 0, -(proto.MaxUsageDays - 1))
	sum := usageDays{}
	for _, row := range rows {
		if row.UpdatedAt.Before(since) {
			continue
		}
		held, err := s.sessionUsage(ctx, row, since, oldest)
		if err != nil {
			return nil, err
		}
		for day, models := range held.days {
			for model, u := range models {
				if sum[day] == nil {
					sum[day] = map[proto.ModelRef]proto.Usage{}
				}
				sum[day][model] = sum[day][model].Add(u)
			}
		}
	}
	return sum, nil
}

// sessionUsage brings the sums kept of row's session up to its newest
// entry, from since on at least, and drops the days before oldest, which
// no report asks for. Sums whose read fails stay as they were.
func (s *Server) sessionUsage(ctx context.Context, row store.SessionRow, since, oldest time.Time) (*sessionUsage, error) {
	held := s.usageSums[row.ID]
	if held == nil || since.Before(held.from) {
		held = &sessionUsage{from: since, days: usageDays{}}
	}
	entries, newest, err := s.engine.UsageEntries(ctx, row.ID, held.through, held.from)
	if err != nil {
		return nil, fmt.Errorf("usage of session %d: %w", row.ID, err)
	}
	model := proto.ModelRef{Provider: row.Provider, ID: row.Model}
	for _, e := range entries {
		held.days.add(e, model, s.zone)
	}
	held.through = max(held.through, newest)
	if held.from.Before(oldest) {
		first := oldest.Format(proto.UsageDayLayout)
		for day := range held.days {
			if day < first {
				delete(held.days, day)
			}
		}
		held.from = oldest
	}
	s.usageSums[row.ID] = held
	return held, nil
}
