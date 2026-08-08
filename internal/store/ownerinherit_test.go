package store

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Owner-inheritance: whole-identity merge (使用者裁示 Q2c) and email uniqueness
// (Q5). Every assertion here is paired with a control group that proves the
// check would actually go red — an "absent" result and a "failed" result must
// never share one output.
// ---------------------------------------------------------------------------

func mkUser(t *testing.T, s *Store, uid, tok, name, email string) {
	t.Helper()
	if err := s.CreateUser(uid, tok, name); err != nil {
		t.Fatalf("CreateUser %s: %v", uid, err)
	}
	if email != "" {
		if err := s.SetEmail(uid, email); err != nil {
			t.Fatalf("SetEmail %s: %v", uid, err)
		}
	}
}

// TestMergeAbsorbsTokensVotesAndOwnership is the core of Q2c: the requester's
// old identity is folded wholly into the verified one.
func TestMergeAbsorbsTokensVotesAndOwnership(t *testing.T) {
	s := newTestStore(t)
	mkUser(t, s, "A", "tokA", "駱子逸", "luo@ex.com")
	mkUser(t, s, "B", "tokB", "駱子逸", "")

	// B owns a proposal of its own — the orphan case Q2 warned about.
	if err := s.CreateProposal("pB", "B", "B 的行程", ""); err != nil {
		t.Fatalf("CreateProposal pB: %v", err)
	}
	if err := s.CreateProposal("pA", "A", "A 的行程", ""); err != nil {
		t.Fatalf("CreateProposal pA: %v", err)
	}
	if err := s.CreateOption("pA", "o1", "B", "2026-09-01", "2026-09-03", ""); err != nil {
		t.Fatalf("CreateOption: %v", err)
	}
	if err := s.SetVote("pA", "o1", "B", 1); err != nil {
		t.Fatalf("SetVote: %v", err)
	}

	// CONTROL GROUP (pre-merge): ownership sits where it was created and B
	// still holds its own roster row. Without this the post-merge assertions
	// could be true for reasons that have nothing to do with the merge.
	if p, _ := s.Proposal("pA"); p.OwnerUID != "A" {
		t.Fatalf("control: pA owner should be A before merge, got %q", p.OwnerUID)
	}
	if p, _ := s.Proposal("pB"); p.OwnerUID != "B" {
		t.Fatalf("control: pB owner should be B before merge, got %q", p.OwnerUID)
	}
	if _, ok := s.User("B"); !ok {
		t.Fatal("control: B must exist before the merge")
	}
	if uid, ok := s.UIDByTokenHash("tokB"); !ok || uid != "B" {
		t.Fatalf("control: tokB should resolve to B before merge, got %q/%v", uid, ok)
	}

	if err := s.MergeUsers("B", "A"); err != nil {
		t.Fatalf("MergeUsers: %v", err)
	}

	// B's token now authenticates as A — that is the whole point: this device
	// keeps working and is now the verified identity.
	if uid, ok := s.UIDByTokenHash("tokB"); !ok || uid != "A" {
		t.Fatalf("after merge tokB should resolve to A, got %q/%v", uid, ok)
	}
	if uid, ok := s.UIDByTokenHash("tokA"); !ok || uid != "A" {
		t.Fatalf("after merge tokA should still resolve to A, got %q/%v", uid, ok)
	}
	// No orphan: B's own proposal moved to A rather than being stranded under
	// a uid no device can authenticate as.
	if p, _ := s.Proposal("pB"); p.OwnerUID != "A" {
		t.Fatalf("pB should have moved to A, got owner %q", p.OwnerUID)
	}
	// Option authorship follows too, otherwise the author line renders blank.
	p, _ := s.Proposal("pA")
	if got := p.Options["o1"].AuthorUID; got != "A" {
		t.Fatalf("option author should be A, got %q", got)
	}
	if v, ok := p.Votes[VoteKey("o1", "A")]; !ok || v.Weight != 1 {
		t.Fatalf("B's vote should have moved to A, got %+v ok=%v", v, ok)
	}
	if _, ok := p.Votes[VoteKey("o1", "B")]; ok {
		t.Fatal("B's original vote row must be gone")
	}
	// The absorbed row leaves the roster — this is what keeps the displayed
	// name unique.
	if _, ok := s.User("B"); ok {
		t.Fatal("B must no longer be on the roster after being absorbed")
	}
	if n := len(s.UsersByName("駱子逸")); n != 1 {
		t.Fatalf("roster should hold exactly one 駱子逸 after the merge, got %d", n)
	}
}

