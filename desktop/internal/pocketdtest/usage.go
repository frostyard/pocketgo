package pocketdtest

import (
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// SetUsage replaces the days GET /v1/usage answers with (ADR-0052): those
// from the request's since through today. A report that names no zone is
// in UTC; its today is the day of the fake's clock in its zone, and its
// day ends at the next midnight there, as pocketd's does, unless the
// report names them.
func (s *Server) SetUsage(r proto.UsageReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Days = slices.Clone(r.Days)
	s.usage = r
}

func (s *Server) usageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+proto.PathUsage, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		report := s.usage
		s.mu.Unlock()
		if report.Zone == "" {
			report.Zone = "UTC"
		}
		zone, err := time.LoadLocation(report.Zone)
		if err != nil {
			writeError(w, http.StatusInternalServerError, proto.ErrorCodeInternal, "the fake's zone: "+err.Error())
			return
		}
		if report.Today == "" {
			report.Today = Now.In(zone).Format(proto.UsageDayLayout)
		}
		today, err := time.Parse(proto.UsageDayLayout, report.Today)
		if err != nil {
			writeError(w, http.StatusInternalServerError, proto.ErrorCodeInternal, "the fake's today: "+err.Error())
			return
		}
		if report.TodayEnds.IsZero() {
			report.TodayEnds = time.Date(today.Year(), today.Month(), today.Day()+1, 0, 0, 0, 0, zone).UTC()
		}
		// The since pocketd takes (protocol rule 63): a day, at most
		// proto.MaxUsageDays-1 days before today.
		since := r.URL.Query().Get("since")
		day, err := time.Parse(proto.UsageDayLayout, since)
		if err != nil || day.Format(proto.UsageDayLayout) != since {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "since must be a day, YYYY-MM-DD")
			return
		}
		if oldest := today.AddDate(0, 0, -(proto.MaxUsageDays - 1)); day.Before(oldest) {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest,
				fmt.Sprintf("since is at most %d days before today, %s: %s is earlier", proto.MaxUsageDays-1, oldest.Format(proto.UsageDayLayout), since))
			return
		}
		report.Since = since
		days := []proto.UsageDay{}
		for _, d := range report.Days {
			if d.Day >= since && d.Day <= report.Today {
				days = append(days, d)
			}
		}
		report.Days = days
		writeJSON(w, http.StatusOK, report)
	})
}
