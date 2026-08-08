package store

import (
	"encoding/json"
	"sync"
	"testing"
)

// The commit observer and seq counter (DD-21) are the seam pkg2b's SSE hub
// plugs into. They are reserved, not yet wired — which is exactly why they
// need tests now: an unexercised seam is a placeholder, and its defects would
// surface during hub integration instead of here.

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestSeqAdvancesOnlyOnCommittedMutations pins what a sequence number means.
//
// ★ A rejected mutation must NOT burn a number. seq is the client's proof that
// it has seen every committed event; skipping one would make a correct client
// conclude it had missed something and re-fetch forever.
func TestSeqAdvancesOnlyOnCommittedMutations(t *testing.T) {
	s := newTestStore(t)
	if got := s.Seq(); got != 0 {
		t.Fatalf("fresh store Seq() = %d, want 0", got)
	}

	if err := s.CreateUser("U1", "hash1", "Alice"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if got := s.Seq(); got != 1 {
		t.Errorf("after one commit Seq() = %d, want 1", got)
	}

	// A validation failure: the uid already exists. Nothing is appended, so
	// nothing may be numbered.
	if err := s.CreateUser("U1", "hash2", "Dup"); err == nil {
		t.Fatal("control group failed: the duplicate was accepted, so the " +
			"assertion below would not be testing a rejected mutation")
	}
	if got := s.Seq(); got != 1 {
		t.Errorf("a REJECTED mutation advanced Seq() to %d, want 1", got)
	}

	if err := s.CreateProposal("P1", "U1", "Trip", ""); err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if got := s.Seq(); got != 2 {
		t.Errorf("after two commits Seq() = %d, want 2", got)
	}
}

// TestObserverSeesEveryCommitInOrder exercises the notification path end to
// end and asserts the property DD-21 exists for.
func TestObserverSeesEveryCommitInOrder(t *testing.T) {
	s := newTestStore(t)

	type seen struct {
		seq uint64
		typ string
	}
	var mu sync.Mutex
	var got []seen
	s.SetObserver(func(seq uint64, ev Event) {
		mu.Lock()
		got = append(got, seen{seq, ev.Type})
		mu.Unlock()
	})

	if err := s.CreateUser("U1", "h1", "Alice"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.CreateProposal("P1", "U1", "Trip", ""); err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}
	if err := s.CreateOption("P1", "O1", "U1", "2026-09-01", "2026-09-03", ""); err != nil {
		t.Fatalf("CreateOption: %v", err)
	}
	// Rejected: must not reach the observer.
	if err := s.CreateOption("P1", "O1", "U1", "2026-10-01", "2026-10-02", ""); err == nil {
		t.Fatal("control group failed: the duplicate oid was accepted")
	}
	if err := s.SetVote("P1", "O1", "U1", WeightYes); err != nil {
		t.Fatalf("SetVote: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []seen{
		{1, EvUserCreate}, {2, EvProposalCreate},
		{3, EvOptionCreate}, {4, EvVoteSet},
	}
	if len(got) != len(want) {
		t.Fatalf("observer saw %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestObserverPayloadIsUsable guards the event body, not just its type.
//
// ★ notify re-marshals the payload. If it handed over an empty or wrong body,
// every ordering assertion above would still pass while the hub shipped blank
// events to clients — a green suite with no discriminating power over content.
func TestObserverPayloadIsUsable(t *testing.T) {
	s := newTestStore(t)
	var payload []byte
	var typ string
	s.SetObserver(func(_ uint64, ev Event) {
		typ, payload = ev.Type, append([]byte(nil), ev.D...)
	})
	if err := s.CreateUser("U7", "h7", "Zoe"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if typ != EvUserCreate {
		t.Errorf("type = %q, want %q", typ, EvUserCreate)
	}
	if len(payload) == 0 {
		t.Fatal("observer received an empty payload")
	}
	var d UserCreateData
	if err := json.Unmarshal(payload, &d); err != nil {
		t.Fatalf("payload is not valid JSON for its type: %v", err)
	}
	if d.UID != "U7" || d.Name != "Zoe" {
		t.Errorf("payload = %+v, want UID=U7 Name=Zoe", d)
	}
}

// TestNilObserverIsInert is the regression guard for pkg1.
//
// Production leaves the observer nil until the hub is wired in pkg2b, so the
// default path must behave exactly as it did before DD-21 was added.
func TestNilObserverIsInert(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateUser("U1", "h1", "Alice"); err != nil {
		t.Fatalf("CreateUser with nil observer: %v", err)
	}
	if got := s.Seq(); got != 1 {
		t.Errorf("Seq() = %d with a nil observer, want 1 — numbering must not "+
			"depend on anyone listening", got)
	}
	// Detaching mid-life must also be safe.
	s.SetObserver(func(uint64, Event) {})
	s.SetObserver(nil)
	if err := s.CreateProposal("P1", "U1", "Trip", ""); err != nil {
		t.Fatalf("commit after detaching observer: %v", err)
	}
}

// TestObserverOrderMatchesCommitOrderUnderConcurrency is the property that
// justifies notifying inside writeMu.
//
// ★ If the notification were moved outside the lock, two commits could land as
// A,B and be announced as B,A. This test drives many concurrent writers and
// asserts the observer's sequence is strictly increasing with no gaps and no
// duplicates — the shape that would break first.
func TestObserverOrderMatchesCommitOrderUnderConcurrency(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateUser("U1", "h1", "Alice"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.CreateProposal("P1", "U1", "Trip", ""); err != nil {
		t.Fatalf("CreateProposal: %v", err)
	}

	var mu sync.Mutex
	var seqs []uint64
	s.SetObserver(func(seq uint64, _ Event) {
		mu.Lock()
		seqs = append(seqs, seq)
		mu.Unlock()
	})

	const writers, each = 8, 12
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				oid := string(rune('a'+w)) + string(rune('0'+i))
				_ = s.CreateOption("P1", oid, "U1",
					"2026-09-01", "2026-09-0"+string(rune('1'+i%3)), oid)
			}
		}(w)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seqs) == 0 {
		t.Fatal("no events observed; the concurrency below proves nothing")
	}
	// Observed under the writer lock, so arrival order IS commit order: the
	// numbers must come out strictly increasing by one.
	for i, s := range seqs {
		// seq starts at 3 here: user, proposal, then the options.
		if want := uint64(i + 3); s != want {
			t.Fatalf("observation %d carried seq %d, want %d — notifications "+
				"are not ordered with commits (DD-21)", i, s, want)
		}
	}
	t.Logf("observed %d concurrent commits in strict order", len(seqs))
}