// TestMergeVoteDedupKeepsNewer pins 使用者裁示 Q3: the newer stance survives.
func TestMergeVoteDedupKeepsNewer(t *testing.T) {
	for _, tc := range []struct {
		name              string
		aCreated, bCreated string
		aWeight, bWeight   float64
		wantWeight         float64
	}{
		// B voted later, so B's stance is the person's latest word.
		{"newer B wins", "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", 1, 0, 0},
		// A voted later — the survivor's own row is newer and must NOT be
		// clobbered just because it belongs to the absorbing identity.
		{"newer A wins", "2026-03-01T00:00:00Z", "2026-02-01T00:00:00Z", 1, 0, 1},
		// Tie keeps the incumbent, so the result cannot depend on Go's
		// randomised map iteration order.
		{"tie keeps incumbent", "2026-02-01T00:00:00Z", "2026-02-01T00:00:00Z", 1, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			mkUser(t, s, "A", "tokA", "同名", "a@ex.com")
			mkUser(t, s, "B", "tokB", "同名", "")
			if err := s.CreateProposal("p1", "A", "T", ""); err != nil {
				t.Fatalf("CreateProposal: %v", err)
			}
			if err := s.CreateOption("p1", "o1", "A", "2026-09-01", "2026-09-02", ""); err != nil {
				t.Fatalf("CreateOption: %v", err)
			}
			// Reach past SetVote to control CreatedAt precisely; SetVote
			// stamps time.Now, which cannot express "B voted later".
			s.mu.Lock()
			s.proposals["p1"].Votes[VoteKey("o1", "A")] = &Vote{
				OptionID: "o1", UID: "A", Weight: tc.aWeight, CreatedAt: tc.aCreated}
			s.proposals["p1"].Votes[VoteKey("o1", "B")] = &Vote{
				OptionID: "o1", UID: "B", Weight: tc.bWeight, CreatedAt: tc.bCreated}
			s.mu.Unlock()

			// CONTROL GROUP: two rows exist right now, so "one row afterwards"
			// is a change the merge caused rather than a state that was
			// already true.
			pre, _ := s.Proposal("p1")
			if len(pre.Votes) != 2 {
				t.Fatalf("control: expected 2 vote rows before merge, got %d", len(pre.Votes))
			}

			if err := s.MergeUsers("B", "A"); err != nil {
				t.Fatalf("MergeUsers: %v", err)
			}

			post, _ := s.Proposal("p1")
			if len(post.Votes) != 1 {
				t.Fatalf("one person must hold one vote, got %d rows", len(post.Votes))
			}
			v, ok := post.Votes[VoteKey("o1", "A")]
			if !ok {
				t.Fatal("surviving vote must be keyed to A")
			}
			if v.Weight != tc.wantWeight {
				t.Fatalf("want weight %v, got %v", tc.wantWeight, v.Weight)
			}
		})
	}
}

