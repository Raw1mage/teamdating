package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// --- 6.3 snapshot compaction ----------------------------------------------

// seedForCompact writes a known state: 2 users, 1 proposal, 2 options, 2 votes.
func seedForCompact(t *testing.T, s *Store) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateUser("U1", "h1", "Ann"))
	must(s.CreateUser("U2", "h2", "Bob"))
	must(s.CreateProposal("P1", "U1", "Trip", "desc"))
	must(s.CreateOption("P1", "O1", "U1", "2026-03-01", "2026-03-05", "n1"))
	must(s.CreateOption("P1", "O2", "U2", "2026-04-01", "2026-04-05", "n2"))
	must(s.SetVote("P1", "O1", "U1", WeightYes))
	must(s.SetVote("P1", "O1", "U2", WeightYes))
}

// assertSeeded checks the state seedForCompact produced, whatever path it
// arrived by (live, snapshot, snapshot+log).
func assertSeeded(t *testing.T, s *Store, what string) {
	t.Helper()
	if n := s.UserCount(); n != 2 {
		t.Errorf("%s: users = %d, want 2", what, n)
	}
	if n := s.ProposalCount(); n != 1 {
		t.Errorf("%s: proposals = %d, want 1", what, n)
	}
	p, ok := s.Proposal("P1")
	if !ok {
		t.Fatalf("%s: proposal P1 missing", what)
	}
	if len(p.Options) != 2 {
		t.Errorf("%s: options = %d, want 2", what, len(p.Options))
	}
	if len(p.Votes) != 2 {
		t.Errorf("%s: votes = %d, want 2", what, len(p.Votes))
	}
	if o, ok := p.Options["O2"]; !ok {
		t.Errorf("%s: option O2 missing", what)
	} else if o.StartDate != "2026-04-01" || o.AuthorUID != "U2" {
		t.Errorf("%s: option O2 = %+v, want start 2026-04-01 author U2", what, *o)
	}
	// byToken is NOT serialised; it must be rebuilt from Users on load. If it
	// were dropped, every existing bearer token would stop authenticating —
	// with no error anywhere, just "please register again".
	if uid, ok := s.UIDByTokenHash("h2"); !ok || uid != "U2" {
		t.Errorf("%s: token index lost: UIDByTokenHash(h2) = %q,%v", what, uid, ok)
	}
}

// TestCompactTruncatesLogAndPreservesState is the happy path: after Compact
// the state is unchanged, the log is empty, and a fresh open still sees
// everything.
func TestCompactTruncatesLogAndPreservesState(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	seedForCompact(t, s)

	before, err := os.ReadFile(filepath.Join(dir, logName))
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("precondition: event log is empty before compaction")
	}

	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	assertSeeded(t, s, "live after compact")

	after, err := os.ReadFile(filepath.Join(dir, logName))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("event log not truncated: %d bytes remain", len(after))
	}
	if _, err := os.Stat(filepath.Join(dir, snapshotName)); err != nil {
		t.Errorf("snapshot.json absent after compaction: %v", err)
	}
	// No temp files may survive a successful compaction.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), tmpSuffix) {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	s.Close()
	assertSeeded(t, newStore(t, dir), "reopened after compact")
}

// TestWritesAfterCompactAreDurable guards the reopen inside compactLocked.
//
// ★ After renaming a fresh file over the log, the old *os.File handle points
// at an unlinked inode. Appending through it succeeds, fsyncs, and returns nil
// — and the bytes are unreachable forever. "The write succeeded" and "the
// write went nowhere" share one output, which is precisely the shape this
// project keeps having to design against.
func TestWritesAfterCompactAreDurable(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	seedForCompact(t, s)
	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if err := s.CreateOption("P1", "O3", "U1", "2026-05-01", "2026-05-03", "after"); err != nil {
		t.Fatalf("post-compact write: %v", err)
	}
	s.Close()

	reopened := newStore(t, dir)
	p, ok := reopened.Proposal("P1")
	if !ok {
		t.Fatal("proposal missing after reopen")
	}
	if _, ok := p.Options["O3"]; !ok {
		t.Errorf("option written after compaction was lost on restart "+
			"(options present: %d)", len(p.Options))
	}
}

// TestSnapshotIsAtomic proves a partially-written snapshot cannot be observed:
// the bytes go to a temp file and become visible only through rename(2).
func TestSnapshotIsAtomic(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	seedForCompact(t, s)

	seen := []string{}
	s.SetCompactHookForControlGroup(func() {
		// Between the two renames the snapshot must already be complete and
		// parseable — never a prefix.
		raw, err := os.ReadFile(filepath.Join(dir, snapshotName))
		if err != nil {
			t.Errorf("snapshot unreadable inside the window: %v", err)
			return
		}
		probe := &Store{users: map[string]*User{}, byToken: map[string]string{},
			proposals: map[string]*Proposal{}}
		if _, err := probe.loadSnapshot(dir); err != nil {
			t.Errorf("snapshot not parseable inside the window (%d bytes): %v", len(raw), err)
			return
		}
		if probe.UserCount() != 2 || probe.ProposalCount() != 1 {
			t.Errorf("snapshot incomplete inside the window: users=%d proposals=%d",
				probe.UserCount(), probe.ProposalCount())
		}
		seen = append(seen, "window")
	})
	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("compact hook did not run: the window was never observed (%d)", len(seen))
	}
}

