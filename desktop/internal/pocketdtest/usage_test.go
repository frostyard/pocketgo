package pocketdtest

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// The fake answers GET /v1/usage as pocketd does (protocol rule 64): it
// refuses a since more than 91 days before its today, and its today and
// the day's end are of its clock in the report's zone (Codex review of
// #107, round 1).
func TestTheFakesUsageIsPocketds(t *testing.T) {
	s := New()
	s.SetUsage(proto.UsageReport{Zone: "America/New_York"})
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Now is 22:00 in UTC on the 18th: 17:00 in New York, whose day
	// ends at 05:00 in UTC.
	r, err := client.Usage(ctx, "2026-08-19")
	if err != nil || r.Today != "2026-11-18" || !r.TodayEnds.Equal(time.Date(2026, 11, 19, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("report %+v, %v", r, err)
	}
	var pe *proto.Error
	if _, err := client.Usage(ctx, "2026-08-18"); !errors.As(err, &pe) || pe.Status != http.StatusBadRequest || pe.Code != proto.ErrorCodeBadRequest {
		t.Fatalf("a since 92 days back: %v", err)
	}
	// A day whose midnight the zone's clocks skip ends the day before as
	// they move on: America/Santiago's 2026-09-06 starts at 01:00 (Codex
	// review of #107, round 2).
	s.SetUsage(proto.UsageReport{Zone: "America/Santiago", Today: "2026-09-05"})
	if r, err := client.Usage(ctx, "2026-09-01"); err != nil || !r.TodayEnds.Equal(time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("the end of a day before a skipped midnight: %+v, %v", r, err)
	}
}