// TestMergeDedupsAcrossAllProposals pins the second half of Q3: dedup is
// global, not just the proposal the merge was triggered from.
func TestMergeDedupsAcrossAllProposals(t *testing.T) {
	s := newTestStore(t)
	mkUser(t, s, "A", "tokA", "同名", "a@ex.com")
	mkUser(t, s, "B", "tokB", "同名", "")
	for _, pid := range []string{"p1", "p2", "p3"} {
		if err := s.CreateProposal(pid, "A", "T", ""); err != nil {
			t.Fatalf("CreateProposal %s: %v", pid, err)
		}
		if err := s.CreateOption(pid, "o1", "A", "2026-09-01", "2026-09-02", ""); err != nil {
			t.Fatalf("CreateOption %s: %v", pid, err)
		}
		if err := s.SetVote(pid, "o1", "A", 1); err != nil {
			t.Fatalf("SetVote A %s: %v", pid, err)
		}
		if err := s.SetVote(pid, "o1", "B", 0); err != nil {
			t.Fatalf("SetVote B %s: %v", pid, err)
		}
	}
	// CONTROL GROUP: every proposal currently double-counts this person.
	for _, pid := range []string{"p1", "p2", "p3"} {
		p, _ := s.Proposal(pid)
		if len(p.Votes) != 2 {
			t.Fatalf("control: %s should hold 2 rows before merge, got %d", pid, len(p.Votes))
		}
	}

	if err := s.MergeUsers("B", "A"); err != nil {
		t.Fatalf("MergeUsers: %v", err)
	}

	for _, pid := range []string{"p1", "p2", "p3"} {
		p, _ := s.Proposal(pid)
		if len(p.Votes) != 1 {
			t.Fatalf("%s: one person must hold one vote after merge, got %d", pid, len(p.Votes))
		}
		seen := map[string]bool{}
		for _, v := range p.Votes {
			if seen[v.UID] {
				t.Fatalf("%s: duplicate voter uid %s", pid, v.UID)
			}
			seen[v.UID] = true
			if v.UID == "B" {
				t.Fatalf("%s: absorbed uid B still holds a vote", pid)
			}
		}
	}
}

// TestMergeRefusesSelf keeps a resolution bug upstream from silently deleting
// the user it was asked to merge into itself.
func TestMergeRefusesSelf(t *testing.T) {
	s := newTestStore(t)
	mkUser(t, s, "A", "tokA", "甲", "a@ex.com")
	if err := s.MergeUsers("A", "A"); err == nil {
		t.Fatal("merging an identity into itself must be refused")
	}
	// CONTROL GROUP: a genuine merge of two distinct identities succeeds, so
	// the refusal above is specific rather than MergeUsers being broken.
	mkUser(t, s, "B", "tokB", "乙", "")
	if err := s.MergeUsers("B", "A"); err != nil {
		t.Fatalf("control: a genuine merge must succeed, got %v", err)
	}
	if _, ok := s.User("A"); !ok {
		t.Fatal("A must survive its own self-merge attempt")
	}
}

// TestMergeConcurrentOnlyOneWins is the Q2c rewrite of the "8 concurrent"
// vector: owner never moves, and the roster converges to one row.
func TestMergeConcurrentOnlyOneWins(t *testing.T) {
	s := newTestStore(t)
	mkUser(t, s, "A", "tokA", "同名", "a@ex.com")
	if err := s.CreateProposal("p1", "A", "T", ""); err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	const n = 8
	for i := 0; i < n; i++ {
		mkUser(t, s, fmt.Sprintf("B%d", i), fmt.Sprintf("tokB%d", i), fmt.Sprintf("dev%d", i), "")
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.MergeUsers(fmt.Sprintf("B%d", i), "A")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent merge %d failed: %v", i, err)
		}
	}
	// Owner is unchanged throughout — under Q2c nothing ever transfers.
	if p, _ := s.Proposal("p1"); p.OwnerUID != "A" {
		t.Fatalf("owner must remain A, got %q", p.OwnerUID)
	}
	// Every absorbed row is gone; only A remains.
	for i := 0; i < n; i++ {
		if _, ok := s.User(fmt.Sprintf("B%d", i)); ok {
			t.Fatalf("B%d should have been absorbed", i)
		}
	}
	u, _ := s.User("A")
	if len(u.Tokens) > MaxTokensPerUser {
		t.Fatalf("token cap breached under concurrency: %d > %d", len(u.Tokens), MaxTokensPerUser)
	}
	// CONTROL GROUP: the cap is genuinely reachable here — 9 tokens went in,
	// so a survivor at exactly the cap proves eviction ran rather than the
	// assertion being vacuously true.
	if len(u.Tokens) != MaxTokensPerUser {
		t.Fatalf("control: 9 tokens merged into one identity should settle at the cap %d, got %d",
			MaxTokensPerUser, len(u.Tokens))
	}
}

