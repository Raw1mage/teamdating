package store

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// tempDir returns a LOCAL scratch directory. The repository lives on an rclone
// FUSE mount where fsync semantics differ from local ext4 — a durability test
// run there measures rclone's behaviour, not this package's.
func tempDir(t *testing.T) string {
	t.Helper()
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	dir, err := os.MkdirTemp(base, "teamdating-test-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New(%s): %v", dir, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// --- 2.1 concurrency -------------------------------------------------------

// TestConcurrentMutationAndRead is the race-detector target. Under -race it
// fails if any map is touched outside the lock.
//
// CONTROL GROUP for V4: removing the s.mu.Lock() in commit() (or the RLock in
// any accessor) makes this test report a data race. Verified by doing exactly
// that — see the [BRNS-DONE] report.
func TestConcurrentMutationAndRead(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "h1", "Ann"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("P1", "U1", "Trip", ""); err != nil {
		t.Fatal(err)
	}

	var wg, writers sync.WaitGroup
	const n = 40

	// Writers: distinct options and votes, plus renames of one shared user.
	for i := 0; i < n; i++ {
		wg.Add(1)
		writers.Add(1)
		go func(i int) {
			defer wg.Done()
			defer writers.Done()
			uid := "U" + itoa(i+2)
			hash := "h" + itoa(i+2)
			if err := s.CreateUser(uid, hash, "N"+itoa(i)); err != nil {
				t.Error(err)
				return
			}
			// Ranges must be distinct: a proposal rejects duplicate ranges, so a
			// colliding generator would make this test fail for a reason that has
			// nothing to do with concurrency. month/day split keeps i in 0..39 unique.
			oid := "O" + itoa(i)
			day := pad2(i%28 + 1)
			month := pad2(i/28 + 1)
			if err := s.CreateOption("P1", oid, uid,
				"2026-"+month+"-"+day, "2026-"+month+"-"+day, ""); err != nil {
				t.Error(err)
				return
			}
			if err := s.SetVote("P1", oid, uid, WeightYes); err != nil {
				t.Error(err)
			}
		}(i)
	}
	// Readers hammer the same structures for the WHOLE duration of the writer
	// phase, not once each.
	//
	// ★ The loop is what makes this a guard rather than a lottery. The race
	// detector only reports accesses that actually interleave, so a single pass
	// per reader leaves a narrow window: measured at ~20% detection (1 catch in
	// 5 runs) when a lock is removed. A reader that keeps reading until the
	// writers are done overlaps every writer instead of at most one, which is
	// the difference between "we sampled for a race" and "we looked for one".
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				_, _ = s.Proposal("P1")
				_ = s.Users()
				_ = s.VoteCount("P1", "O1")
				_ = s.OthersVoteCount("P1", "O1")
				_ = s.ProposalsForUser("U1")
				_, _ = s.UIDByTokenHash("h1")
			}
		}()
	}

	writers.Wait()
	close(done)
	wg.Wait()

	p, ok := s.Proposal("P1")
	if !ok {
		t.Fatal("proposal vanished")
	}
	if len(p.Options) != n {
		t.Errorf("options = %d, want %d", len(p.Options), n)
	}
	if len(p.Votes) != n {
		t.Errorf("votes = %d, want %d", len(p.Votes), n)
	}
}

// TestAccessorsReturnCopies proves the "no handler touches a bare map" claim.
// If an accessor leaked a live pointer, mutating the result would corrupt the
// store and this test would notice.
func TestAccessorsReturnCopies(t *testing.T) {
	s := newStore(t, tempDir(t))
	mustSeed(t, s)

	p, _ := s.Proposal("P1")
	p.Title = "hacked"
	delete(p.Options, "O1")
	p.Votes["O1__U9"] = &Vote{OptionID: "O1", UID: "U9"}

	again, _ := s.Proposal("P1")
	if again.Title != "Trip" {
		t.Errorf("Title mutated through copy: %q", again.Title)
	}
	if _, ok := again.Options["O1"]; !ok {
		t.Error("option deleted through copy")
	}
	if _, ok := again.Votes["O1__U9"]; ok {
		t.Error("vote injected through copy")
	}
}

// --- 2.2 write-ahead durability -------------------------------------------

