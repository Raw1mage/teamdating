package store

import (
	"os"
	"strings"
	"testing"
)

// TestNoTimePackageInDateSource is a wiring guard, not a behaviour test.
//
// DD-9 forbids routing trip dates through time.Time. A behavioural test cannot
// catch a violation of that: a time.Time-based implementation returns the right
// answer in the developer's own timezone and only diverges for particular zone
// combinations. So the assertion has to be about the code itself.
//
// It reads the file from disk deliberately. Asserting on a variable or an
// injected dependency would test the seam and leave the actual wiring — the one
// line that would break this — unexecuted by any test.
func TestNoTimePackageInDateSource(t *testing.T) {
	const path = "date.go"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(b)

	// Control group: prove the check can actually fire. If this needle is not
	// found in a string we know contains it, the matcher is broken and a clean
	// result below would mean nothing (absent and failing must not share one
	// output).
	const canary = `time.Parse("2006-01-02", s)`
	if !containsAny(canary, []string{"time."}) {
		t.Fatal("control group failed: the matcher does not detect a known " +
			"violation, so a clean result proves nothing")
	}

	forbidden := []string{`"time"`, "time.Parse", "time.Date", "time.Now", "time.Time"}
	// Strip comments so prose explaining WHY we avoid time.Time is not a hit.
	code := stripComments(src)
	for _, needle := range forbidden {
		if strings.Contains(code, needle) {
			t.Errorf("%s references %s — floating dates must never pass through "+
				"a timezone conversion path (DD-9, TV-8)", path, needle)
		}
	}
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// stripComments removes // and /* */ comments. It is deliberately simple; the
// only inputs are this package's own source files.
func stripComments(src string) string {
	var out strings.Builder
	i := 0
	for i < len(src) {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = len(src)
			} else {
				i += 2 + end + 2
			}
		default:
			out.WriteByte(src[i])
			i++
		}
	}
	return out.String()
}
