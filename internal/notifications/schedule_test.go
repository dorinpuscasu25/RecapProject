package notifications

import (
	"testing"
	"time"
)

func TestNextDailyAcrossDSTSpringForward(t *testing.T) {
	// Europe/Chisinau switches to summer time on the last Sunday of March (2026-03-29) at 02:00→03:00.
	s := Schedule{Kind: "daily", LocalTimes: []string{"08:00"}, Timezone: "Europe/Chisinau"}
	before := time.Date(2026, 3, 28, 7, 0, 0, 0, time.UTC) // 09:00 local (UTC+2) on 28 Mar
	next, key, err := s.Next(before)
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Europe/Chisinau")
	if got := next.In(loc); got.Day() != 29 || got.Hour() != 8 || got.Minute() != 0 {
		t.Fatalf("expected 29 Mar 08:00 local, got %v", got)
	}
	if next.UTC().Hour() != 5 { // UTC+3 after the switch
		t.Fatalf("expected 05:00 UTC after DST, got %v", next.UTC())
	}
	if key != "2026-03-29T08:00@Europe/Chisinau" {
		t.Fatalf("occurrence key: %s", key)
	}
}

func TestNonexistentLocalTimeDuringGap(t *testing.T) {
	// Europe/Chisinau springs forward 03:00→04:00 local, so 03:30 does not exist that day.
	s := Schedule{Kind: "daily", LocalTimes: []string{"03:30"}, Timezone: "Europe/Chisinau"}
	next, _, err := s.Next(time.Date(2026, 3, 28, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Europe/Chisinau")
	if got := next.In(loc); got.Day() != 29 || got.Hour() != 4 || got.Minute() != 30 {
		t.Fatalf("03:30 does not exist on 29 Mar; expected normalisation to 04:30, got %v", got)
	}
}

func TestFallBackFiresOnce(t *testing.T) {
	// 25 Oct 2026: 04:00 summer → 03:00 winter; 03:30 occurs twice.
	s := Schedule{Kind: "daily", LocalTimes: []string{"03:30"}, Timezone: "Europe/Chisinau"}
	first, k1, _ := s.Next(time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC))
	second, k2, _ := s.Next(first)
	if k1 == k2 {
		t.Fatalf("same occurrence twice: %s", k1)
	}
	if second.Sub(first) < 23*time.Hour {
		t.Fatalf("ambiguous time fired twice: %v then %v", first, second)
	}
}

func TestWeeklyAndQuietHours(t *testing.T) {
	s := Schedule{Kind: "weekly", LocalTimes: []string{"19:30"}, Weekdays: []int{5}, Timezone: "Europe/Chisinau", QuietStart: "22:00", QuietEnd: "07:00"}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	next, _, _ := s.Next(time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)) // Tuesday
	loc, _ := time.LoadLocation("Europe/Chisinau")
	if got := next.In(loc); got.Weekday() != time.Friday || got.Hour() != 19 {
		t.Fatalf("weekly: %v", got)
	}
	if !s.InQuietHours(time.Date(2026, 10, 13, 21, 30, 0, 0, time.UTC)) { // 00:30 local
		t.Fatal("00:30 should be quiet (overnight window)")
	}
	if s.InQuietHours(time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)) {
		t.Fatal("12:00 should not be quiet")
	}
}

func TestValidation(t *testing.T) {
	bad := []Schedule{
		{Kind: "daily", LocalTimes: []string{"25:00"}, Timezone: "Europe/Chisinau"},
		{Kind: "daily", LocalTimes: []string{"08:00"}, Timezone: "Mars/Olympus"},
		{Kind: "weekly", LocalTimes: []string{"08:00"}, Timezone: "UTC"},
		{Kind: "daily", LocalTimes: []string{"08:00"}, Timezone: "UTC", QuietStart: "22:00"},
	}
	for i, s := range bad {
		if s.Validate() == nil {
			t.Errorf("case %d should be invalid", i)
		}
	}
}
