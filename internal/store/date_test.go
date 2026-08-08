package store

import "testing"

// Floating-date tests (DD-9, TV-8).
//
// The timezone dimension is exercised by running the whole package under two
// zones 25 hours apart — see the -race/TZ matrix in the verification report.
// Because nothing in date.go calls into the time package, the results are
// identical by construction; the TZ runs prove that claim rather than assume it.

func TestValidDate(t *testing.T) {
	valid := []string{"2026-12-28", "2027-01-03", "2024-02-29", "2000-02-29", "1999-12-31"}
	for _, s := range valid {
		if !ValidDate(s) {
			t.Errorf("ValidDate(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"", "2026-1-3", "2026/12/28", "20261228", "2026-13-01", "2026-00-10",
		"2026-12-32", "2026-12-00", "2023-02-29", "1900-02-29", "2026-12-28T00:00:00Z",
		"abcd-ef-gh", "2026-12-2a",
	}
	for _, s := range invalid {
		if ValidDate(s) {
			t.Errorf("ValidDate(%q) = true, want false", s)
		}
	}
}

func TestCompareDate(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2026-12-28", "2027-01-03", -1},
		{"2027-01-03", "2026-12-28", 1},
		{"2026-12-28", "2026-12-28", 0},
		{"2026-01-09", "2026-01-10", -1}, // lexicographic == chronological only because of zero-padding
	}
	for _, c := range cases {
		if got := CompareDate(c.a, c.b); got != c.want {
			t.Errorf("CompareDate(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNextDateRollovers(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-12-28", "2026-12-29"},
		{"2026-12-31", "2027-01-01"}, // year
		{"2026-01-31", "2026-02-01"}, // month
		{"2024-02-28", "2024-02-29"}, // leap
		{"2024-02-29", "2024-03-01"},
		{"2023-02-28", "2023-03-01"}, // non-leap
		{"2000-02-28", "2000-02-29"}, // century leap
		{"1900-02-28", "1900-03-01"}, // century non-leap
		{"2026-04-30", "2026-05-01"},
	}
	for _, c := range cases {
		got, err := NextDate(c.in)
		if err != nil {
			t.Errorf("NextDate(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NextDate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := NextDate("nope"); err == nil {
		t.Error("NextDate accepted an invalid date")
	}
}

// TestExpandRangeTV8 is the TV-8 range: 2026-12-28..2027-01-03 spans a year
// boundary and must be exactly 7 days regardless of the ambient timezone.
func TestExpandRangeTV8(t *testing.T) {
	days, err := ExpandRange("2026-12-28", "2027-01-03")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"2026-12-28", "2026-12-29", "2026-12-30", "2026-12-31",
		"2027-01-01", "2027-01-02", "2027-01-03",
	}
	if len(days) != len(want) {
		t.Fatalf("ExpandRange len = %d, want %d: %v", len(days), len(want), days)
	}
	for i := range want {
		if days[i] != want[i] {
			t.Errorf("day[%d] = %q, want %q", i, days[i], want[i])
		}
	}
	n, err := DayCount("2026-12-28", "2027-01-03")
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("DayCount = %d, want 7 (TV-8)", n)
	}
}

func TestExpandRangeSingleDay(t *testing.T) {
	days, err := ExpandRange("2026-08-04", "2026-08-04")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || days[0] != "2026-08-04" {
		t.Errorf("single-day range = %v, want [2026-08-04]", days)
	}
}

func TestExpandRangeRejectsInverted(t *testing.T) {
	if _, err := ExpandRange("2027-01-03", "2026-12-28"); err == nil {
		t.Error("ExpandRange accepted start > end")
	}
	if _, err := ExpandRange("2026-13-01", "2026-12-28"); err == nil {
		t.Error("ExpandRange accepted an invalid start")
	}
}

// TestDateHelpersDoNotUseTimePackage is the mechanical half of DD-9: the source
// of date.go must contain no reference to the time package. A helper that
// "happens to give the right answer today" is not the requirement — the
// requirement is that no timezone conversion path exists at all, because that
// defect is invisible in single-zone development.
//
// This is asserted against the file on disk in TestNoTimePackageInDateSource
// (source_guard_test.go) so it fails if someone later reaches for time.Parse.
func TestFloatingDateIsPureStringArithmetic(t *testing.T) {
	// Dates far outside any plausible time.Time-based implementation's comfort
	// zone still work, because there is no conversion involved.
	for _, c := range []struct{ in, want string }{
		{"1583-12-31", "1584-01-01"},
		{"9999-12-30", "9999-12-31"},
	} {
		got, err := NextDate(c.in)
		if err != nil {
			t.Errorf("NextDate(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NextDate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestOptionRejectsInvalidDates checks the store-level guard, so a bad range
// never reaches the event log.
func TestOptionRejectsInvalidDates(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "h1", "Ann"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("P1", "U1", "Trip", ""); err != nil {
		t.Fatal(err)
	}
	bad := [][2]string{
		{"2026-13-01", "2026-12-01"},
		{"2026-12-05", "2026-12-01"}, // inverted
		{"not-a-date", "2026-12-01"},
		{"2026-12-01", ""},
	}
	for _, c := range bad {
		if err := s.CreateOption("P1", "OX", "U1", c[0], c[1], ""); err == nil {
			t.Errorf("CreateOption accepted %v", c)
		}
	}
	p, _ := s.Proposal("P1")
	if len(p.Options) != 0 {
		t.Errorf("invalid options reached the store: %d", len(p.Options))
	}
}
