package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"teamdating/internal/store"
)

// Tests for the four defects VANS round 1 returned. Each one carries the
// control group that proves it is measuring the defect rather than an
// accident of the fixture.

// ---------------------------------------------------------------------------
// R1-1 — /api/health must not leak whether a submitted address is on file.
// ---------------------------------------------------------------------------

// healthBody returns the raw health response, so an assertion can be made
// about fields that must NOT exist. A typed decode cannot see those: json
// silently drops keys the struct does not declare, so a leaked field would
// stay invisible to every typed test.
func healthBody(t *testing.T, e *env) string {
	t.Helper()
	var raw json.RawMessage
	if code := e.anonClient().do("GET", "/api/health", nil, &raw); code != http.StatusOK {
		t.Fatalf("health: status %d", code)
	}
	return string(raw)
}

// TestHealthDoesNotRevealWhetherAnEmailIsOnFile is the R1-1 vector.
//
// ★ THE CONTROL GROUP IS THE FIRST HALF AND IT IS NOT OPTIONAL. The claim
// "health no longer distinguishes a hit from a miss" is satisfied trivially by
// a server where verify-email stopped working altogether — nothing would be
// created on either path and the two would match for the wrong reason. So the
// test first proves, through the pending table directly, that a match and a
// miss DO still differ underneath (1 entry vs 0). Only then does the absence
// of that difference in the public response mean anything.
func TestHealthDoesNotRevealWhetherAnEmailIsOnFile(t *testing.T) {
	m := newMailEnv(t)
	m.registerWithEmail("駱子逸", "luo@ex.com")

	// --- control arm: the underlying difference still exists ---
	beforeMiss := m.api.pending.pendingCount()
	var out map[string]any
	if code := m.anonClient().do("POST", "/api/verify-email",
		map[string]any{"name": "駱子逸", "email": "wrong@ex.com"}, &out); code != http.StatusOK {
		t.Fatalf("miss: status %d", code)
	}
	missDelta := m.api.pending.pendingCount() - beforeMiss

	beforeHit := m.api.pending.pendingCount()
	if code := m.anonClient().do("POST", "/api/verify-email",
		map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, &out); code != http.StatusOK {
		t.Fatalf("hit: status %d", code)
	}
	hitDelta := m.api.pending.pendingCount() - beforeHit

	if missDelta != 0 || hitDelta != 1 {
		t.Fatalf("control group failed: miss delta %d (want 0), hit delta %d "+
			"(want 1). The two paths no longer differ underneath, so this test "+
			"cannot show that the difference is hidden — it would pass even on "+
			"a server where verification stopped working entirely",
			missDelta, hitDelta)
	}

	// --- the assertion: none of that reaches an anonymous caller ---
	body := healthBody(t, m.env)
	if !strings.Contains(body, "candidateProbesTotal") {
		t.Fatalf("this is not the health response, so the absence of a pending "+
			"figure proves nothing: %s", body)
	}
	for _, leaked := range []string{"pendingVerifications", "pendingHighWater"} {
		if strings.Contains(body, leaked) {
			t.Errorf("health publishes %q: %s\n"+
				"verify-email answers a matching and a non-matching address "+
				"identically so it cannot be asked 'is this address on file'. "+
				"Any figure here that moves with a single pending entry answers "+
				"it anyway — read health, submit a guess, read health again.",
				leaked, body)
		}
	}
}

// TestHealthPendingFigureStaysConstantAcrossAGuess is the same claim asserted
// behaviourally rather than on field names, so a future rename cannot restore
// the leak past the test above.
func TestHealthPendingFigureStaysConstantAcrossAGuess(t *testing.T) {
	m := newMailEnv(t)
	m.registerWithEmail("駱子逸", "luo@ex.com")

	before := healthBody(t, m.env)
	var out map[string]any
	if code := m.anonClient().do("POST", "/api/verify-email",
		map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, &out); code != http.StatusOK {
		t.Fatalf("hit: status %d", code)
	}
	after := healthBody(t, m.env)

	// Control: the guess really did land — otherwise identical bodies would
	// prove only that nothing happened at all.
	if n := m.api.pending.pendingCount(); n != 1 {
		t.Fatalf("control group failed: the correct guess created %d pending "+
			"entries, want 1; nothing was probed, so identical health bodies "+
			"mean nothing", n)
	}
	if before != after {
		t.Errorf("the health response changed across a single correct guess:\n"+
			"before: %s\nafter:  %s\n"+
			"whatever moved is an oracle for 'is this address on file'",
			before, after)
	}
}

