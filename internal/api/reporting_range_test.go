package api

import (
	"testing"
	"time"
)

func TestReportingRangeBusinessDays(t *testing.T) {
	now := time.Date(2026, 10, 1, 16, 30, 0, 0, time.UTC)
	for _, tc := range []struct{ from, to, wantFrom, wantTo string }{
		{"2026-10-02", "2026-10-02", "2026-10-01T16:00:00Z", "2026-10-02T16:00:00Z"},
		{"2026-10-01", "2026-10-31", "2026-09-30T16:00:00Z", "2026-10-31T16:00:00Z"},
		{"2028-02-29", "2028-02-29", "2028-02-28T16:00:00Z", "2028-02-29T16:00:00Z"},
		{"", "", "2026-09-02T16:00:00Z", "2026-10-02T16:00:00Z"},
	} {
		from, to, err := reportingRange(tc.from, tc.to, now)
		if err != nil || from.UTC().Format(time.RFC3339) != tc.wantFrom || to.UTC().Format(time.RFC3339) != tc.wantTo {
			t.Fatalf("%q..%q = %v..%v (%v), want %s..%s", tc.from, tc.to, from, to, err, tc.wantFrom, tc.wantTo)
		}
	}
	for _, tc := range [][2]string{{"bad", "2026-10-01"}, {"2026-02-29", "2026-03-01"}, {"2026-10-02", "2026-10-01"}} {
		if _, _, err := reportingRange(tc[0], tc[1], now); err == nil {
			t.Fatalf("accepted invalid range %v", tc)
		}
	}
}
