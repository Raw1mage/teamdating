package store

import (
	"strings"
	"testing"
	"time"
)

// Behaviour vectors for the multi-token model.

// addTokens is a fatal-on-error helper.
func addTokens(t *testing.T, s *Store, uid string, hashes ...string) {
	t.Helper()
	for _, h := range hashes {
		if err := s.AddToken(uid, h); err != nil {
			t.Fatalf("AddToken(%s,%s): %v", uid, h, err)
		}
	}
}

// setLastSeen forces a token's LastSeenAt through the real event path, so the
// value is in the LOG and not merely in memory. Using a direct field poke here
// would make every restart assertion vacuous.
func setLastSeen(t *testing.T, s *Store, uid, hash, stamp string) {
	t.Helper()
	err := s.commit(EvUserTouchToken,
		UserTouchTokenData{UID: uid, Hash: hash, SeenAt: stamp},
		nil,
		func() {
			if ref, ok := s.users[uid].FindToken(hash); ok {
				ref.LastSeenAt = stamp
			}
		})
	if err != nil {
		t.Fatalf("force LastSeenAt(%s,%s): %v", uid, hash, err)
	}
}

func hashesOf(u User) []string {
	out := make([]string, 0, len(u.Tokens))
	for _, tr := range u.Tokens {
		out = append(out, tr.Hash)
	}
	return out
}

// TestBothTokensResolveToOneIdentity is TV-1: two tokens, one identity.
func TestBothTokensResolveToOneIdentity(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "hA", "小明"); err != nil {
		t.Fatal(err)
	}
	addTokens(t, s, "U1", "hB")

	for _, h := range []string{"hA", "hB"} {
		uid, ok := s.UIDByTokenHash(h)
		if !ok || uid != "U1" {
			t.Errorf("%s resolved to (%q,%v), want (\"U1\",true)", h, uid, ok)
		}
	}
}

// TestFirstTokenSurvivesAddingASecond is TV-2, the control-group vector for
// AddToken.
//
// ★ TV-1 cannot catch the defect this catches. Under a REPLACING AddToken
// (u.Tokens = []TokenRef{new}) the new device authenticates perfectly, so
// TV-1's hB half still passes; the only casualty is the token nobody is
// looking at during the test — which in production is the user's other phone,
// logged out silently. This vector looks specifically at the OLD key.
func TestFirstTokenSurvivesAddingASecond(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "hA", "小明"); err != nil {
		t.Fatal(err)
	}
	addTokens(t, s, "U1", "hB")

	uid, ok := s.UIDByTokenHash("hA")
	if !ok || uid != "U1" {
		t.Fatalf("the FIRST token no longer resolves after adding a second: "+
			"got (%q,%v). AddToken replaced instead of appending — the user's "+
			"other device is now logged out with no error anywhere", uid, ok)
	}
	u, _ := s.User("U1")
	if len(u.Tokens) != 2 {
		t.Errorf("token count = %d (%v), want 2", len(u.Tokens), hashesOf(u))
	}
}

// TestSixthTokenEvictsAndKeepsTheCap is TV-5.
func TestSixthTokenEvictsAndKeepsTheCap(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "hOld", "小明"); err != nil {
		t.Fatal(err)
	}
	addTokens(t, s, "U1", "h2", "h3", "h4", "h5")
	// hOld is the least recently seen: created first, never touched since.
	setLastSeen(t, s, "U1", "hOld", "2020-01-01T00:00:00Z")

	addTokens(t, s, "U1", "hNew")

	u, _ := s.User("U1")
	if len(u.Tokens) != MaxTokensPerUser {
		t.Errorf("token count = %d (%v), want %d",
			len(u.Tokens), hashesOf(u), MaxTokensPerUser)
	}
	if _, held := u.FindToken("hNew"); !held {
		t.Error("hNew was not added")
	}
	if _, held := u.FindToken("hOld"); held {
		t.Error("hOld survived past the cap")
	}
	// An evicted token must stop authenticating — an empty identity, not an
	// error. A stale byToken entry would keep the revoked device working while
	// the roster says it is gone.
	if uid, ok := s.UIDByTokenHash("hOld"); ok {
		t.Errorf("byToken still resolves the evicted hOld to %q", uid)
	}
}

