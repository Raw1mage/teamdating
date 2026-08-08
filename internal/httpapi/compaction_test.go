package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"teamdating/internal/store"
)

// --- 6.3 compaction wiring -------------------------------------------------

// TestCompactionRunsAfterWrites drives the REAL middleware with a low
// threshold and asserts the log actually gets truncated.
func TestCompactionRunsAfterWrites(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// threshold 4: the register below (1 event) must NOT trigger; the writes
	// after it must.
	//
	// ★ THE THRESHOLD MOVED FROM 3 TO 4 WHEN DD-7 LANDED, and the reason is
	// arithmetic rather than taste. The owner must now hold an email before a
	// proposal is accepted, so this fixture writes one more event than it used
	// to (register, set-email, proposal, 3 options = 6). At threshold 3 the
	// extra event lines compaction up with the LAST write, truncating to an
	// empty log and leaving nothing to measure — an empty log reads exactly
	// like a log that was never written, which is the collapse this test exists
	// to catch. At 4, compaction fires on the first option and the two after it
	// remain, so the assertion below still distinguishes truncated from
	// untouched.
	inner := New(st, emptyFS{}, "test", "", "")
	srv := httptest.NewServer(compactAfterWrite(st, 4, inner))
	t.Cleanup(srv.Close)

	e := &env{t: t, srv: srv, st: st, dir: dir}
	c := e.newClient("Ann")

	logPath := filepath.Join(dir, "events.jsonl")
	if logLines(t, logPath) != 1 {
		t.Fatalf("precondition: registering wrote %d log lines, want 1",
			logLines(t, logPath))
	}
	// One event is below the threshold, so nothing may have been compacted yet.
	// Without this the test would pass even if compaction ran on every write.
	//
	// This check runs BEFORE the email is set, so the count here is still the
	// single register event.
	if _, err := os.Stat(filepath.Join(dir, "snapshot.json")); err == nil {
		t.Fatal("compaction ran below the threshold: the trigger is not counting")
	}

	var pr struct {
		PID string `json:"pid"`
	}
	// The proposal gate now requires the owner to hold an email (DD-7); without
	// this the request below is refused and this test measures a 403 instead of
	// compaction.
	c.ensureEmail()
	if code := c.do("POST", "/api/proposals",
		map[string]any{"title": "Trip", "desc": ""}, &pr); code != http.StatusOK {
		t.Fatalf("create proposal: %d", code)
	}
	for i, d := range []string{"2026-03-01", "2026-04-01", "2026-05-01"} {
		code := c.do("POST", "/api/proposals/"+pr.PID+"/options",
			map[string]any{"startDate": d, "endDate": d, "note": ""}, nil)
		if code != http.StatusOK {
			t.Fatalf("create option %d: %d", i, code)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "snapshot.json")); err != nil {
		t.Fatalf("no snapshot after 6 writes past a threshold of 4: %v", err)
	}
	// 6 events (register, set-email, proposal, 3 options), threshold 4:
	// compaction fires ON the fourth and truncates, so exactly the 2 events
	// after it remain.
	//
	// ★ Asserted as a LINE COUNT, not a byte size. Comparing sizes against
	// "after one write" reads a correctly-compacted log holding 2 later events
	// as "never truncated" — which is what this assertion did on its first
	// draft, reporting a defect that did not exist.
	if n := logLines(t, logPath); n != 2 {
		t.Errorf("event log holds %d lines after compaction, want 2 "+
			"(6 events written, threshold 4)", n)
	}

	// The state must survive the compaction it just went through.
	st.Close()
	reopened, err := store.New(dir)
	if err != nil {
		t.Fatalf("reopen after compaction: %v", err)
	}
	defer reopened.Close()
	p, ok := reopened.Proposal(pr.PID)
	if !ok {
		t.Fatal("proposal lost across compaction + restart")
	}
	if len(p.Options) != 3 {
		t.Errorf("options after compaction + restart = %d, want 3", len(p.Options))
	}
	if reopened.UserCount() != 1 {
		t.Errorf("users after compaction + restart = %d, want 1", reopened.UserCount())
	}
}

// TestReadsDoNotTriggerCompaction: a GET must not take the crash window, even
// when the counter is already past the threshold.
//
// ★ The setup writes DIRECTLY to the store rather than over HTTP, and that is
// the whole point. Driven through the API, the write that crosses the
// threshold compacts on its own way out, so the counter is never left standing
// above it — and a GET would find nothing due no matter what the middleware
// did. Measured: the first draft of this test did exactly that, passed, and
// passed just as happily with the GET early-return DELETED. It was asserting
// on the counter, never on the branch it claimed to guard.
func TestReadsDoNotTriggerCompaction(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv := httptest.NewServer(compactAfterWrite(st, 2, New(st, emptyFS{}, "test", "", "")))
	t.Cleanup(srv.Close)

	// Bypass the middleware so the counter ends up ABOVE the threshold with no
	// compaction having run.
	if err := st.CreateUser("U1", "h1", "Ann"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProposal("P1", "U1", "Trip", ""); err != nil {
		t.Fatal(err)
	}
	if n := st.PendingSinceCompact(); n < 2 {
		t.Fatalf("precondition: pending=%d, need >= threshold 2 for a GET to "+
			"be capable of triggering compaction at all", n)
	}

	c := &client{t: t, srv: srv}
	for i := 0; i < 5; i++ {
		if code := c.do("GET", "/api/health", nil, nil); code != http.StatusOK {
			t.Fatalf("health: %d", code)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshot.json")); err == nil {
		t.Error("a GET triggered compaction while the counter was past the " +
			"threshold: reads must not take the crash window")
	}
	if n := st.PendingSinceCompact(); n < 2 {
		t.Errorf("the GETs moved the counter (pending=%d): reads must not "+
			"count toward compaction either", n)
	}
}

// TestCompactionIsWiredIntoNew is a wiring guard, not a behaviour test.
//
// ★ The behaviour tests above pass an INJECTED threshold, so they would stay
// green if New forgot to install the middleware at all — the exact shape of
// defect this project has already paid for once (a durability guard that lived
// only inside a throwaway binary). The one production line that matters is
// therefore asserted against the source itself.
func TestCompactionIsWiredIntoNew(t *testing.T) {
	const path = "httpapi.go"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(b)

	// Control group: prove the matcher fires on a string known to contain the
	// needle. Without this, a typo'd needle and a missing wiring produce the
	// same clean result.
	if !strings.Contains("return limitWrites(w, r, compactAfterWrite(st, x, mux))",
		"compactAfterWrite(") {
		t.Fatal("control group failed: the matcher cannot detect a known call, " +
			"so a clean result below would prove nothing")
	}

	code := stripGoComments(src)
	if !strings.Contains(code, "compactAfterWrite(") {
		t.Fatal("New does not install compactAfterWrite: compaction would never " +
			"run in production and the event log would grow without bound, " +
			"with every test still green")
	}
	// It must be wired with the production constant, not a literal that could
	// silently be 0 (which CompactIfDue would then treat as the default) or a
	// number that drifts away from the documented one.
	if !strings.Contains(code, "store.DefaultCompactThreshold") {
		t.Error("compactAfterWrite is not wired with store.DefaultCompactThreshold")
	}
}

// logLines counts non-empty lines in the event log. A byte size cannot answer
// "how many events are still here", which is the actual question.
func logLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	n := 0
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}