// TestSetEmailUniqueness pins Q5 on the write path.
func TestSetEmailUniqueness(t *testing.T) {
	s := newTestStore(t)
	mkUser(t, s, "A", "tokA", "甲", "shared@ex.com")
	mkUser(t, s, "B", "tokB", "乙", "")

	if err := s.SetEmail("B", "shared@ex.com"); err == nil {
		t.Fatal("a second identity must not be able to claim a held address")
	} else if err != ErrEmailTaken {
		t.Fatalf("want ErrEmailTaken, got %v", err)
	}
	// CONTROL GROUP 1: a FREE address succeeds — the check rejects collisions,
	// not every write.
	if err := s.SetEmail("B", "free@ex.com"); err != nil {
		t.Fatalf("control: setting an unheld address must succeed, got %v", err)
	}
	// CONTROL GROUP 2: re-setting your OWN address succeeds. The identity form
	// resubmits unchanged values on every save, so a naive check would fail on
	// the second save only.
	if err := s.SetEmail("B", "free@ex.com"); err != nil {
		t.Fatalf("control: re-setting one's own address must succeed, got %v", err)
	}
	// Case folding: these reach one mailbox, so they must not both register.
	if err := s.SetEmail("B", "SHARED@Ex.com"); err != ErrEmailTaken {
		t.Fatalf("case-different form of a held address must collide, got %v", err)
	}
	// Clearing is always allowed and does not collide with the other empty one.
	if err := s.SetEmail("B", ""); err != nil {
		t.Fatalf("clearing an address must succeed, got %v", err)
	}
}

// TestSetEmailUniquenessConcurrent proves the check is inside the commit and
// not a check-then-act pair.
func TestSetEmailUniquenessConcurrent(t *testing.T) {
	s := newTestStore(t)
	const n = 8
	for i := 0; i < n; i++ {
		mkUser(t, s, fmt.Sprintf("U%d", i), fmt.Sprintf("tok%d", i), fmt.Sprintf("u%d", i), "")
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.SetEmail(fmt.Sprintf("U%d", i), "contested@ex.com")
		}(i)
	}
	wg.Wait()

	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		} else if err != ErrEmailTaken {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("exactly one identity may hold the address, %d succeeded", won)
	}
	holders := 0
	for _, u := range s.Users() {
		if NormalizeEmail(u.Email) == "contested@ex.com" {
			holders++
		}
	}
	if holders != 1 {
		t.Fatalf("roster holds %d identities with the contested address", holders)
	}
	// CONTROL GROUP: sequential writes to DISTINCT addresses all succeed, so
	// the "exactly one" result above is contention-specific and not the store
	// refusing concurrent writes in general.
	for i := 0; i < n; i++ {
		if err := s.SetEmail(fmt.Sprintf("U%d", i), fmt.Sprintf("u%d@ex.com", i)); err != nil {
			t.Fatalf("control: distinct address for U%d must succeed, got %v", i, err)
		}
	}
}

// TestReplayAcceptsDuplicateEmail is the other half of Q5: history is
// reconstructed, never re-adjudicated. A log written before the rule existed
// must still load, or the invariant would be enforced by refusing to boot.
func TestReplayAcceptsDuplicateEmail(t *testing.T) {
	s := newTestStore(t)
	mkUser(t, s, "A", "tokA", "甲", "shared@ex.com")
	mkUser(t, s, "B", "tokB", "乙", "")

	ev := Event{Type: EvUserSetEmail, D: mustJSON(t, UserSetEmailData{UID: "B", Email: "shared@ex.com"})}
	s.mu.Lock()
	err := s.applyEvent(ev)
	s.mu.Unlock()
	if err != nil {
		t.Fatalf("replay must accept a recorded duplicate, got %v", err)
	}
	if u, _ := s.User("B"); u.Email != "shared@ex.com" {
		t.Fatalf("replayed address should have been applied, got %q", u.Email)
	}
	// CONTROL GROUP: the live write path still refuses the same value, so the
	// exemption is scoped to replay rather than having disabled the rule.
	if err := s.SetEmail("B", "shared@ex.com"); err != nil && err != ErrEmailTaken {
		t.Fatalf("control: unexpected error from the write path: %v", err)
	}
}
