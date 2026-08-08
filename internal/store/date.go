package store

import "fmt"

// Floating-date helpers (DD-9, risk J).
//
// A trip date has no timezone: "we leave on 8/4" means the same day to someone
// in Taipei and someone in Tokyo. Storing a timezone-bearing value makes
// members in different zones see an off-by-one day, and — this is the part that
// matters — that defect only appears for particular zone combinations, so it is
// invisible in single-timezone local development (TV-8).
//
// Therefore NOTHING in this file calls time.Parse, time.Date, or any other
// time-package entry point. Dates are "YYYY-MM-DD" strings throughout, and
// because ISO 8601 is lexicographically ordered, string comparison IS date
// comparison. Day arithmetic is done on the integer triple directly.

// ValidDate reports whether s is a well-formed YYYY-MM-DD calendar date.
func ValidDate(s string) bool {
	_, _, _, ok := parseDate(s)
	return ok
}

func parseDate(s string) (y, m, d int, ok bool) {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return 0, 0, 0, false
	}
	num := func(a, b int) (int, bool) {
		n := 0
		for i := a; i < b; i++ {
			c := s[i]
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		return n, true
	}
	var okY, okM, okD bool
	if y, okY = num(0, 4); !okY {
		return 0, 0, 0, false
	}
	if m, okM = num(5, 7); !okM {
		return 0, 0, 0, false
	}
	if d, okD = num(8, 10); !okD {
		return 0, 0, 0, false
	}
	if m < 1 || m > 12 || d < 1 || d > daysInMonth(y, m) {
		return 0, 0, 0, false
	}
	return y, m, d, true
}

func isLeap(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

func daysInMonth(y, m int) int {
	switch m {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	case 2:
		if isLeap(y) {
			return 29
		}
		return 28
	}
	return 0
}

func formatDate(y, m, d int) string {
	buf := []byte("0000-00-00")
	buf[0] = byte('0' + y/1000%10)
	buf[1] = byte('0' + y/100%10)
	buf[2] = byte('0' + y/10%10)
	buf[3] = byte('0' + y%10)
	buf[5] = byte('0' + m/10%10)
	buf[6] = byte('0' + m%10)
	buf[8] = byte('0' + d/10%10)
	buf[9] = byte('0' + d%10)
	return string(buf)
}

// CompareDate returns -1, 0 or +1. Both inputs must be valid dates; for
// well-formed input this is exactly strings.Compare, which is the point.
func CompareDate(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// NextDate returns the day after s.
func NextDate(s string) (string, error) {
	y, m, d, ok := parseDate(s)
	if !ok {
		return "", fmt.Errorf("invalid date %q (want YYYY-MM-DD)", s)
	}
	d++
	if d > daysInMonth(y, m) {
		d = 1
		m++
		if m > 12 {
			m = 1
			y++
		}
	}
	return formatDate(y, m, d), nil
}

// ExpandRange lists every day from start to end inclusive.
func ExpandRange(start, end string) ([]string, error) {
	if err := validateRange(start, end); err != nil {
		return nil, err
	}
	out := []string{}
	cur := start
	for {
		out = append(out, cur)
		if cur == end {
			return out, nil
		}
		next, err := NextDate(cur)
		if err != nil {
			return nil, err
		}
		cur = next
	}
}

// DayCount is the inclusive number of days in the range.
func DayCount(start, end string) (int, error) {
	days, err := ExpandRange(start, end)
	if err != nil {
		return 0, err
	}
	return len(days), nil
}

func validateRange(start, end string) error {
	if !ValidDate(start) {
		return fmt.Errorf("invalid startDate %q (want YYYY-MM-DD)", start)
	}
	if !ValidDate(end) {
		return fmt.Errorf("invalid endDate %q (want YYYY-MM-DD)", end)
	}
	if CompareDate(start, end) > 0 {
		return fmt.Errorf("startDate %s is after endDate %s", start, end)
	}
	return nil
}
