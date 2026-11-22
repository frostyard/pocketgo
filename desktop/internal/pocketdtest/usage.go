package pocketdtest

import (
	"net/http"
	"slices"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// SetUsage replaces the days GET /v1/usage answers with (ADR-0052): those
// from the request's since on. A report that names no zone, today or end
// of today is in UTC, on the fake's clock.
func (s *Server) SetUsage(r proto.UsageReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Days = slices.Clone(r.Days)
	s.usage = r
}

func (s *Server) usageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+proto.PathUsage, func(w http.ResponseWriter, r *http.Request) {
		since := r.URL.Query().Get("since")
		if day, err := time.Parse(proto.UsageDayLayout, since); err != nil || day.Format(proto.UsageDayLayout) != since {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "since must be a day, YYYY-MM-DD")
			return
		}
		s.mu.Lock()
		report := s.usage
		s.mu.Unlock()
		if report.Zone == "" {
			report.Zone = "UTC"
		}
		if report.Today == "" {
			report.Today = Now.Format(proto.UsageDayLayout)
		}
		if report.TodayEnds.IsZero() {
			y, m, d := Now.Date()
			report.TodayEnds = time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
		}
		report.Since = since
		days := []proto.UsageDay{}
		for _, day := range report.Days {
			if day.Day >= since && day.Day <= report.Today {
				days = append(days, day)
			}
		}
		report.Days = days
		writeJSON(w, http.StatusOK, report)
	})
}