// ---------------------------------------------------------------------------
// R1-2 — every traceless anonymous answer must be counted.
// ---------------------------------------------------------------------------

// TestEmailSplitBranchesAreCounted covers the three branches the email split
// added. Each answers an unauthenticated caller and writes nothing to the
// store, which is the exact shape candidateProbes exists to make visible.
func TestEmailSplitBranchesAreCounted(t *testing.T) {
	t.Run("needEmailVerify on a matching address", func(t *testing.T) {
		m := newMailEnv(t)
		m.registerWithEmail("駱子逸", "luo@ex.com")

		before := healthProbeCount(t, m.env)
		var out map[string]any
		if code := m.anonClient().do("POST", "/api/register",
			map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, &out); code != http.StatusOK {
			t.Fatalf("register: status %d", code)
		}
		if out["needEmailVerify"] != true {
			t.Fatalf("this is not the branch under test: %v", out)
		}
		if got := healthProbeCount(t, m.env) - before; got != 1 {
			t.Errorf("candidateProbesTotal rose by %d, want 1 — this branch "+
				"answers an anonymous caller and changes nothing, so it leaves "+
				"no trace in the event log; uncounted, 'nobody probed' and "+
				"'somebody enumerated all week' are the same empty log", got)
		}
	})

	// ★ THE MISS ARM MUST COST THE SAME AS THE HIT ARM. Counting only the hit
	// would rebuild the R1-1 oracle on a different field: two health reads
	// around a guess would answer "was that address on file".
	t.Run("a non-matching address costs the same", func(t *testing.T) {
		m := newMailEnv(t)
		m.registerWithEmail("駱子逸", "luo@ex.com")

		before := healthProbeCount(t, m.env)
		var out map[string]any
		if code := m.anonClient().do("POST", "/api/register",
			map[string]any{"name": "駱子逸", "email": "stranger@ex.com"}, &out); code != http.StatusOK {
			t.Fatalf("register: status %d", code)
		}
		if out["nameCollision"] != true {
			t.Fatalf("this is not the branch under test: %v", out)
		}
		if got := healthProbeCount(t, m.env) - before; got != 1 {
			t.Errorf("candidateProbesTotal rose by %d on a NON-matching "+
				"address, want 1 — the matching arm rises by 1, so a different "+
				"number here is itself the oracle: two health reads around a "+
				"guess would reveal whether the address was on file", got)
		}
	})

	t.Run("emailRequired when no email was supplied", func(t *testing.T) {
		m := newMailEnv(t)
		m.registerWithEmail("駱子逸", "luo@ex.com")

		before := healthProbeCount(t, m.env)
		var out map[string]any
		if code := m.anonClient().do("POST", "/api/register",
			map[string]any{"name": "駱子逸"}, &out); code != http.StatusOK {
			t.Fatalf("register: status %d", code)
		}
		if out["emailRequired"] != true {
			t.Fatalf("this is not the branch under test: %v", out)
		}
		if got := healthProbeCount(t, m.env) - before; got != 1 {
			t.Errorf("candidateProbesTotal rose by %d, want 1", got)
		}
	})
}

// TestOrdinaryRegistrationStillDoesNotCount is the control group for the three
// subtests above: it proves the counter tracks traceless answers specifically,
// not "requests to /api/register". A counter that ticked on everything would
// satisfy all of them and mean nothing.
func TestOrdinaryRegistrationStillDoesNotCount(t *testing.T) {
	m := newMailEnv(t)

	before := healthProbeCount(t, m.env)
	m.newClient("全新的人")
	if got := healthProbeCount(t, m.env) - before; got != 0 {
		t.Errorf("candidateProbesTotal rose by %d on a plain registration that "+
			"minted an identity, want 0 — that request DOES leave a trace in "+
			"the event log, and counting it would make a rising number "+
			"meaningless as a probe signal", got)
	}
}

