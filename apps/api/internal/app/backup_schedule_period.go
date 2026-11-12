package app

import (
	"sort"
	"time"
)

func backupSchedulePeriod(p backupSchedule) string {
	if p.Period == "" {
		return "daily"
	}
	return p.Period
}

func backupScheduleInterval(p backupSchedule) (time.Duration, error) {
	var unit time.Duration
	var maximum, minimum int
	switch p.Unit {
	case "minutes":
		unit, maximum, minimum = time.Minute, 366*24*60, 5
	case "hours":
		unit, maximum, minimum = time.Hour, 8760, 1
	case "days":
		unit, maximum, minimum = 24*time.Hour, 366, 1
	default:
		return 0, errBackupInvalid
	}
	if p.Interval < minimum || p.Interval > maximum {
		return 0, errBackupInvalid
	}
	return time.Duration(p.Interval) * unit, nil
}

func validateBackupSchedulePeriod(p backupSchedule) error {
	if _, _, err := parseBackupSchedule(p.Time, p.Timezone); err != nil {
		return err
	}
	period := backupSchedulePeriod(p)
	if period != "weekly" && len(p.Weekdays) != 0 || period != "monthly" && p.DayOfMonth != 0 || period != "hourly" && p.Minute != 0 || period != "interval" && (p.Interval != 0 || p.Unit != "" || p.AnchorAt != nil) {
		return errBackupInvalid
	}
	switch period {
	case "daily":
	case "hourly":
		if p.Minute < 0 || p.Minute > 59 || p.Time != "00:00" {
			return errBackupInvalid
		}
	case "weekly":
		if len(p.Weekdays) < 1 || len(p.Weekdays) > 7 {
			return errBackupInvalid
		}
		seen := map[int]bool{}
		for _, day := range p.Weekdays {
			if day < 0 || day > 6 || seen[day] {
				return errBackupInvalid
			}
			seen[day] = true
		}
	case "monthly":
		if p.DayOfMonth < 1 || p.DayOfMonth > 31 {
			return errBackupInvalid
		}
	case "interval":
		if p.Time != "00:00" || p.AnchorAt == nil || p.AnchorAt.IsZero() || p.AnchorAt.Year() < 1 || p.AnchorAt.Year() > 9998 {
			return errBackupInvalid
		}
		if _, err := backupScheduleInterval(p); err != nil {
			return err
		}
	default:
		return errBackupInvalid
	}
	return nil
}

func backupScheduleDayOccurrences(day time.Time, p backupSchedule, wall int, loc *time.Location) []time.Time {
	y, m, d := day.In(loc).Date()
	switch backupSchedulePeriod(p) {
	case "weekly":
		matched := false
		for _, weekday := range p.Weekdays {
			matched = matched || int(day.In(loc).Weekday()) == weekday
		}
		if !matched {
			return nil
		}
	case "monthly":
		last := time.Date(y, m+1, 0, 12, 0, 0, 0, loc).Day()
		target := p.DayOfMonth
		if target > last {
			target = last
		}
		if d != target {
			return nil
		}
	}
	if backupSchedulePeriod(p) != "hourly" {
		if occurrence, ok := backupScheduleOccurrence(day, wall, loc); ok {
			return []time.Time{occurrence}
		}
		return nil
	}
	// Enumerate local wall hours separately: repeated hours select their first
	// occurrence, nonexistent wall hours use the same forward policy as daily.
	values := []time.Time{}
	seen := map[int64]bool{}
	for hour := 0; hour < 24; hour++ {
		if occurrence, ok := backupScheduleOccurrence(day, hour*60+p.Minute, loc); ok && !seen[occurrence.Unix()] {
			values = append(values, occurrence)
			seen[occurrence.Unix()] = true
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Before(values[j]) })
	return values
}

// Advance calendar labels independently of local timezone normalization. A
// completely skipped date (e.g. Samoa 2011-12-30) must not loop on the same
// normalized day or masquerade as a different month's scheduled occurrence.
func backupScheduleCalendarDay(reference time.Time, delta int, loc *time.Location) (time.Time, bool) {
	y, m, d := reference.In(loc).Date()
	label := time.Date(y, m, d, 12, 0, 0, 0, time.UTC).AddDate(0, 0, delta)
	y, m, d = label.Date()
	day := time.Date(y, m, d, 12, 0, 0, 0, loc)
	actualY, actualM, actualD := day.Date()
	return day, actualY == y && actualM == m && actualD == d
}

func nextBackupSchedulePeriod(after time.Time, p backupSchedule) (time.Time, error) {
	if err := validateBackupSchedulePeriod(p); err != nil {
		return time.Time{}, err
	}
	if backupSchedulePeriod(p) == "interval" {
		duration, _ := backupScheduleInterval(p)
		anchor := p.AnchorAt.UTC()
		if after.Before(anchor) {
			return anchor.Add(duration), nil
		}
		elapsed := after.Sub(anchor)
		return after.Add(duration - elapsed%duration).UTC(), nil
	}
	wall, loc, _ := parseBackupSchedule(p.Time, p.Timezone)
	for n := 0; n < 370; n++ {
		day, exists := backupScheduleCalendarDay(after, n, loc)
		if !exists {
			continue
		}
		for _, candidate := range backupScheduleDayOccurrences(day, p, wall, loc) {
			if candidate.After(after) {
				return candidate, nil
			}
		}
	}
	return time.Time{}, errBackupInvalid
}

func latestBackupSchedulePeriod(now time.Time, p backupSchedule) (time.Time, error) {
	if err := validateBackupSchedulePeriod(p); err != nil {
		return time.Time{}, err
	}
	if backupSchedulePeriod(p) == "interval" {
		duration, _ := backupScheduleInterval(p)
		anchor := p.AnchorAt.UTC()
		if now.Before(anchor.Add(duration)) {
			return time.Time{}, errBackupInvalid
		}
		return now.Add(-now.Sub(anchor) % duration).UTC(), nil
	}
	wall, loc, _ := parseBackupSchedule(p.Time, p.Timezone)
	lookback := 40
	if backupSchedulePeriod(p) == "monthly" {
		lookback = 370
	}
	for n := 0; n < lookback; n++ {
		day, exists := backupScheduleCalendarDay(now, -n, loc)
		if !exists {
			continue
		}
		values := backupScheduleDayOccurrences(day, p, wall, loc)
		for i := len(values) - 1; i >= 0; i-- {
			if !values[i].After(now) {
				return values[i], nil
			}
		}
	}
	return time.Time{}, errBackupInvalid
}