// --- kill -9 during compaction (tasks 6.3 verification) --------------------

// The environment contract for the re-entrant child process.
const (
	envCompactCrash = "TEAMDATING_COMPACT_CRASH_DIR"
	envCompactOrder = "TEAMDATING_COMPACT_ORDER" // "safe" | "reversed"
)

// TestMain lets this test binary re-enter itself as the crashing child.
//
// ★ A real SIGKILL, not a panic. The two renames are filesystem operations;
// a panic unwinds the Go stack but the kernel has already applied whatever
// renames ran, so a panic happens to reproduce the same on-disk state here.
// The reason to use SIGKILL anyway is that it is what tasks 6.3 specifies and
// what an operator actually does — a test that models the interruption
// differently from the real one is measuring its own model.
func TestMain(m *testing.M) {
	if dir := os.Getenv(envCompactCrash); dir != "" {
		runCompactCrashChild(dir, os.Getenv(envCompactOrder) == "reversed")
		return // unreachable: the child kills itself inside the window
	}
	os.Exit(m.Run())
}

func runCompactCrashChild(dir string, reversed bool) {
	s, err := New(dir)
	if err != nil {
		os.Stderr.WriteString("child: New: " + err.Error() + "\n")
		os.Exit(2)
	}
	s.SetUnsafeCompactOrderForControlGroup(reversed)
	s.SetCompactHookForControlGroup(func() {
		// SIGKILL to self, exactly in the window between the two renames.
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
		// Unreachable, but block so a delayed signal cannot let the second
		// rename slip through and turn the crash into a clean compaction.
		select {}
	})
	_ = s.Compact()
	os.Stderr.WriteString("child: survived the compact window (hook never fired)\n")
	os.Exit(3)
}

// crashDuringCompact seeds dir, then runs a child that SIGKILLs itself between
// the two renames. It returns the child's wait status for inspection.
func crashDuringCompact(t *testing.T, dir string, reversed bool) {
	t.Helper()

	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedForCompact(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMain")
	cmd.Env = append(os.Environ(), envCompactCrash+"="+dir)
	if reversed {
		cmd.Env = append(cmd.Env, envCompactOrder+"=reversed")
	} else {
		cmd.Env = append(cmd.Env, envCompactOrder+"=safe")
	}
	out, err := cmd.CombinedOutput()

	// The child MUST die by signal. Any other exit means the window was never
	// entered, and then the whole test proves nothing.
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("child did not fail as expected (err=%v, output=%q)", err, out)
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("child exited without SIGKILL (status=%v, output=%q): the crash "+
			"window was never reached, so this test measured nothing", ee, out)
	}
}

// TestCompactSurvivesKillBetweenRenames is the 6.3 acceptance test: a kill -9
// landing between the two renames must leave a state that reopens COMPLETE —
// old snapshot or new snapshot, never half.
func TestCompactSurvivesKillBetweenRenames(t *testing.T) {
	dir := tempDir(t)
	crashDuringCompact(t, dir, false)
	assertSeeded(t, newStore(t, dir), "reopened after kill -9 mid-compaction")
}

// TestCompactCrashControlGroup proves the test above can fail.
//
// ★ Without this, "survived the crash" and "the crash never threatened
// anything" produce the same green. With the renames reversed, the same
// injection loses every record — so the acceptance test is measuring the
// ordering and not merely the absence of trouble.
func TestCompactCrashControlGroup(t *testing.T) {
	dir := tempDir(t)
	crashDuringCompact(t, dir, true)

	s, err := New(dir)
	if err != nil {
		// A load error is also a failure of the reversed order — but it must
		// not be silently accepted as "the control group worked", because a
		// bug in loadSnapshot would look identical.
		t.Fatalf("reversed order left an unloadable directory: %v", err)
	}
	defer s.Close()

	if s.UserCount() == 2 && s.ProposalCount() == 1 {
		t.Fatalf("CONTROL GROUP FAILED: the reversed rename order lost nothing "+
			"(users=%d proposals=%d). The acceptance test therefore has no "+
			"discriminating power and its green means nothing.",
			s.UserCount(), s.ProposalCount())
	}
	t.Logf("control group has discriminating power: reversed order left "+
		"users=%d proposals=%d (seeded 2 and 1)", s.UserCount(), s.ProposalCount())
}

// TestCorruptSnapshotIsFatal: a damaged snapshot must refuse to open rather
// than falling back to replaying whatever the log still holds. That fallback
// would look like a graceful recovery and would serve a state missing every
// record the compaction already removed from the log (天條 11).
func TestCorruptSnapshotIsFatal(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	seedForCompact(t, s)
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if err := os.WriteFile(filepath.Join(dir, snapshotName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil {
		t.Fatal("New accepted a corrupt snapshot instead of failing loudly")
	}

	// Same for an unsupported version: a future format must not be read with
	// today's field meanings.
	if err := os.WriteFile(filepath.Join(dir, snapshotName),
		[]byte(`{"version":99,"users":{},"proposals":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(dir)
	if err == nil {
		t.Fatal("New accepted an unsupported snapshot version")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("version error does not say which field: %v", err)
	}
}
