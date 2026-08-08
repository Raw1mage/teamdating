package httpapi

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"teamdating/internal/auth"
	"teamdating/internal/store"
)

// readAppJS returns web/app.js. The frontend guards below read the SOURCE
// because the defect they catch is a missing line, and a missing line produces
// no error anywhere — the page simply stops updating.
func readAppJS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../web/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Owner-inheritance HTTP vectors. Every "refused / absent" assertion is paired
// with a control arm proving the check goes red when it should.
// ---------------------------------------------------------------------------

// forgeSameName mints a SECOND identity holding an existing name.
//
// ★ IT GOES THROUGH THE STORE, NOT /api/register, AND THAT IS THE POINT. The
// register path deliberately cannot produce this state — it routes a same-name
// caller to arbitration instead. The state IS reachable in production (two
// people register concurrently, or a legacy row predates the invariant), and it
// is exactly the state owner-inheritance exists to resolve, so the fixture has
// to build it directly.
func forgeSameName(t *testing.T, st *store.Store, uid, name string) *http.Cookie {
	t.Helper()
	plaintext, hash, err := auth.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := st.CreateUser(uid, hash, name); err != nil {
		t.Fatalf("CreateUser %s: %v", uid, err)
	}
	return &http.Cookie{Name: cookieName, Value: plaintext, Path: "/"}
}

// ---------------------------------------------------------------------------
// The challenge predicate — FOUR ARMS. Each false must arise for its OWN
// reason: a predicate that returns false because it never ran is
// indistinguishable from one that ran and cleared.
// ---------------------------------------------------------------------------

