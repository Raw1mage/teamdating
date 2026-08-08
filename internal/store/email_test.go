package store

import (
	"errors"
	"strings"
	"testing"
)

// Store-level vectors for identity_access-control-email-auth.
// Every "rejects" assertion below is paired with a control arm that proves the
// same code path ACCEPTS when it should — a refusal that is actually a constant
// `return error` would satisfy the negative half alone.

// TestSetEmailPersistsAndReplays covers tasks 1.2/1.3: the address survives a
// restart because it went through the event log, not just memory.
func TestSetEmailPersistsAndReplays(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	if err := s.CreateUser("U1", "h1", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.User("U1"); u.Email != "" {
		t.Fatalf("a fresh user has email %q, want empty — unset must be the zero state (DD-6)", u.Email)
	}
	if err := s.SetEmail("U1", "luo@ex.com"); err != nil {
		t.Fatalf("SetEmail: %v", err)
	}
	if u, _ := s.User("U1"); u.Email != "luo@ex.com" {
		t.Fatalf("in-memory email is %q, want luo@ex.com", u.Email)
	}

	// The decisive half. A SetEmail that only touched memory passes every
	// assertion above and loses the address on the next restart — at which
	// point the proposal gate silently re-closes on a user who set one.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := newStore(t, dir)
	u, ok := s2.User("U1")
	if !ok {
		t.Fatal("user vanished across restart")
	}
	if u.Email != "luo@ex.com" {
		t.Errorf("after replay email is %q, want luo@ex.com — the event never "+
			"reached the log, so the value existed only in RAM", u.Email)
	}
}

// TestSetEmailClearIsAnEventNotANoop: clearing must also land, or a restart
// resurrects an address the user deliberately removed.
func TestSetEmailClearIsAnEventNotANoop(t *testing.T) {
	dir := tempDir(t)
	s := newStore(t, dir)
	if err := s.CreateUser("U1", "h1", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEmail("U1", "luo@ex.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEmail("U1", ""); err != nil {
		t.Fatalf("clearing email: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := newStore(t, dir)
	if u, _ := s2.User("U1"); u.Email != "" {
		t.Errorf("after replay email is %q, want empty — a cleared address came "+
			"back, so the proposal gate would keep passing someone who removed it", u.Email)
	}
}

// TestSetEmailRejectsMalformed, with its control arm.
func TestSetEmailRejectsMalformed(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "h1", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"not-an-email",
		"a@localhost",              // no dot in the domain: almost always a typo
		"名字 <a@b.com>",             // display-name form: stored value ≠ address
		"a@b.com\r\nBcc: x@y.com",  // header injection
		"@b.com",
		"a@",
	}
	for _, v := range bad {
		if err := s.SetEmail("U1", v); err == nil {
			t.Errorf("SetEmail(%q) was accepted — this value reaches net/smtp "+
				"as an envelope recipient", v)
		}
	}
	// CONTROL ARM: the same function must accept a real address. Without this,
	// a `return error` stub would pass every assertion above.
	if err := s.SetEmail("U1", "luo@ex.com"); err != nil {
		t.Fatalf("control arm failed: a valid address was refused (%v) — the "+
			"rejections above therefore prove nothing", err)
	}
	if u, _ := s.User("U1"); u.Email != "luo@ex.com" {
		t.Errorf("control arm: email is %q after a valid SetEmail", u.Email)
	}
}

// TestSameEmailTreatsUnsetAsNoMatch is the load-bearing half of DD-3.
func TestSameEmailTreatsUnsetAsNoMatch(t *testing.T) {
	// The defect this guards: "" == "" is true in Go, so a naive comparison
	// makes every pair of email-less accounts look like the same person, and
	// the merge path opens to anyone who types a matching name.
	if SameEmail("", "") {
		t.Error("two unset emails compared EQUAL — an absent claim is not a claim, " +
			"and treating it as one hands the merge path to anyone who knows a name")
	}
	if SameEmail("luo@ex.com", "") || SameEmail("", "luo@ex.com") {
		t.Error("an unset email matched a set one")
	}
	// Control arm: real matches must still match, including case folding —
	// otherwise the same person typing a capital is routed to the suffix branch
	// and permanently split from their own proposals.
	if !SameEmail("luo@ex.com", "luo@ex.com") {
		t.Error("control arm: identical addresses did not match")
	}
	if !SameEmail("Luo@Ex.com", "luo@ex.com") {
		t.Error("control arm: case-different forms of one address did not match")
	}
	if SameEmail("luo@ex.com", "other@ex.com") {
		t.Error("different addresses matched")
	}
}

// TestUserByNameAndEmailSplit is GRAFCET step 3: the merge/suffix decision.
func TestUserByNameAndEmailSplit(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "h1", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEmail("U1", "luo@ex.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("U2", "h2", "駱子逸"); err != nil {
		t.Fatal(err)
	}

	// (a) matching email resolves to that identity.
	got, ok := s.UserByNameAndEmail("駱子逸", "luo@ex.com")
	if !ok || got.UID != "U1" {
		t.Fatalf("matching email resolved to %+v ok=%v, want U1", got, ok)
	}
	// Case-insensitively, too.
	if got, ok := s.UserByNameAndEmail("駱子逸", "LUO@ex.com"); !ok || got.UID != "U1" {
		t.Errorf("case-different address resolved to %+v ok=%v, want U1", got, ok)
	}
	// (b) a non-matching email resolves to nobody — that is the suffix branch.
	if _, ok := s.UserByNameAndEmail("駱子逸", "other@ex.com"); ok {
		t.Error("a non-matching email resolved to an identity — this is exactly " +
			"the unverified merge the plan closes")
	}
	// U2 has no email: it must NOT be reachable through the email path at all.
	if _, ok := s.UserByNameAndEmail("駱子逸", ""); ok {
		t.Error("an empty email resolved to an identity")
	}
	// A different name never matches, however good the email is.
	if _, ok := s.UserByNameAndEmail("別人", "luo@ex.com"); ok {
		t.Error("name mismatch was ignored")
	}
}

// TestUserByNameAndEmailRefusesAmbiguity: two identities with the same name AND
// email is not a normal state; picking one by map order is the coin flip
// UsersByName was written to end.
// ★ THE FIXTURE IS BUILT THROUGH REPLAY, NOT THROUGH SetEmail, AND THAT IS NOW
// THE ONLY HONEST WAY TO BUILD IT. Email uniqueness (使用者裁示 Q5) makes this
// state unreachable on the write path — SetEmail answers ErrEmailTaken — while
// leaving it fully reachable from a log written before the rule existed, which
// is exactly why replay is exempt. Constructing it the way production would
// actually acquire it keeps the assertion honest; deleting the test instead
// would drop the defence for the one case that can still occur.
func TestUserByNameAndEmailRefusesAmbiguity(t *testing.T) {
	s := newStore(t, tempDir(t))
	for _, u := range []struct{ uid, hash string }{{"U1", "h1"}, {"U2", "h2"}} {
		if err := s.CreateUser(u.uid, u.hash, "駱子逸"); err != nil {
			t.Fatal(err)
		}
		ev := Event{Type: EvUserSetEmail,
			D: mustJSON(t, UserSetEmailData{UID: u.uid, Email: "luo@ex.com"})}
		s.mu.Lock()
		err := s.applyEvent(ev)
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	// CONTROL: the fixture really did land two identities on one address —
	// otherwise the "refuses to resolve" assertion below would pass for the
	// trivial reason that there is nothing ambiguous to resolve.
	holders := 0
	for _, u := range s.Users() {
		if NormalizeEmail(u.Email) == "luo@ex.com" {
			holders++
		}
	}
	if holders != 2 {
		t.Fatalf("control: fixture should hold 2 identities on one address, got %d", holders)
	}
	if got, ok := s.UserByNameAndEmail("駱子逸", "luo@ex.com"); ok {
		t.Errorf("resolved ambiguously to %q — with two equal candidates the "+
			"answer is map order, which is a coin flip", got.UID)
	}
}

// TestUsersByNameWithoutEmail backs the DD-9 claim path.
func TestUsersByNameWithoutEmail(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("OLD", "h1", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("NEW", "h2", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEmail("NEW", "new@ex.com"); err != nil {
		t.Fatal(err)
	}
	got := s.UsersByNameWithoutEmail("駱子逸")
	if len(got) != 1 || got[0].UID != "OLD" {
		t.Fatalf("claimable set is %+v, want exactly [OLD] — an account WITH an "+
			"email must never appear here, or the DD-9 weak path would swallow "+
			"the verified one", got)
	}
	// Control arm: a name nobody holds returns empty, so a non-empty result
	// above is a real match rather than an unconditional list.
	if n := len(s.UsersByNameWithoutEmail("查無此人")); n != 0 {
		t.Errorf("unknown name returned %d claimable identities", n)
	}
}

// TestCreateUserWithSuffixPicksLowestFree is TV-6 plus its control arm TV-6C.
func TestCreateUserWithSuffixPicksLowestFree(t *testing.T) {
	// TV-6: name / -1 / -2 taken → -3.
	t.Run("TV-6 gaps absent", func(t *testing.T) {
		s := newStore(t, tempDir(t))
		for i, n := range []string{"駱子逸", "駱子逸-1", "駱子逸-2"} {
			if err := s.CreateUser(string(rune('A'+i)), string(rune('a'+i)), n); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.CreateUserWithSuffix("NEW", "hnew", "駱子逸", "")
		if err != nil {
			t.Fatalf("CreateUserWithSuffix: %v", err)
		}
		if got != "駱子逸-3" {
			t.Errorf("allocated %q, want 駱子逸-3", got)
		}
	})

	// TV-6C CONTROL ARM: with -2 free, the allocator must pick -2, NOT -3.
	//
	// ★ This is what separates "lowest free" from "count + 1" and from a fixed
	// bump. Both wrong implementations produce 駱子逸-3 in the case above, so
	// TV-6 alone cannot tell any of them apart.
	t.Run("TV-6C gap present", func(t *testing.T) {
		s := newStore(t, tempDir(t))
		for i, n := range []string{"駱子逸", "駱子逸-1"} {
			if err := s.CreateUser(string(rune('A'+i)), string(rune('a'+i)), n); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.CreateUserWithSuffix("NEW", "hnew", "駱子逸", "")
		if err != nil {
			t.Fatalf("CreateUserWithSuffix: %v", err)
		}
		if got != "駱子逸-2" {
			t.Errorf("allocated %q, want 駱子逸-2 — the allocator is not choosing "+
				"the lowest free suffix", got)
		}
	})

	// The bare name is taken when it is free: a suffix nobody needed would
	// rename a user for no reason.
	t.Run("bare name when free", func(t *testing.T) {
		s := newStore(t, tempDir(t))
		got, err := s.CreateUserWithSuffix("NEW", "hnew", "獨一無二", "")
		if err != nil {
			t.Fatal(err)
		}
		if got != "獨一無二" {
			t.Errorf("allocated %q for an unused name, want it unchanged", got)
		}
	})
}

// TestCreateUserWithSuffixKeepsNamesUnique is the invariant itself, asserted on
// the roster rather than on the return value.
func TestCreateUserWithSuffixKeepsNamesUnique(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U0", "h0", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		uid := "U" + string(rune('1'+i))
		if _, err := s.CreateUserWithSuffix(uid, "h"+uid, "駱子逸", ""); err != nil {
			t.Fatalf("allocation %d: %v", i, err)
		}
	}
	seen := map[string]string{}
	for _, u := range s.Users() {
		if prev, dup := seen[u.Name]; dup {
			t.Errorf("name %q is held by both %q and %q — the roster invariant "+
				"(names are unique) is broken", u.Name, prev, u.UID)
		}
		seen[u.Name] = u.UID
	}
	if len(seen) != 6 {
		t.Errorf("roster holds %d distinct names, want 6", len(seen))
	}
}

// TestCreateUserWithSuffixStoresEmail: the suffixed account keeps the address
// that was supplied, so the next same-name login can be decided on it.
func TestCreateUserWithSuffixStoresEmail(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U0", "h0", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	name, err := s.CreateUserWithSuffix("U1", "h1", "駱子逸", "other@ex.com")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := s.User("U1")
	if u.Email != "other@ex.com" || u.Name != name {
		t.Errorf("suffixed account is %+v, want name=%q email=other@ex.com", u, name)
	}
	// And it is now resolvable by that address, which is what makes the SECOND
	// login from this person a merge rather than another suffix.
	if got, ok := s.UserByNameAndEmail(name, "other@ex.com"); !ok || got.UID != "U1" {
		t.Errorf("the suffixed account is not resolvable by its own email")
	}
}

// TestNameSuffixExhaustionIsReported: the bound is reported, never silently
// worked around with a random name.
func TestNameSuffixExhaustionIsReported(t *testing.T) {
	s := newStore(t, tempDir(t))
	// A 15-rune name leaves room for "-1".."-9" only (MaxNameLen 16), so the
	// space is exhaustible without creating a thousand users.
	base := strings.Repeat("あ", 15)
	if err := s.CreateUser("U0", "h0", base); err != nil {
		t.Fatal(err)
	}
	var lastErr error
	for i := 1; i <= 12; i++ {
		uid := "X" + string(rune('a'+i))
		if _, err := s.CreateUserWithSuffix(uid, "h"+uid, base, ""); err != nil {
			lastErr = err
			break
		}
	}
	if lastErr == nil {
		t.Fatal("suffix allocation never reported exhaustion for a name with no room")
	}
	// Control arm: a name WITH room must still succeed, proving the failure
	// above is about the name and not an unconditional refusal.
	if _, err := s.CreateUserWithSuffix("OK1", "hok1", "短名", ""); err != nil {
		t.Errorf("control arm: a name with room was refused (%v)", err)
	}
}

// TestErrNameSuffixExhaustedIsMatchable keeps the sentinel usable by the HTTP
// layer: a string-compared error would break silently on a message change.
func TestErrNameSuffixExhaustedIsMatchable(t *testing.T) {
	if !errors.Is(ErrNameSuffixExhausted, ErrNameSuffixExhausted) {
		t.Fatal("sentinel is not matchable by errors.Is")
	}
}

// TestNameTakenReportsHonestly backs the rename collision check.
func TestNameTakenReportsHonestly(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "h1", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	if !s.NameTaken("駱子逸") {
		t.Error("an occupied name reported as free")
	}
	// Control arm: a free name must report free, or the check would block every
	// rename and look like it was working.
	if s.NameTaken("沒人用的名字") {
		t.Error("a free name reported as taken")
	}
}

// TestUnknownEventTypeStillRefused guards DD-5's other half: adding
// EvUserSetEmail must not have turned the replayer permissive.
func TestUnknownEventTypeStillRefused(t *testing.T) {
	s := newStore(t, tempDir(t))
	err := s.Replay(strings.NewReader(`{"t":"x","type":"user_set_email_v2","d":{}}`+"\n"), "test")
	if !errors.Is(err, ErrUnknownEventType) {
		t.Fatalf("replaying an unknown type returned %v, want ErrUnknownEventType — "+
			"a replayer that skips what it does not understand makes 'we dropped a "+
			"record' and 'there was none' the same state", err)
	}
	// Control arm: the type we DID add must replay cleanly, otherwise this test
	// would pass against a replayer that rejects everything.
	if err := s.CreateUser("U1", "h1", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	if err := s.Replay(strings.NewReader(`{"t":"x","type":"user_set_email","d":{"UID":"U1","Email":"a@b.com"}}`+"\n"), "test"); err != nil {
		t.Fatalf("control arm: the new event type failed to replay: %v", err)
	}
	if u, _ := s.User("U1"); u.Email != "a@b.com" {
		t.Errorf("control arm: replayed email is %q, want a@b.com", u.Email)
	}
}