// TestUniquenessCommentDoesNotClaimToBeTheOnlyBranch is a source guard.
//
// ★ A behavioural test cannot catch this. The comment on the multi-candidate
// branch used to assert it was "THE ONLY BRANCH" that answers anonymously
// without changing state; the email split added three more, and a reader who
// believes the comment stops looking for them. A claim a reader can check
// against the file and find false is worse than no claim, because it stops the
// search that would have found the gap.
func TestUniquenessCommentDoesNotClaimToBeTheOnlyBranch(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	code := string(src)

	// Control group: prove the matcher can fire at all.
	if !strings.Contains(strings.ToUpper("the only branch of this endpoint"), "ONLY BRANCH") {
		t.Fatal("control group failed: the matcher cannot detect a known-bad shape")
	}

	if strings.Contains(code, "THIS IS THE ONLY BRANCH OF THIS ENDPOINT") {
		t.Error("handlers.go still claims the multi-candidate branch is the " +
			"ONLY traceless one; countProbe now has four call sites, so the " +
			"comment is checkable against the file and false")
	}

	// The counting itself must go through one helper, or the next branch added
	// will quietly skip it exactly as these three did.
	if n := strings.Count(code, "a.countProbe("); n < 4 {
		t.Errorf("countProbe has %d call sites, want at least 4 (multi-candidate, "+
			"uid-pick-with-email, same-name-with-email, emailRequired)", n)
	}
	// ★ EXACTLY ONE direct increment, and it must be countProbe's own. Zero
	// would mean the helper does not actually count anything; two or more means
	// a second increment path exists, which is precisely how one branch ends up
	// counted and its sibling does not — the oracle R1-2 was about.
	if n := strings.Count(code, "a.candidateProbes.Add("); n != 1 {
		t.Errorf("handlers.go has %d direct candidateProbes.Add call sites, want "+
			"exactly 1 (countProbe's own); every branch must increment through "+
			"the helper so no sibling can be forgotten", n)
	}
	if i := strings.Index(code, "func (a *api) countProbe("); i < 0 {
		t.Error("countProbe is not defined in handlers.go")
	} else {
		// The one increment must sit inside the helper, not somewhere that
		// merely happens to be the only call site today.
		body := code[i:]
		if end := strings.Index(body, "\n}\n"); end >= 0 {
			body = body[:end]
		}
		if !strings.Contains(body, "a.candidateProbes.Add(") {
			t.Error("countProbe does not increment candidateProbes; the branches " +
				"calling it are counted in name only")
		}
	}
}

// ---------------------------------------------------------------------------
// R1-3 — the rename uniqueness gate must hold under concurrency.
// ---------------------------------------------------------------------------

// TestConcurrentRenamesOntoOneNameYieldExactlyOneWinner is the R1-3 vector.
//
// ★ THE DEFECT IS ONLY REACHABLE IN PARALLEL, which is why the sequential
// control below is a separate assertion rather than a substitute. Asking
// NameTaken and then calling RenameUser is correct every time it is run
// sequentially; the two calls take different locks, so N concurrent renames
// onto one free name each observe "free" and each succeed — 200 every time,
// and the roster holds N identities with the same name.
func TestConcurrentRenamesOntoOneNameYieldExactlyOneWinner(t *testing.T) {
	e := newEnv(t)

	const racers = 8
	clients := make([]*client, racers)
	for i := 0; i < racers; i++ {
		clients[i] = e.newClient("競爭者" + string(rune('A'+i)))
	}

	codes := make([]int, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = clients[i].do("POST", "/api/identity",
				map[string]any{"name": "唯一"}, nil)
		}(i)
	}
	close(start)
	wg.Wait()

	var ok, conflict, other int
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			other++
		}
	}
	if ok != 1 || conflict != racers-1 || other != 0 {
		t.Errorf("%d renames succeeded, %d were refused, %d returned something "+
			"else; want exactly 1 winner and %d conflicts",
			ok, conflict, other, racers-1)
	}

	// The roster is the claim that actually matters: the product requirement is
	// that the participant list shows each name once.
	var holders int
	for _, u := range e.st.Users() {
		if u.Name == "唯一" {
			holders++
		}
	}
	if holders != 1 {
		t.Errorf("%d identities now hold the name 唯一; the roster-uniqueness "+
			"invariant the suffix allocator enforces on the register path has "+
			"been undone through the rename door", holders)
	}
}