func TestOwnerTakeoverChallengeFourArms(t *testing.T) {
	owner := store.User{UID: "A", Name: "駱子逸", Email: "luo@ex.com"}
	ownerNoMail := store.User{UID: "A", Name: "駱子逸"}

	for _, tc := range []struct {
		name   string
		viewer identity
		owner  store.User
		want   bool
	}{
		// THE ONLY TRUE ARM.
		{"same name, different uid, owner has email",
			identity{UID: "B", Name: "駱子逸", Registered: true}, owner, true},
		// Same person looking at their own proposal — the common case. Without
		// the uid test this would challenge every owner forever.
		{"same uid is never challenged",
			identity{UID: "A", Name: "駱子逸", Registered: true}, owner, false},
		// DD-9 寬容: nothing to verify against, so no link can be sent.
		{"owner without email is not challenged",
			identity{UID: "B", Name: "駱子逸", Registered: true}, ownerNoMail, false},
		// Sharing nothing, claiming nothing.
		{"different name is not challenged",
			identity{UID: "B", Name: "另一個人", Registered: true}, owner, false},
		// No identity yet — no name, so no claim on the owner's.
		{"anonymous viewer is not challenged",
			identity{}, owner, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownerTakeoverChallenge(tc.viewer, tc.owner); got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

// TestOwnerTakeoverChallengeReachesTheWire proves the predicate is actually
// wired into the response, not merely correct in isolation.
//
// ★ THIS IS THE HALF A UNIT TEST CANNOT COVER. ownerTakeoverChallenge could be
// perfect and never called, and every table case above would still pass.
func TestOwnerTakeoverChallengeReachesTheWire(t *testing.T) {
	m := newMailEnv(t)
	owner := m.registerWithEmail("駱子逸", "luo@ex.com")
	p := owner.createProposal("華強北公模採購")

	// CONTROL ARM: the owner's own view carries no challenge. If the field were
	// true for everyone the assertion below would prove nothing.
	var ownerView proposalView
	if code := owner.do("GET", "/api/proposals/"+p.PID, nil, &ownerView); code != http.StatusOK {
		t.Fatalf("owner view: status %d", code)
	}
	if ownerView.OwnerTakeoverChallenge {
		t.Fatal("control: the owner must never be challenged on their own proposal")
	}

	// A second identity holding the same name.
	other := m.anonClient()
	other.jar = []*http.Cookie{forgeSameName(t, m.st, "B-uid", "駱子逸")}
	var otherView proposalView
	if code := other.do("GET", "/api/proposals/"+p.PID, nil, &otherView); code != http.StatusOK {
		t.Fatalf("other view: status %d", code)
	}
	if !otherView.OwnerTakeoverChallenge {
		t.Fatal("a same-name visitor must be challenged when the owner holds an email")
	}

	// CONTROL ARM: the challenge is a PROMPT, not a GATE (使用者裁示 Q4) — the
	// proposal stays fully readable. A blocking implementation would pass the
	// assertion above while breaking the product decision.
	if otherView.PID != p.PID || otherView.Title == "" {
		t.Fatalf("the proposal must remain readable while challenged, got %+v", otherView)
	}
}

// TestOwnerTakeoverMergeEndToEnd walks the whole Q2c flow over HTTP: a
// same-name visitor verifies by magic link and is absorbed into the owner.
func TestOwnerTakeoverMergeEndToEnd(t *testing.T) {
	m := newMailEnv(t)
	owner := m.registerWithEmail("駱子逸", "luo@ex.com")
	p := owner.createProposal("華強北公模採購")

	other := m.anonClient()
	other.jar = []*http.Cookie{forgeSameName(t, m.st, "B-uid", "駱子逸")}

	// The visitor votes BEFORE verifying — 使用者裁示 Q4 says those votes are
	// their own and must survive the merge rather than being discarded.
	pv := owner.addOption(p.PID, "2026-09-01", "2026-09-03")
	opt, ok := findOption(pv, "2026-09-01")
	if !ok {
		t.Fatalf("the option just created is not in the view: %+v", pv)
	}
	if code := other.do("POST", "/api/proposals/"+p.PID+"/votes",
		map[string]any{"optionId": opt.OID, "weight": 1}, nil); code != http.StatusOK {
		t.Fatalf("pre-merge vote: status %d", code)
	}

	// CONTROL ARM: two identities hold this one name right now, and the
	// visitor cannot yet manage the owner's proposal.
	if n := len(m.st.UsersByName("駱子逸")); n != 2 {
		t.Fatalf("control: expected 2 rows named 駱子逸 before the merge, got %d", n)
	}
	code, _ := other.errCode("PATCH", "/api/proposals/"+p.PID,
		map[string]any{"title": "改掉", "desc": ""})
	if code == http.StatusOK {
		t.Fatal("control: an unverified same-name visitor must not manage the proposal")
	}

	// Ask for the link, then click it.
	if code := other.do("POST", "/api/verify-email",
		map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, nil); code != http.StatusOK {
		t.Fatalf("verify-email: status %d", code)
	}
	tok := m.linkToken(t)
	codeC, confirmed := other.confirmMagicLink(tok)
	if codeC != http.StatusOK {
		t.Fatalf("verify confirm: status %d", codeC)
	}

	// The visitor IS the owner now — same uid, not a transferred proposal.
	if confirmed.UID != owner.UID {
		t.Fatalf("after verification the device should be the owner identity %q, got %q",
			owner.UID, confirmed.UID)
	}
	// The absorbed row left the roster, so the displayed name is unique again.
	if n := len(m.st.UsersByName("駱子逸")); n != 1 {
		t.Fatalf("exactly one 駱子逸 must remain after the merge, got %d", n)
	}
	if _, ok := m.st.User("B-uid"); ok {
		t.Fatal("the absorbed identity must be gone from the roster")
	}
	// Management now works — the change the merge was supposed to bring.
	if code := other.do("PATCH", "/api/proposals/"+p.PID,
		map[string]any{"title": "改好了", "desc": ""}, nil); code != http.StatusOK {
		t.Fatalf("after the merge the device must manage the proposal, status %d", code)
	}
	// The pre-merge vote moved rather than vanishing, and did not double up.
	pr, _ := m.st.Proposal(p.PID)
	if len(pr.Votes) != 1 {
		t.Fatalf("one person must hold one vote, got %d rows", len(pr.Votes))
	}
	for _, v := range pr.Votes {
		if v.UID != owner.UID {
			t.Fatalf("the surviving vote should belong to %q, got %q", owner.UID, v.UID)
		}
	}
}

// TestEmailTakenOverHTTP pins Q5 at the boundary: a collision is a 409 with its
// own code, never a 500.
func TestEmailTakenOverHTTP(t *testing.T) {
	m := newMailEnv(t)
	m.registerWithEmail("甲", "shared@ex.com")
	second := m.newClient("乙")

	code, ec := second.errCode("POST", "/api/identity",
		map[string]any{"email": "shared@ex.com"})
	if code != http.StatusConflict {
		t.Fatalf("want 409 for a held address, got %d (code %q)", code, ec)
	}
	if ec != codeEmailTaken {
		t.Fatalf("want %s, got %q", codeEmailTaken, ec)
	}

	// CONTROL ARM 1: a FREE address succeeds through the same endpoint, so the
	// 409 above is collision-specific rather than the endpoint being broken.
	var out meResp
	if c := second.do("POST", "/api/identity",
		map[string]any{"email": "free@ex.com"}, &out); c != http.StatusOK {
		t.Fatalf("control: an unheld address must be accepted, got %d", c)
	}
	if out.Email != "free@ex.com" {
		t.Fatalf("control: identity should report the new address, got %q", out.Email)
	}
	// CONTROL ARM 2: re-submitting the SAME address succeeds. The gear form
	// resends unchanged values on every save, so a naive check fails only on
	// the second save — invisible to a single-save test.
	if c := second.do("POST", "/api/identity",
		map[string]any{"email": "free@ex.com"}, &out); c != http.StatusOK {
		t.Fatalf("control: re-setting one's own address must succeed, got %d", c)
	}
}

// TestEmailTakenOnRegisterPath covers the OTHER writer. Two call sites reaching
// one store error answered differently is precisely the drift the shared
// helper exists to prevent, and only a test per site can show they agree.
func TestEmailTakenOnRegisterPath(t *testing.T) {
	m := newMailEnv(t)
	m.registerWithEmail("甲", "shared@ex.com")
	second := m.newClient("乙")

	code, ec := second.errCode("POST", "/api/register",
		map[string]any{"name": "乙", "email": "shared@ex.com"})
	if code != http.StatusConflict || ec != codeEmailTaken {
		t.Fatalf("register path must answer 409/%s, got %d/%q", codeEmailTaken, code, ec)
	}
	// CONTROL ARM: the same request with a free address is accepted.
	var out meResp
	if c := second.do("POST", "/api/register",
		map[string]any{"name": "乙", "email": "own@ex.com"}, &out); c != http.StatusOK {
		t.Fatalf("control: a free address on the register path must succeed, got %d", c)
	}
}

// ---------------------------------------------------------------------------
// Frontend source guards. These read web/app.js because the defect they catch
// is a MISSING line, and a missing line produces no error anywhere — the page
// simply stops updating. Same rationale as the existing SERVER_EVENTS guard.
// ---------------------------------------------------------------------------

func TestGearVisibilityIsPerItem(t *testing.T) {
	code := stripGoComments(readAppJS(t))

	// The old whole-menu binding must be gone: it hid 身分設定 from every
	// non-owner, which is the bug this change exists to fix.
	if strings.Contains(code, "gear-wrap').hidden = p.ownerUid !== myUid()") {
		t.Fatal("the gear menu is still bound wholesale to ownership")
	}
	// CONTROL: the guard can see real content — if this fails the assertion
	// above passed only because the file was unreadable or over-stripped.
	if !strings.Contains(code, "renderGear") {
		t.Fatal("control: renderGear not found, so the guard is not reading app.js properly")
	}
	// Owner-only items must still be gated, or a non-owner sees a delete
	// button the server will refuse — a UI lie, not a security hole.
	for _, id := range []string{"b-repid", "b-del-proposal"} {
		if !strings.Contains(code, "setItem('"+id+"', isOwner)") {
			t.Fatalf("%s must remain owner-only", id)
		}
	}
	for _, id := range []string{"me-identity", "me-recovery"} {
		if !strings.Contains(code, "setItem('"+id+"', !!me)") {
			t.Fatalf("%s must be reachable by any logged-in identity", id)
		}
	}
}

func TestFrontendHandlesUserMergeEvent(t *testing.T) {
	code := stripGoComments(readAppJS(t))
	if !strings.Contains(code, "'user_merge'") {
		t.Fatal("SERVER_EVENTS must carry user_merge: the merge changes the roster, " +
			"the votes and the ownership, and an unhandled event updates nothing " +
			"while reporting no error")
	}
	// CONTROL: the guard genuinely inspects the event list.
	if !strings.Contains(code, "'user_set_email'") {
		t.Fatal("control: user_set_email missing too, so this guard is not reading the list")
	}
}