// TestWriteAheadSurvivesCrash simulates kill -9 by discarding the in-memory
// Store without a clean Close and replaying the log from disk.
//
// This is the correct-order half of TV-12; the broken-order half below is what
// gives it discriminating power.
func TestWriteAheadSurvivesCrash(t *testing.T) {
	dir := tempDir(t)
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustSeed(t, s)
	// No Close(): the process "dies" here. The data must already be on disk
	// because every mutator appended + fsynced BEFORE returning success.

	revived := newStore(t, dir)
	p, ok := revived.Proposal("P1")
	if !ok {
		t.Fatal("proposal lost across crash")
	}
	if len(p.Options) != 1 || len(p.Votes) != 1 {
		t.Fatalf("options=%d votes=%d, want 1/1", len(p.Options), len(p.Votes))
	}
	s.Close()
}

// TestCommitIsDurableBeforeVisible asserts the DD-14 ordering contract head-on.
//
// ★ This is the guard that was missing. The previous version of this file only
// checked "does truncating the log lose data", which is true under BOTH orders
// — so reversing commit()'s two steps left the whole suite green. The contract
// is not about truncation; it is about which of the two steps happens first,
// and the only place that is observable is the window between them.
//
// crashHook runs exactly in that window, so the assertion is direct: at that
// instant the event must ALREADY be on disk and must NOT YET be visible in
// memory. Reverse the order and both halves flip, so this fails immediately and
// deterministically — no crash simulation, no timing, no sampling.
func TestCommitIsDurableBeforeVisible(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	logPath := filepath.Join(dir, "events.jsonl")

	if err := s.CreateUser("U1", "h1", "Ann"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("P1", "U1", "Trip", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOption("P1", "O1", "U1", "2026-12-28", "2027-01-03", ""); err != nil {
		t.Fatal(err)
	}

	// Control group, run BEFORE arming: prove each probe can report the other
	// answer. Without this, "on disk" and "not in memory" could both be
	// hard-wired truths of a broken probe rather than observations.
	if logContains(t, logPath, "vote_set") {
		t.Fatal("control group failed: log already contains vote_set before any vote")
	}
	if s.HasVote("P1", "O1", "U1") {
		t.Fatal("control group failed: memory already shows the vote before any vote")
	}

	var (
		sawHook      bool
		onDiskAtHook bool
		inMemAtHook  bool
	)
	s.SetCrashHookForControlGroup(func() {
		sawHook = true
		onDiskAtHook = logContains(t, logPath, "vote_set")
		inMemAtHook = s.HasVote("P1", "O1", "U1")
	})
	if err := s.SetVote("P1", "O1", "U1", WeightYes); err != nil {
		t.Fatal(err)
	}
	s.SetCrashHookForControlGroup(nil)

	if !sawHook {
		t.Fatal("the hook never ran — this test asserts nothing (an absent probe " +
			"and a passing one must not share an output)")
	}
	if !onDiskAtHook {
		t.Error("DD-14 violated: memory was mutated before the event was durable. " +
			"A crash here loses a write the caller was already told succeeded.")
	}
	if inMemAtHook {
		t.Error("DD-14 violated: the vote was visible in memory before its event " +
			"reached the log — the two commit steps are in the wrong order.")
	}

	// And the post-condition still holds: after commit returns, both are true.
	if !logContains(t, logPath, "vote_set") || !s.HasVote("P1", "O1", "U1") {
		t.Error("after commit returned, the vote must be both durable and visible")
	}
}

// TestWriteAheadControlGroup is the control for V3/TV-12.
//
// ★ Both arms take the SAME interruption: a panic injected by crashHook at the
// exact point between commit()'s two steps. That symmetry is the whole point —
// the earlier version truncated the log on the broken arm only and let the good
// arm shut down cleanly, so it compared two different events and could not see
// the ordering at all.
//
// Under the correct order the interrupted write is already fsynced, so replay
// restores it. Under the reversed order it never reached the disk, so it is
// gone — which is the user-visible harm: "it said saved, and after the restart
// it wasn't there."
func TestWriteAheadControlGroup(t *testing.T) {
	goodHasVote := crashDuringVote(t, false)
	brokenHasVote := crashDuringVote(t, true)

	if !goodHasVote {
		t.Error("write-ahead order lost data — the ordering is not being honoured")
	}
	if brokenHasVote {
		t.Error("broken order kept the data — this test does not cut between the " +
			"append and the memory write, so it has no discriminating power (TV-12)")
	}
	if goodHasVote == brokenHasVote {
		t.Fatalf("both orders behaved identically (correct=%v broken=%v): "+
			"the durability test is measuring nothing", goodHasVote, brokenHasVote)
	}
}

// crashDuringVote seeds a store, interrupts the vote commit mid-window, then
// replays from disk and reports whether the vote survived.
//
// The interruption is a panic rather than a real SIGKILL so it can run inside
// `go test`: what matters for the ordering contract is that control leaves
// commit() between the two steps without completing the second one, and a
// panic does exactly that. (The deferred writeMu.Unlock still runs during
// unwinding, so the abandoned store stays usable for nothing but replay — we
// open a fresh one anyway, which is what a restart does.)
func crashDuringVote(t *testing.T, brokenOrder bool) (voteSurvived bool) {
	t.Helper()
	dir := tempDir(t)

	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.SetUnsafeOrderForControlGroup(brokenOrder)
	if err := s.CreateUser("U1", "h1", "Ann"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("P1", "U1", "Trip", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOption("P1", "O1", "U1", "2026-12-28", "2027-01-03", ""); err != nil {
		t.Fatal(err)
	}

	const crashMarker = "crash: interrupted inside the commit window"
	crashed := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				if msg, ok := r.(string); ok && msg == crashMarker {
					crashed = true
					return
				}
				panic(r) // not ours — do not swallow a real bug
			}
		}()
		// Armed only for the vote: the three seeding writes above had to
		// complete normally to set the scene.
		s.SetCrashHookForControlGroup(func() { panic(crashMarker) })
		_ = s.SetVote("P1", "O1", "U1", WeightYes)
	}()
	if !crashed {
		t.Fatalf("brokenOrder=%v: the interruption never fired, so this arm "+
			"measured nothing", brokenOrder)
	}
	// No Close(): the process is notionally dead. Whatever is on disk is all
	// that a restart gets.

	revived := newStore(t, dir)
	p, ok := revived.Proposal("P1")
	if !ok {
		t.Fatalf("brokenOrder=%v: the proposal itself was lost; this arm is not "+
			"isolating the vote", brokenOrder)
	}
	if len(p.Options) != 1 {
		t.Fatalf("brokenOrder=%v: options=%d want 1 — the seeding writes must "+
			"survive under both orders, or the arms are not comparable",
			brokenOrder, len(p.Options))
	}
	return len(p.Votes) == 1
}

// logContains reports whether the event log currently holds a substring. It
// reads from disk on every call — the point is to observe durability, so a
// cached answer would defeat it.
func logContains(t *testing.T, path, needle string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Contains(string(b), needle)
}

// --- 2.3 replay -----------------------------------------------------------

// TestReplayRejectsUnknownType covers TV-17 including its control group: the
// same file WITHOUT the bad line must replay cleanly, otherwise a failure
// cannot be attributed to that line rather than to replay being broken.
func TestReplayRejectsUnknownType(t *testing.T) {
	dir := tempDir(t)
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustSeed(t, s)
	s.Close()

	path := filepath.Join(dir, "events.jsonl")
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Control group: unmodified file replays clean.
	if _, err := New(dir); err != nil {
		t.Fatalf("control group failed: pristine log must replay, got %v", err)
	}

	// Insert an unknown type in the middle.
	lines := strings.Split(strings.TrimRight(string(orig), "\n"), "\n")
	mid := len(lines) / 2
	bad := append([]string{}, lines[:mid]...)
	bad = append(bad, `{"t":"2026-08-04T00:00:00Z","type":"unknown_thing","d":{}}`)
	bad = append(bad, lines[mid:]...)
	if err := os.WriteFile(path, []byte(strings.Join(bad, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = New(dir)
	if err == nil {
		t.Fatal("replay accepted an unknown event type; skipping it would make " +
			"'a record was dropped' and 'the file never had one' identical")
	}
	wantLine := ":" + itoa(mid+1) + ":"
	if !strings.Contains(err.Error(), wantLine) {
		t.Errorf("error %q does not name line %d", err, mid+1)
	}
	if !strings.Contains(err.Error(), "unknown_thing") {
		t.Errorf("error %q does not name the offending type", err)
	}
}

func TestReplayRejectsMalformedJSON(t *testing.T) {
	dir := tempDir(t)
	path := filepath.Join(dir, "events.jsonl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir); err == nil {
		t.Fatal("replay accepted malformed JSON")
	} else if !strings.Contains(err.Error(), ":1:") {
		t.Errorf("error %q does not name line 1", err)
	}
}

func TestReplayRoundTripsEveryEventType(t *testing.T) {
	dir := tempDir(t)
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("U1", "h1", "Ann"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("U2", "h2", "Bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameUser("U2", "Bobby"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("P1", "U1", "Trip", "d"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProposal("P1", "Trip2", "d2"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOption("P1", "O1", "U2", "2026-12-28", "2027-01-03", "n"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOption("P1", "O2", "U1", "2026-11-01", "2026-11-02", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateOption("P1", "O1", "2026-12-29", "2027-01-04", "n2"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVote("P1", "O1", "U1", WeightYes); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVote("P1", "O2", "U2", WeightYes); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteVote("P1", "O2", "U2"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteOption("P1", "O2"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("P2", "U2", "Gone", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProposal("P2"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	r := newStore(t, dir)
	if u, ok := r.User("U2"); !ok || u.Name != "Bobby" {
		t.Errorf("rename not replayed: %+v", u)
	}
	p, ok := r.Proposal("P1")
	if !ok {
		t.Fatal("P1 missing")
	}
	if p.Title != "Trip2" || p.Desc != "d2" {
		t.Errorf("proposal update not replayed: %+v", p)
	}
	if len(p.Options) != 1 {
		t.Errorf("options=%d want 1 (O2 was deleted)", len(p.Options))
	}
	if o := p.Options["O1"]; o == nil || o.StartDate != "2026-12-29" || o.Note != "n2" {
		t.Errorf("option update not replayed: %+v", o)
	}
	if len(p.Votes) != 1 {
		t.Errorf("votes=%d want 1", len(p.Votes))
	}
	if _, ok := r.Proposal("P2"); ok {
		t.Error("deleted proposal came back")
	}
}

// TestDeleteProposalCascades checks that no orphan survives a restart (4.1's
// invariant, verified here at the data layer).
func TestDeleteProposalCascades(t *testing.T) {
	dir := tempDir(t)
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	mustSeed(t, s)
	if err := s.DeleteProposal("P1"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	r := newStore(t, dir)
	if _, ok := r.Proposal("P1"); ok {
		t.Fatal("proposal survived delete")
	}
	if r.ProposalCount() != 0 {
		t.Errorf("ProposalCount=%d want 0", r.ProposalCount())
	}
}

// --- 2.4 deterministic vote key -------------------------------------------

// TestVoteIsIdempotent is TV-1. Its control group is TestVoteKeyIsDeterministic
// below: an append-to-slice implementation would produce 5 here, and the key
// helper is what makes that impossible.
func TestVoteIsIdempotent(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	mustSeed(t, s)

	for i := 0; i < 5; i++ {
		if err := s.SetVote("P1", "O1", "U1", WeightYes); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.VoteCount("P1", "O1"); n != 1 {
		t.Fatalf("VoteCount=%d want 1 — five POSTs must leave one vote (TV-1)", n)
	}
	p, _ := s.Proposal("P1")
	if _, ok := p.Votes["O1__U1"]; !ok {
		t.Errorf("expected key O1__U1, got keys %v", keysOf(p.Votes))
	}
	if len(p.Votes) != 1 {
		t.Errorf("len(Votes)=%d want 1", len(p.Votes))
	}

	// And it stays one after replay — five events, one cell.
	s.Close()
	r := newStore(t, dir)
	if n := r.VoteCount("P1", "O1"); n != 1 {
		t.Errorf("after replay VoteCount=%d want 1", n)
	}
}

func TestVoteKeyIsDeterministic(t *testing.T) {
	if got := VoteKey("O1", "U1"); got != "O1__U1" {
		t.Errorf("VoteKey = %q want O1__U1", got)
	}
	// Control-group reasoning made executable: the same inputs must always
	// collapse to the same key, which is what makes assignment idempotent.
	if VoteKey("O1", "U1") != VoteKey("O1", "U1") {
		t.Error("VoteKey is not deterministic")
	}
	if VoteKey("O1", "U1") == VoteKey("O1", "U2") {
		t.Error("different voters collapsed to one key")
	}
}

// TestOthersVoteCountExcludesAuthor is TV-2/TV-3's data-layer half: an
// implementation using the TOTAL count cannot distinguish these two states.
func TestOthersVoteCountExcludesAuthor(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("P1owner", "hp", "Owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("U2", "h2", "Author"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("U3", "h3", "Other"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("P1", "P1owner", "Trip", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOption("P1", "O1", "U2", "2026-03-01", "2026-03-05", ""); err != nil {
		t.Fatal(err)
	}

	// TV-2: the author votes for their own option.
	if err := s.SetVote("P1", "O1", "U2", WeightYes); err != nil {
		t.Fatal(err)
	}
	if got := s.OthersVoteCount("P1", "O1"); got != 0 {
		t.Errorf("TV-2: OthersVoteCount=%d want 0 (author's own vote must not count)", got)
	}
	if got := s.VoteCount("P1", "O1"); got != 1 {
		t.Errorf("TV-2: VoteCount=%d want 1", got)
	}

	// TV-3: the first foreign vote arrives.
	if err := s.SetVote("P1", "O1", "U3", WeightYes); err != nil {
		t.Fatal(err)
	}
	if got := s.OthersVoteCount("P1", "O1"); got != 1 {
		t.Errorf("TV-3: OthersVoteCount=%d want 1", got)
	}
	if got := s.VoteCount("P1", "O1"); got != 2 {
		t.Errorf("TV-3: VoteCount=%d want 2", got)
	}
}

func TestDeleteOptionRemovesItsVotes(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	mustSeed(t, s)
	if err := s.DeleteOption("P1", "O1"); err != nil {
		t.Fatal(err)
	}
	if n := s.VoteCount("P1", "O1"); n != 0 {
		t.Errorf("VoteCount=%d want 0 after option delete", n)
	}
	s.Close()
	r := newStore(t, dir)
	p, _ := r.Proposal("P1")
	if len(p.Votes) != 0 {
		t.Errorf("orphan votes survived replay: %v", keysOf(p.Votes))
	}
}

// TestNoGlobalEnumeration guards R8 structurally: ProposalsForUser is
// subject-scoped and must never return someone else's proposal.
func TestNoGlobalEnumeration(t *testing.T) {
	s := newStore(t, tempDir(t))
	for _, u := range []struct{ uid, h, n string }{{"UA", "ha", "A"}, {"UB", "hb", "B"}} {
		if err := s.CreateUser(u.uid, u.h, u.n); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateProposal("PA", "UA", "A trip", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("PB", "UB", "B trip", ""); err != nil {
		t.Fatal(err)
	}

	mine := s.ProposalsForUser("UA")
	if len(mine) != 1 || mine[0].PID != "PA" {
		t.Fatalf("ProposalsForUser(UA) = %v, want exactly [PA]", pidsOf(mine))
	}
	// Control group (TV-6): the known-pid lookup must still work, otherwise an
	// empty result could mean "the endpoint is broken" rather than "correctly
	// filtered".
	if _, ok := s.Proposal("PA"); !ok {
		t.Error("control group failed: direct lookup of a known pid must succeed")
	}
	if _, ok := s.Proposal("PB"); !ok {
		t.Error("control group failed: PB must exist even though UA cannot list it")
	}
}

// --- helpers ---------------------------------------------------------------

func mustSeed(t *testing.T, s *Store) {
	t.Helper()
	if err := s.CreateUser("U1", "h1", "Ann"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("P1", "U1", "Trip", "desc"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOption("P1", "O1", "U1", "2026-12-28", "2027-01-03", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVote("P1", "O1", "U1", WeightYes); err != nil {
		t.Fatal(err)
	}
}

func keysOf(m map[string]*Vote) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func pidsOf(ps []Proposal) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.PID)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func pad2(n int) string {
	s := itoa(n)
	if len(s) < 2 {
		return "0" + s
	}
	return s
}

// TestDecisionSurvivesReplay pins that the freeze is DURABLE (6.7 + DD-14).
//
// ★ An in-memory-only decision is the defect this guards: everything looks
// correct until a restart, at which point a proposal everyone agreed was
// settled silently reopens for voting — and nothing reports an error.
func TestDecisionSurvivesReplay(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	if err := s.CreateUser("U1", "h1", "Ann"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("P1", "U1", "T", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOption("P1", "O1", "U1", "2026-03-01", "2026-03-05", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDecision("P1", "O1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := newStore(t, dir)
	got, ok := s2.DecidedOption("P1")
	if !ok {
		t.Fatal("proposal missing after replay")
	}
	if got != "O1" {
		t.Fatalf("decision after replay = %q, want O1 (the freeze did not survive restart)", got)
	}

	// Control half: the release must survive too. Without it, an apply branch
	// that hard-coded the decided id would pass the assertion above.
	if err := s2.SetDecision("P1", ""); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3 := newStore(t, dir)
	if got, _ := s3.DecidedOption("P1"); got != "" {
		t.Fatalf("release did not survive replay: decision = %q, want empty", got)
	}
}

// TestDecisionRejectsUnknownOption: freezing on an option that does not exist
// would produce a proposal frozen on nothing — unreachable by the UI and
// releasable only by an operator.
func TestDecisionRejectsUnknownOption(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "h1", "Ann"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("P1", "U1", "T", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDecision("P1", "GHOST"); err == nil {
		t.Fatal("decided on a nonexistent option")
	}
	// Control: the empty oid (release) must NOT be rejected by the same check.
	if err := s.SetDecision("P1", ""); err != nil {
		t.Fatalf("release rejected as an unknown option: %v", err)
	}
}

// --- 6.8 三態表態（DD-6）---------------------------------------------------

func stanceEnv(t *testing.T) *Store {
	t.Helper()
	s := newStore(t, tempDir(t))
	for _, u := range []string{"U1", "U2", "U3", "U4"} {
		if err := s.CreateUser(u, "h"+u, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateProposal("P1", "U1", "T", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOption("P1", "O1", "U1", "2026-03-01", "2026-03-05", ""); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestTallySeparatesStances is the core of DD-6: the three counts must NOT be
// derivable from the score.
//
// ★ The two rows below score identically on purpose. An implementation that
// reported only Score could not tell "one 可以, nobody else" from "one 可以 plus
// three 不行": both score 1, yet the proposer must see whether the option is
// merely unnoticed or actively rejected. The No count carries that distinction.
func TestTallySeparatesStances(t *testing.T) {
	s := stanceEnv(t)
	if err := s.SetVote("P1", "O1", "U1", WeightYes); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVote("P1", "O1", "U2", WeightNo); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVote("P1", "O1", "U3", WeightNo); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVote("P1", "O1", "U4", WeightNo); err != nil {
		t.Fatal(err)
	}
	got := s.Tally("P1", "O1")
	want := StanceTally{Yes: 1, No: 3, Score: 1.0}
	if got != want {
		t.Fatalf("tally = %+v, want %+v", got, want)
	}
	// The discriminating assertion: a different mix with the SAME score must
	// produce different counts. One lone 可以 scores 1 too, but has No=0.
	s2 := stanceEnv(t)
	if err := s2.SetVote("P1", "O1", "U1", WeightYes); err != nil {
		t.Fatal(err)
	}
	g2 := s2.Tally("P1", "O1")
	if g2.Score != got.Score {
		t.Fatalf("setup error: scores differ (%v vs %v); this case cannot test what it claims",
			g2.Score, got.Score)
	}
	if g2.No == got.No {
		t.Fatal("two different stance mixes with the same score produced the same No count — " +
			"the tally is folding rejection into the score")
	}
}

// TestZeroWeightVoteIsStillAResponse: 不行 must be distinguishable from silence.
func TestZeroWeightVoteIsStillAResponse(t *testing.T) {
	s := stanceEnv(t)
	silent := s.Tally("P1", "O1")
	if silent.No != 0 {
		t.Fatalf("nobody voted yet but No = %d", silent.No)
	}
	if err := s.SetVote("P1", "O1", "U2", WeightNo); err != nil {
		t.Fatal(err)
	}
	after := s.Tally("P1", "O1")
	if after.No != 1 {
		t.Fatalf("a 不行 vote left No = %d, want 1 — rejection is being dropped and "+
			"reads identically to never having responded", after.No)
	}
	if after.Score != silent.Score {
		t.Fatalf("a 不行 vote changed the score (%v -> %v); it must not", silent.Score, after.Score)
	}
	// And the row genuinely exists.
	if !s.HasVote("P1", "O1", "U2") {
		t.Fatal("a 不行 vote did not create a vote row")
	}
}

// TestSetVoteRejectsOutOfSetWeight: a forged weight is refused, not clamped.
func TestSetVoteRejectsOutOfSetWeight(t *testing.T) {
	s := stanceEnv(t)
	for _, w := range []float64{0.9, 2, -1, 0.51} {
		if err := s.SetVote("P1", "O1", "U2", w); err == nil {
			t.Fatalf("weight %v accepted", w)
		}
	}
	// Control: both legal stances are accepted, so the rejections above
	// are not simply "SetVote refuses everything".
	for _, w := range []float64{WeightYes, WeightNo} {
		if err := s.SetVote("P1", "O1", "U2", w); err != nil {
			t.Fatalf("legal weight %v rejected: %v", w, err)
		}
	}
}

// TestWeightSurvivesReplay: the stance must be durable, not just in memory.
func TestWeightSurvivesReplay(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	for _, u := range []string{"U1", "U2"} {
		if err := s.CreateUser(u, "h"+u, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateProposal("P1", "U1", "T", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOption("P1", "O1", "U1", "2026-03-01", "2026-03-05", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVote("P1", "O1", "U2", WeightNo); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := newStore(t, dir)
	got := s2.Tally("P1", "O1")
	if got.No != 1 || got.Score != 0 {
		t.Fatalf("after replay tally = %+v, want No=1 Score=0 — the 不行 vote was "+
			"lost and reads identically to never having responded", got)
	}
}

// --- 6.9 pid 重新產生（風險 B）----------------------------------------------

// TestRepIDRevokesOldPID is the whole point: after rotation the leaked id must
// be GONE, not aliased.
//
// ★ An implementation that inserted the new key while leaving the old one in
// place would pass any test that only checks "the new pid works" — and would
// revoke nothing at all while reporting success.
func TestRepIDRevokesOldPID(t *testing.T) {
	s := stanceEnv(t)
	if err := s.SetVote("P1", "O1", "U2", WeightYes); err != nil {
		t.Fatal(err)
	}
	if err := s.RepID("P1", "P2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Proposal("P1"); ok {
		t.Fatal("the old pid still resolves — the leaked URL keeps working and the " +
			"revocation reported success while revoking nothing")
	}
	p, ok := s.Proposal("P2")
	if !ok {
		t.Fatal("the new pid does not resolve — rotation destroyed the proposal")
	}
	// Contents must travel with it, including the embedded PID field.
	if p.PID != "P2" {
		t.Fatalf("proposal.PID = %q after rotation, want P2 — the record still "+
			"reports its old id, so anything reading it round-trips to a dead key", p.PID)
	}
	if len(p.Options) != 1 {
		t.Fatalf("options after rotation = %d, want 1", len(p.Options))
	}
	if tl := s.Tally("P2", "O1"); tl.Yes != 1 {
		t.Fatalf("votes did not travel: tally = %+v", tl)
	}
}

// TestRepIDSurvivesReplay: the rotation must be durable. Otherwise a restart
// silently resurrects the leaked pid.
func TestRepIDSurvivesReplay(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	if err := s.CreateUser("U1", "h1", "Ann"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProposal("P1", "U1", "T", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOption("P1", "O1", "U1", "2026-03-01", "2026-03-05", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RepID("P1", "P2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := newStore(t, dir)
	if _, ok := s2.Proposal("P1"); ok {
		t.Fatal("the revoked pid came back after a restart — the rotation was " +
			"in-memory only and the leaked URL works again")
	}
	p, ok := s2.Proposal("P2")
	if !ok {
		t.Fatal("the rotated proposal is missing after replay")
	}
	if p.PID != "P2" || len(p.Options) != 1 {
		t.Fatalf("after replay: PID=%q options=%d, want P2/1", p.PID, len(p.Options))
	}
}

// TestRepIDRejectsCollisionAndNoop.
func TestRepIDRejectsCollisionAndNoop(t *testing.T) {
	s := stanceEnv(t)
	if err := s.CreateProposal("PX", "U1", "Other", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RepID("P1", "PX"); err == nil {
		t.Fatal("rotated onto an existing pid — one proposal would have overwritten another")
	}
	// The victim must be untouched by the refused attempt.
	if _, ok := s.Proposal("PX"); !ok {
		t.Fatal("the collision target was destroyed by a refused rotation")
	}
	if err := s.RepID("P1", "P1"); err == nil {
		t.Fatal("rotating to the same pid was accepted — it revokes nothing while " +
			"reporting success")
	}
	if err := s.RepID("NOPE", "P9"); err == nil {
		t.Fatal("rotated an unknown proposal")
	}
	// Control: a legitimate rotation still succeeds.
	if err := s.RepID("P1", "P2"); err != nil {
		t.Fatalf("legitimate rotation rejected: %v", err)
	}
}