// TestEvictionOrdersByLastSeenNotAdded is TV-6, the control-group vector for
// the LRU ordering.
//
// ★ TV-5 passes under BOTH orderings, because it adds tokens in the same order
// it uses them — which is what makes the mistake invisible. Here the two
// orderings disagree by construction: hA is the OLDEST by AddedAt but the
// newest by LastSeenAt, and hStale is the reverse. An AddedAt-keyed
// implementation evicts hA (the phone in the user's hand); a LastSeenAt-keyed
// one evicts hStale (the one abandoned a month ago).
func TestEvictionOrdersByLastSeenNotAdded(t *testing.T) {
	s := newStore(t, tempDir(t))
	// hA is added FIRST.
	if err := s.CreateUser("U1", "hA", "小明"); err != nil {
		t.Fatal(err)
	}
	addTokens(t, s, "U1", "hStale", "h3", "h4", "h5")

	now := time.Now().UTC()
	// …but hA is the most recently USED.
	setLastSeen(t, s, "U1", "hA", now.Format(time.RFC3339))
	// hStale was added later and abandoned 30 days ago.
	setLastSeen(t, s, "U1", "hStale", now.AddDate(0, 0, -30).Format(time.RFC3339))
	for _, h := range []string{"h3", "h4", "h5"} {
		setLastSeen(t, s, "U1", h, now.AddDate(0, 0, -1).Format(time.RFC3339))
	}

	addTokens(t, s, "U1", "hNew")

	u, _ := s.User("U1")
	if _, held := u.FindToken("hStale"); held {
		t.Errorf("hStale (30 days unused) survived; tokens=%v — eviction is "+
			"not ordered by LastSeenAt", hashesOf(u))
	}
	if _, held := u.FindToken("hA"); !held {
		t.Errorf("hA was evicted even though it was used seconds ago; "+
			"tokens=%v — the LRU key is AddedAt, not LastSeenAt. The user's "+
			"ACTIVE device just got logged out while an abandoned one was kept",
			hashesOf(u))
	}
}

// TestTouchIsThrottled is TV-14: the read path must not become a write path.
func TestTouchIsThrottled(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "hA", "小明"); err != nil {
		t.Fatal(err)
	}
	// A fresh token was just seen at creation time, so nothing is due.
	before := s.PendingSinceCompact()
	for i := 0; i < 100; i++ {
		wrote, err := s.TouchTokenIfStale("U1", "hA")
		if err != nil {
			t.Fatalf("touch %d: %v", i, err)
		}
		if wrote {
			t.Fatalf("touch %d wrote an event inside the throttle window", i)
		}
	}
	if got := s.PendingSinceCompact() - before; got != 0 {
		t.Errorf("%d events appended by 100 reads, want 0 — every "+
			"authenticated request is now an fsync (DD-7)", got)
	}

	// Control group: the same call MUST write once the stamp is stale, or the
	// zero above would merely mean "touch never writes anything at all".
	setLastSeen(t, s, "U1", "hA",
		time.Now().UTC().Add(-2*TouchInterval).Format(time.RFC3339))
	mid := s.PendingSinceCompact()
	wrote, err := s.TouchTokenIfStale("U1", "hA")
	if err != nil {
		t.Fatalf("stale touch: %v", err)
	}
	if !wrote {
		t.Fatal("CONTROL GROUP FAILED: a stale token was not refreshed, so " +
			"the zero-write result above proves nothing")
	}
	if got := s.PendingSinceCompact() - mid; got != 1 {
		t.Errorf("stale touch appended %d events, want exactly 1", got)
	}
}

