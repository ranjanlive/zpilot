package trigger

import (
	"testing"
	"time"
)

func TestTriggers(t *testing.T) {
	now := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	cases := []struct {
		tr   Trigger
		want time.Time
	}{
		{Trigger{Kind: "interval", EverySeconds: 3600}, now.Add(time.Hour)},
		{Trigger{Kind: "random_interval", MinSeconds: 72000, MaxSeconds: 108000}, now.Add(30 * time.Hour)},
		{Trigger{Kind: "weekly", Days: []int{6}, Time: "09:00", Timezone: "Asia/Kolkata"}, time.Date(2026, 10, 17, 3, 30, 0, 0, time.UTC)},
		{Trigger{Kind: "random_weekly", Days: []int{6}, WindowStart: "10:00", WindowEnd: "12:00", Timezone: "Asia/Kolkata"}, time.Date(2026, 10, 17, 6, 30, 0, 0, time.UTC)},
		{Trigger{Kind: "once", At: now.Add(time.Hour)}, now.Add(time.Hour)},
	}
	for _, c := range cases {
		n, e := Next(c.tr, now, func(n int64) int64 { return n })
		if e != nil || !n.Equal(c.want) {
			t.Fatalf("%s: %v %v want %v", c.tr.Kind, n, e, c.want)
		}
	}
	n, e := Next(Trigger{Kind: "daily", Time: "02:30", Timezone: "America/New_York"}, time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), nil)
	if e != nil || n.Day() != 9 {
		t.Fatalf("DST missing time: %v %v", n, e)
	}
	for _, tr := range []Trigger{{Kind: "interval", EverySeconds: 0}, {Kind: "random_interval", MinSeconds: 2, MaxSeconds: 1}, {Kind: "weekly", Days: []int{9}, Time: "00:00"}, {Kind: "random_weekly", Days: []int{6}, WindowStart: "23:00", WindowEnd: "01:00"}, {Kind: "daily", Time: "25:00"}} {
		if tr.Validate() == nil {
			t.Fatal("invalid trigger accepted", tr)
		}
	}
}
