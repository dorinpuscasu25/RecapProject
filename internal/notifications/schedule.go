// Package notifications implements timezone-aware digest schedules, device registration,
// anti-spam policy, deduplicated delivery records and push provider adapters (APNs, FCM).
package notifications

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Schedule is the pure scheduling model (DST-correct, unit-tested).
type Schedule struct {
	Kind       string   // daily | weekly | custom
	LocalTimes []string // "HH:MM"
	Weekdays   []int    // ISO 1=Mon..7=Sun
	Timezone   string
	QuietStart string // "HH:MM" or ""
	QuietEnd   string
}

func parseHM(s string) (int, int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid time %q (HH:MM)", s)
	}
	return t.Hour(), t.Minute(), nil
}

// Validate checks the schedule fields.
func (s Schedule) Validate() error {
	if _, err := time.LoadLocation(s.Timezone); err != nil || s.Timezone == "" || s.Timezone == "Local" {
		return fmt.Errorf("invalid IANA timezone %q", s.Timezone)
	}
	if len(s.LocalTimes) == 0 || len(s.LocalTimes) > 6 {
		return fmt.Errorf("1..6 local times required")
	}
	for _, t := range s.LocalTimes {
		if _, _, err := parseHM(t); err != nil {
			return err
		}
	}
	switch s.Kind {
	case "daily":
	case "weekly":
		if len(s.Weekdays) != 1 {
			return fmt.Errorf("weekly schedules need exactly one weekday")
		}
	case "custom":
		if len(s.Weekdays) == 0 {
			return fmt.Errorf("custom schedules need weekdays")
		}
	default:
		return fmt.Errorf("kind must be daily, weekly or custom")
	}
	for _, d := range s.Weekdays {
		if d < 1 || d > 7 {
			return fmt.Errorf("weekdays are ISO 1..7")
		}
	}
	if (s.QuietStart == "") != (s.QuietEnd == "") {
		return fmt.Errorf("quiet hours need both start and end")
	}
	if s.QuietStart != "" {
		if _, _, err := parseHM(s.QuietStart); err != nil {
			return err
		}
		if _, _, err := parseHM(s.QuietEnd); err != nil {
			return err
		}
	}
	return nil
}

func isoWeekday(t time.Time) int {
	w := int(t.Weekday())
	if w == 0 {
		return 7
	}
	return w
}

func (s Schedule) dayAllowed(t time.Time) bool {
	if s.Kind == "daily" {
		return true
	}
	wd := isoWeekday(t)
	for _, d := range s.Weekdays {
		if d == wd {
			return true
		}
	}
	return false
}

// Next returns the first occurrence strictly after `after`, in UTC, plus its occurrence key.
// Local wall-clock times that do not exist (DST spring-forward gap) are moved forward by the gap
// (Go normalisation); ambiguous times (fall-back) fire once, at the first instance.
func (s Schedule) Next(after time.Time) (time.Time, string, error) {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return time.Time{}, "", err
	}
	times := append([]string{}, s.LocalTimes...)
	sort.Strings(times)
	local := after.In(loc)
	for day := 0; day < 15; day++ {
		d := time.Date(local.Year(), local.Month(), local.Day()+day, 12, 0, 0, 0, loc)
		if !s.dayAllowed(d) {
			continue
		}
		for _, hm := range times {
			h, m, _ := parseHM(hm)
			cand := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, loc)
			if cand.After(after) {
				key := fmt.Sprintf("%s@%s", time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, time.UTC).Format("2006-01-02T15:04"), s.Timezone)
				return cand.UTC(), key, nil
			}
		}
	}
	return time.Time{}, "", fmt.Errorf("no occurrence within 15 days")
}

// InQuietHours reports whether t falls inside the quiet window (supports overnight windows).
func (s Schedule) InQuietHours(t time.Time) bool {
	if s.QuietStart == "" {
		return false
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return false
	}
	cur := t.In(loc).Format("15:04")
	if s.QuietStart <= s.QuietEnd {
		return cur >= s.QuietStart && cur < s.QuietEnd
	}
	return cur >= s.QuietStart || cur < s.QuietEnd
}

func normalizeTimes(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if h, m, err := parseHM(t); err == nil {
			t = fmt.Sprintf("%02d:%02d", h, m)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}
