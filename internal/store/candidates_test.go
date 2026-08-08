package store

import "testing"

// Store-level vectors for same-name candidate lookup (round 3).

// TestUsersByNameReturnsEveryMatch is the core replacement for the old
// first-match UIDByName.
func TestUsersByNameReturnsEveryMatch(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "h1", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("U2", "h2", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser("U3", "h3", "小明"); err != nil {
		t.Fatal(err)
	}

	got := s.UsersByName("駱子逸")
	if len(got) != 2 {
		t.Fatalf("got %d matches, want 2 — returning one of several same-name "+
			"identities is a coin flip, and on live data it landed on the "+
			"identity holding none of the user's work", len(got))
	}
	seen := map[string]bool{}
	for _, u := range got {
		seen[u.UID] = true
		if u.Name != "駱子逸" {
			t.Errorf("match %q has name %q", u.UID, u.Name)
		}
	}
	if !seen["U1"] || !seen["U2"] {
		t.Errorf("matches are %v, want both U1 and U2", seen)
	}
	if seen["U3"] {
		t.Error("a differently-named user was returned")
	}

	// Control arm: the query must return something when it should, otherwise
	// an implementation that always returned an empty slice would satisfy the
	// negative assertions above.
	if n := len(s.UsersByName("小明")); n != 1 {
		t.Errorf("UsersByName(小明) returned %d, want 1", n)
	}
	if n := len(s.UsersByName("查無此人")); n != 0 {
		t.Errorf("UsersByName(查無此人) returned %d, want 0", n)
	}
}

// TestUsersByNameNeverMatchesBlank keeps a nameless stub unreachable.
//
// ★ An unregistered roster entry has an empty Name. If a blank query matched
// it, anyone submitting an empty name would be offered someone else's
// identity — and the roster is full of such stubs by design.
func TestUsersByNameNeverMatchesBlank(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("USTUB", "hstub", ""); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"", "   ", "\t"} {
		if n := len(s.UsersByName(q)); n != 0 {
			t.Errorf("UsersByName(%q) returned %d matches, want 0", q, n)
		}
	}
}

// TestUsersByNameOrderIsStable pins the ordering.
//
// ★ Map iteration in Go is deliberately randomised, so an unsorted
// implementation returns a DIFFERENT order on each call. The user-visible
// damage is specific: the candidate list reorders itself between the render
// and the tap, so a mis-tap joins the wrong identity — the exact failure the
// candidate list exists to prevent. Nothing about a single call looks wrong,
// which is why this asserts across repeated calls.
func TestUsersByNameOrderIsStable(t *testing.T) {
	s := newStore(t, tempDir(t))
	for _, uid := range []string{"UE", "UB", "UA", "UD", "UC"} {
		if err := s.CreateUser(uid, "h"+uid, "駱子逸"); err != nil {
			t.Fatal(err)
		}
	}

	first := s.UsersByName("駱子逸")
	if len(first) != 5 {
		t.Fatalf("got %d matches, want 5", len(first))
	}
	for i := 0; i < 40; i++ {
		again := s.UsersByName("駱子逸")
		if len(again) != len(first) {
			t.Fatalf("call %d returned %d matches, want %d", i, len(again), len(first))
		}
		for j := range first {
			if again[j].UID != first[j].UID {
				t.Fatalf("call %d reordered the candidates: position %d was %q, "+
					"now %q. The list the user is looking at would shift under "+
					"their finger", i, j, first[j].UID, again[j].UID)
			}
		}
	}
}

// TestUsersByNameReturnsDeepCopies keeps the round-2 aliasing fix from being
// reintroduced through a new accessor.
//
// ★ THE DEFECT THIS GUARDS IS ONE LINE AWAY AT ALL TIMES. `*u` and `u.clone()`
// look equally correct at the call site, compile identically, and differ only
// in whether the caller's Tokens slice shares the store's backing array. The
// original defect reached production this way; a new accessor added later is
// the obvious place for it to come back.
func TestUsersByNameReturnsDeepCopies(t *testing.T) {
	s := newStore(t, tempDir(t))
	if err := s.CreateUser("U1", "hA", "駱子逸"); err != nil {
		t.Fatal(err)
	}
	addTokens(t, s, "U1", "hB")

	got := s.UsersByName("駱子逸")
	if len(got) != 1 || len(got[0].Tokens) != 2 {
		t.Fatalf("setup: got %d users with %v tokens", len(got), got)
	}

	// Scribble on the copy the way a caller legitimately may.
	got[0].Tokens[0].Hash = "TAMPERED"
	got[0].Tokens = got[0].Tokens[:1]

	after, ok := s.User("U1")
	if !ok {
		t.Fatal("user vanished")
	}
	if len(after.Tokens) != 2 {
		t.Fatalf("store now holds %d tokens, want 2 — the returned slice shared "+
			"the store's backing array", len(after.Tokens))
	}
	if _, held := after.FindToken("hA"); !held {
		t.Error("hA was overwritten in the store through a returned copy")
	}
	if _, held := after.FindToken("TAMPERED"); held {
		t.Error("a value written into a caller's copy appeared in the store")
	}
	// And the index must be untouched too.
	if uid, ok := s.UIDByTokenHash("hA"); !ok || uid != "U1" {
		t.Errorf("hA resolves to (%q,%v), want (U1,true)", uid, ok)
	}
}