// TestTokenHashIsGloballyUnique is TV-15.
func TestTokenHashIsGloballyUnique(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "hA", "小明"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("U2", "hB", "小華"); err != nil {
		t.Fatal(err)
	}

	err := s.AddToken("U2", "hA")
	if err == nil {
		t.Fatal("AddToken accepted a hash already held by another user; " +
			"byToken maps a hash to exactly one uid, so sharing one would " +
			"make the index resolve an arbitrary identity")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("error = %q, want it to name the collision", err)
	}
	u2, _ := s.User("U2")
	if len(u2.Tokens) != 1 {
		t.Errorf("U2 token count = %d, want 1 (the refused add must not "+
			"partially apply)", len(u2.Tokens))
	}
	if uid, _ := s.UIDByTokenHash("hA"); uid != "U1" {
		t.Errorf("byToken[hA] = %q, want U1 unchanged", uid)
	}
}

// TestAddTokenRejectsUnknownUser covers the other AddToken refusal.
func TestAddTokenRejectsUnknownUser(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.AddToken("nobody", "hX"); err == nil {
		t.Fatal("AddToken created a token for a uid that does not exist")
	}
	if _, ok := s.UIDByTokenHash("hX"); ok {
		t.Error("a refused AddToken still left an index entry")
	}
}

// TestLastSeenAtSurvivesRestart is the fifth mandated control-group vector.
//
// ★ This is the ONLY test that can catch "LastSeenAt lives in memory only".
// Every in-process eviction test passes under that defect, because the value is
// correct right up until the process exits. After a restart it comes back equal
// to AddedAt, LRU silently degrades to FIFO, and the eviction counts still look
// perfect — the wrong device just gets logged out.
func TestLastSeenAtSurvivesRestart(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	if err := s.CreateUser("U1", "hA", "小明"); err != nil {
		t.Fatal(err)
	}
	addTokens(t, s, "U1", "hB")

	const stamp = "2026-07-04T12:00:00Z"
	setLastSeen(t, s, "U1", "hB", stamp)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen from disk — a fresh Store, replaying the same log.
	s2 := newStore(t, dir)
	u, ok := s2.User("U1")
	if !ok {
		t.Fatal("U1 missing after restart")
	}
	ref, ok := u.FindToken("hB")
	if !ok {
		t.Fatal("hB missing after restart")
	}
	if ref.LastSeenAt != stamp {
		t.Fatalf("LastSeenAt = %q after restart, want %q.\n"+
			"★ The touch never reached the log, so LRU has degraded to FIFO. "+
			"Nothing errors; the wrong device simply gets evicted from now on.",
			ref.LastSeenAt, stamp)
	}
	// And the same fact through the surviving AddedAt, to be sure the two
	// stamps did not simply collapse into one value.
	if ref.AddedAt == ref.LastSeenAt {
		t.Errorf("AddedAt == LastSeenAt == %q; the test fixture no longer "+
			"distinguishes the two fields", ref.AddedAt)
	}
}

// TestRevokeTokenRemovesBothEnds covers the type prepared for device
// management (tasks 3.5): no route reaches it yet, but its event type must be
// replayable from the first release that could write one.
func TestRevokeTokenRemovesBothEnds(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	if err := s.CreateUser("U1", "hA", "小明"); err != nil {
		t.Fatal(err)
	}
	addTokens(t, s, "U1", "hB")

	if err := s.RevokeToken("U1", "hB"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	u, _ := s.User("U1")
	if _, held := u.FindToken("hB"); held {
		t.Error("hB still in the slice after revoke")
	}
	if _, ok := s.UIDByTokenHash("hB"); ok {
		t.Error("hB still in byToken after revoke — the revoked device would " +
			"keep authenticating")
	}
	s.Close()

	// It must also survive replay, which is the reason the type exists now.
	s2 := newStore(t, dir)
	if _, ok := s2.UIDByTokenHash("hB"); ok {
		t.Error("hB came back after replay")
	}
	if _, ok := s2.UIDByTokenHash("hA"); !ok {
		t.Error("hA lost during replay of a revoke")
	}
}