// TestSequentialRenameOntoATakenNameStillConflicts is the control group: the
// concurrent fix must not have been bought by breaking the ordinary path.
func TestSequentialRenameOntoATakenNameStillConflicts(t *testing.T) {
	e := newEnv(t)
	e.newClient("先來的")
	second := e.newClient("後到的")

	code := second.do("POST", "/api/identity", map[string]any{"name": "先來的"}, nil)
	if code != http.StatusConflict {
		t.Fatalf("sequential rename onto a taken name: status %d, want 409", code)
	}

	// Control on the control: a FREE name must still be accepted, or the
	// assertion above would also pass on a server that refuses every rename.
	if code := second.do("POST", "/api/identity",
		map[string]any{"name": "沒人用過的名字"}, nil); code != http.StatusOK {
		t.Fatalf("rename onto a free name: status %d, want 200 — the gate is "+
			"refusing everything, so the 409 above proves nothing", code)
	}
}

// TestRenameToOwnNameIsNotACollision pins the boundary the uniqueness check
// must not overrun: an idempotent re-submit of one's own name is not a clash
// with oneself.
func TestRenameToOwnNameIsNotACollision(t *testing.T) {
	e := newEnv(t)
	c := e.newClient("小明")
	if code := c.do("POST", "/api/identity", map[string]any{"name": "小明"}, nil); code != http.StatusOK {
		t.Errorf("renaming to one's own name: status %d, want 200", code)
	}
}

// TestStoreRenameUserIsAtomic asserts the same invariant one layer down, where
// the fix actually lives. The HTTP test above can be satisfied by a handler
// that serialises requests for some unrelated reason; this one cannot.
func TestStoreRenameUserIsAtomic(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	const racers = 8
	for i := 0; i < racers; i++ {
		uid := "U" + string(rune('A'+i))
		if err := st.CreateUser(uid, "hash-"+uid, "原名"+string(rune('A'+i))); err != nil {
			t.Fatalf("CreateUser %s: %v", uid, err)
		}
	}

	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = st.RenameUser("U"+string(rune('A'+i)), "唯一")
		}(i)
	}
	close(start)
	wg.Wait()

	var won int
	for _, err := range errs {
		if err == nil {
			won++
		}
	}
	if won != 1 {
		t.Errorf("%d concurrent RenameUser calls succeeded, want exactly 1", won)
	}

	var holders int
	for _, u := range st.Users() {
		if u.Name == "唯一" {
			holders++
		}
	}
	if holders != 1 {
		t.Errorf("%d users hold the name after the race, want 1", holders)
	}
}

// ---------------------------------------------------------------------------
// R1-4 — the wiring-guard comment must point at a file that exists.
// ---------------------------------------------------------------------------

// TestMailerCommentPointsAtTheRealGuard is the R1-4 check.
//
// ★ A COMMENT THAT NAMES A NON-EXISTENT FILE IS NOT COSMETIC. It sends the next
// reader looking for a guard where there is none; finding nothing, the honest
// conclusion available to them is "the guard was deleted", and the cheapest
// response to that is to write a second one. The guard is real — it just lives
// with the wiring it protects, in httpapi, because the line it asserts on is in
// httpapi.New.
func TestMailerCommentPointsAtTheRealGuard(t *testing.T) {
	src, err := os.ReadFile("../mailer/mailer.go")
	if err != nil {
		t.Fatalf("read mailer.go: %v", err)
	}
	code := string(src)

	if strings.Contains(code, "mailer_test.go") {
		t.Error("mailer.go still points at mailer_test.go, which does not " +
			"exist; the guard is TestMailerIsWiredIntoNew in " +
			"internal/httpapi/email_test.go")
	}

	// Control group: the file it now names must actually hold the guard, or
	// this test would accept a comment pointing somewhere equally wrong.
	guard, err := os.ReadFile("email_test.go")
	if err != nil {
		t.Fatalf("read email_test.go: %v", err)
	}
	if !strings.Contains(string(guard), "func TestMailerIsWiredIntoNew(") {
		t.Fatal("control group failed: email_test.go does not contain " +
			"TestMailerIsWiredIntoNew, so redirecting the comment there is " +
			"just a different wrong answer")
	}
	if !strings.Contains(code, "email_test.go") {
		t.Error("mailer.go does not name the file that holds the guard")
	}
}

// TestMailerTestFileIsStillAbsent is the other half: if someone later adds
// internal/mailer/mailer_test.go, the comment above should point back at it.
// This test failing is a prompt to re-check the pointer, not a defect.
func TestMailerTestFileIsStillAbsent(t *testing.T) {
	if _, err := os.Stat("../mailer/mailer_test.go"); err == nil {
		t.Error("internal/mailer/mailer_test.go now exists; re-check which " +
			"file mailer.go's wiring-guard comment should point at")
	}
}
