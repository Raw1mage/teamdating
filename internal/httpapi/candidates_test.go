package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"teamdating/internal/auth"
)

// Same-name candidate selection (round 3).
//
// ★ THE LIVE FAILURE THIS ENCODES. Name-reattach shipped assuming every
// identity sharing a name was an equivalent target. On live data one person
// already held TWO uids, created before reattach existed: one owned a
// proposal, the other held only votes. Map iteration is randomised, so a new
// device reattaching by name landed on the empty identity half the time — and
// the user saw an app with none of their work in it, with no error logged
// anywhere. The bug was never in the token model; it was in treating identities
// as interchangeable when the CONTENT hanging off them is not.

// splitIdentity registers a second, independent identity with the SAME display
// name directly through the store — the way live data acquired them, before
// name-reattach existed to prevent it.
func splitIdentity(t *testing.T, e *env, uid, tokenSeed, name string) {
	t.Helper()
	if err := e.st.CreateUser(uid, auth.HashToken(tokenSeed), name); err != nil {
		t.Fatalf("CreateUser %s: %v", uid, err)
	}
}

// candidatesResp is the multi-candidate shape of POST /api/register.
type candidatesResp struct {
	Candidates []struct {
		UID       string `json:"uid"`
		Name      string `json:"name"`
		Proposals int    `json:"proposals"`
		CreatedAt string `json:"createdAt"`
	} `json:"candidates"`
	// The direct-attach fields, so one decode can tell the two shapes apart.
	UID        string `json:"uid"`
	Registered bool   `json:"registered"`
}

// TestSameNameOffersCandidatesInsteadOfGuessing is the primary vector.
func TestSameNameOffersCandidatesInsteadOfGuessing(t *testing.T) {
	e := newEnv(t)

	// The identity that owns the work.
	//
	// ★ ITS PROPOSAL IS SEEDED THROUGH THE STORE, and that is load-bearing for
	// this test rather than a shortcut past the DD-7 gate: an owner holding an
	// email answers the same-name split with emailRequired, so the candidate
	// list under test would never be reached. The condition IS an email-less
	// legacy identity.
	owner := e.newClient("駱子逸")
	e.legacyProposal(owner, "PLEGACY1", "華強北公模採購")

	// The same person's OTHER identity, from before reattach existed.
	splitIdentity(t, e, "UEMPTY", "empty-seed", "駱子逸")

	var out candidatesResp
	code := e.anonClient().do("POST", "/api/register",
		map[string]any{"name": "駱子逸"}, &out)
	if code != http.StatusOK {
		t.Fatalf("register: status %d", code)
	}

	if len(out.Candidates) != 2 {
		t.Fatalf("got %d candidates, want 2 — with two same-name identities on "+
			"the roster the endpoint must not pick one; picking is exactly the "+
			"coin flip that logged a real user into an empty account",
			len(out.Candidates))
	}
	// A candidate list must NOT also attach. Doing both would hand out a token
	// for a guessed identity and then ask which one the user wanted.
	if out.UID != "" || out.Registered {
		t.Errorf("the candidate response also attached an identity (uid=%q "+
			"registered=%v)", out.UID, out.Registered)
	}

	var withWork, withoutWork int
	for _, c := range out.Candidates {
		switch c.UID {
		case owner.UID:
			withWork++
			if c.Proposals != 1 {
				t.Errorf("the owning identity reports %d proposals, want 1",
					c.Proposals)
			}
			// CreatedAt is the tie-breaker when counts cannot decide, so it
			// must actually be present.
			if c.CreatedAt == "" {
				t.Error("candidate carries no createdAt — with equal proposal " +
					"counts the two rows would render identically and the user " +
					"would be picking blind")
			}
		case "UEMPTY":
			withoutWork++
			if c.Proposals != 0 {
				t.Errorf("the empty identity reports %d proposals, want 0",
					c.Proposals)
			}
		default:
			t.Errorf("unexpected candidate uid %q", c.UID)
		}
		if c.Name != "駱子逸" {
			t.Errorf("candidate %q has name %q", c.UID, c.Name)
		}
	}
	if withWork != 1 || withoutWork != 1 {
		t.Errorf("candidate set is wrong: withWork=%d withoutWork=%d",
			withWork, withoutWork)
	}
}

// TestSingleSameNameStillAttachesDirectly is the CONTROL GROUP for the vector
// above.
//
// ★ Without this, a broken implementation that returned a candidate list for
// EVERY reattach would pass the multi-candidate test and still be a regression:
// the common case (one identity, one device joining) would have gained a
// pointless "which of these 1 identities is you?" prompt. This proves the
// branch actually discriminates on the candidate COUNT rather than always
// taking the new path.
func TestSingleSameNameStillAttachesDirectly(t *testing.T) {
	e := newEnv(t)
	first := e.newClient("小明")

	var out candidatesResp
	second := e.anonClient()
	code := second.do("POST", "/api/register", map[string]any{"name": "小明"}, &out)
	if code != http.StatusOK {
		t.Fatalf("register: status %d", code)
	}

	if len(out.Candidates) != 0 {
		t.Fatalf("a candidate list was returned for a name with exactly ONE "+
			"identity (%d candidates) — the user would be asked to choose "+
			"between one option", len(out.Candidates))
	}
	if out.UID != first.UID || !out.Registered {
		t.Fatalf("direct attach did not happen: uid=%q registered=%v, want %q",
			out.UID, out.Registered, first.UID)
	}
	if len(second.jar) == 0 {
		t.Error("no cookie was set, so the device did not actually join")
	}
	if got := tokensOf(t, e, first.UID); got != 2 {
		t.Errorf("identity holds %d tokens, want 2", got)
	}
}

// TestPickedCandidateJoinsThatExactIdentity proves the pick is honoured — the
// half that makes the feature work at all.
func TestPickedCandidateJoinsThatExactIdentity(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("駱子逸")
	e.legacyProposal(owner, "PLEGACY2", "華強北公模採購")
	splitIdentity(t, e, "UEMPTY", "empty-seed", "駱子逸")

	var out meResp
	device := e.anonClient()
	code := device.do("POST", "/api/register",
		map[string]any{"name": "駱子逸", "uid": owner.UID}, &out)
	if code != http.StatusOK {
		t.Fatalf("picked register: status %d", code)
	}
	if out.UID != owner.UID {
		t.Fatalf("joined %q, want the PICKED %q", out.UID, owner.UID)
	}
	if got := tokensOf(t, e, owner.UID); got != 2 {
		t.Errorf("picked identity holds %d tokens, want 2", got)
	}
	if got := tokensOf(t, e, "UEMPTY"); got != 1 {
		t.Errorf("the identity that was NOT picked gained a token (%d, want 1)",
			got)
	}

	// Decisive half: the device must now SEE the work it picked. A pick that
	// attached to the right uid but left the device unable to load the
	// proposal would be the original symptom with extra steps.
	var mine struct {
		Proposals []proposalView `json:"proposals"`
	}
	if code := device.do("GET", "/api/my/proposals", nil, &mine); code != http.StatusOK {
		t.Fatalf("my proposals: status %d", code)
	}
	if len(mine.Proposals) != 1 || mine.Proposals[0].Title != "華強北公模採購" {
		t.Errorf("the joined device sees %d proposals (%+v), want the one it "+
			"picked the identity FOR", len(mine.Proposals), mine.Proposals)
	}
}

// TestPickedUIDMustBelongToTheSubmittedName is the security vector for the one
// place a uid may arrive in a request body.
//
// ★ DD-8 is about AUTHENTICATION and is untouched here: identify() still reads
// the cookie and only the cookie. The risk this closes is different and would
// be invisible in any response — if the pick were honoured without checking it
// against the name's candidate set, the privilege would silently widen from
// "knowing a name lets you join it" (the accepted collision risk) to "knowing
// any uid lets you join anything".
func TestPickedUIDMustBelongToTheSubmittedName(t *testing.T) {
	e := newEnv(t)
	victim := e.newClient("小明")
	victim.createProposal("被害者的提案")
	// Two identities named 小華 so the attacker's request reaches the pick
	// branch rather than short-circuiting elsewhere.
	e.newClient("小華")
	splitIdentity(t, e, "UHUA2", "hua2-seed", "小華")

	var out candidatesResp
	attacker := e.anonClient()
	code := attacker.do("POST", "/api/register",
		map[string]any{"name": "小華", "uid": victim.UID}, &out)

	if code == http.StatusOK && out.UID == victim.UID {
		t.Fatal("a uid from the request body was honoured for a DIFFERENT " +
			"name: anyone who learns a uid could join that identity, which is " +
			"strictly more than the name-collision risk the product accepted")
	}
	if code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for a uid outside the name's candidate set",
			code)
	}
	if got := tokensOf(t, e, victim.UID); got != 1 {
		t.Errorf("the victim identity gained a token (%d, want 1)", got)
	}
	if len(attacker.jar) != 0 {
		t.Error("a cookie was issued for a rejected pick")
	}
}

// TestCookieBearingRequestNeverSeesCandidates keeps the rename branch above the
// reattach branch (the must-stay-above contract in handleRegister).
//
// ★ Under a regression that consulted candidates first, a logged-in user
// renaming themselves to a name someone else already uses would be shown that
// stranger's proposal titles — a disclosure that no test asserting response
// CORRECTNESS would catch, because the rename itself would still succeed.
func TestCookieBearingRequestNeverSeesCandidates(t *testing.T) {
	e := newEnv(t)
	stranger := e.newClient("駱子逸")
	e.legacyProposal(stranger, "PLEGACY3", "華強北公模採購")
	splitIdentity(t, e, "UEMPTY", "empty-seed", "駱子逸")

	// CONTROL ARM FIRST. Without it the assertion below is satisfied by any
	// response that happens to carry no candidates — including one from a
	// server that stopped returning candidates to ANYBODY, which would be a
	// different regression wearing this test's green tick. This proves the
	// candidate branch is live for this exact name right now.
	var anon candidatesResp
	if code := e.anonClient().do("POST", "/api/register",
		map[string]any{"name": "駱子逸"}, &anon); code != http.StatusOK {
		t.Fatalf("control: anonymous register: status %d", code)
	}
	if len(anon.Candidates) != 2 {
		t.Fatalf("control: an anonymous caller got %d candidates, want 2 — the "+
			"branch this test is about is not being taken, so the cookie-bearing "+
			"result below proves nothing", len(anon.Candidates))
	}

	me := e.newClient("小明")
	var out candidatesResp
	code := me.do("POST", "/api/register", map[string]any{"name": "駱子逸"}, &out)
	// ★ 409, NOT 200, SINCE THE ROSTER-UNIQUENESS RULE LANDED. Renaming onto a
	// name someone else already holds is now refused (the same invariant the
	// suffix allocator enforces on the register path); before that rule this
	// call succeeded and produced a second 駱子逸 on the roster.
	//
	// The DISCLOSURE claim this test exists for is unchanged and is in fact
	// stronger here: whatever the status, a cookie-bearing caller must not be
	// handed the candidate list — the refusal must not become a new door to the
	// same leak.
	if code != http.StatusConflict {
		t.Fatalf("cookie-bearing rename onto a taken name: status %d, want 409", code)
	}
	if len(out.Candidates) != 0 {
		t.Fatalf("a cookie-bearing rename was shown %d candidates — including "+
			"another user's proposal titles", len(out.Candidates))
	}

	// The refused rename must leave the caller exactly as it was: same identity,
	// same name, and no token burned.
	var meNow meResp
	if c := me.do("GET", "/api/me", nil, &meNow); c != http.StatusOK {
		t.Fatalf("read own identity: status %d", c)
	}
	if meNow.UID != me.UID {
		t.Errorf("the refused rename changed identity: uid=%q, want %q", meNow.UID, me.UID)
	}
	if meNow.Name != "小明" {
		t.Errorf("the refused rename still renamed: name=%q, want 小明", meNow.Name)
	}
	if got := tokensOf(t, e, me.UID); got != 1 {
		t.Errorf("the rename burned a token slot (%d tokens, want 1)", got)
	}
}

// TestCandidateListNeverCarriesProposalTitles is the guard for the disclosure
// boundary, asserted on the RAW response body rather than a typed struct.
//
// ★ A TYPED DECODE CANNOT SEE THIS REGRESSION. Adding a Titles field back to
// candidateView would be invisible to every test above — encoding/json simply
// ignores response fields the test's struct does not declare, so all of them
// stay green while the endpoint hands out a stranger's proposal titles. The
// only way to test for a field that MUST NOT EXIST is to look at the bytes.
//
// ★ WHY IT MUST NOT EXIST, restated where the next person will hit it: the
// old behaviour gave someone who mistyped a name a coin flip between two
// identities. A title tells them which one holds the work, converting a random
// collision into a guided one — the self-healing feature would then have made
// the accepted name-collision risk STRICTLY WORSE. A count is the minimum that
// lets the rightful owner decide; a title adds nothing for them and everything
// for an impostor.
func TestCandidateListNeverCarriesProposalTitles(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("駱子逸")
	e.legacyProposal(owner, "PLEGACY4", "華強北公模採購")
	splitIdentity(t, e, "UEMPTY", "empty-seed", "駱子逸")

	var raw json.RawMessage
	if code := e.anonClient().do("POST", "/api/register",
		map[string]any{"name": "駱子逸"}, &raw); code != http.StatusOK {
		t.Fatalf("register: status %d", code)
	}
	body := string(raw)

	// Control arm FIRST: prove this body is the candidate list at all. Without
	// it, a response that changed shape entirely would pass the negative
	// assertion below by containing nothing.
	if !strings.Contains(body, "candidates") || !strings.Contains(body, "proposals") {
		t.Fatalf("this is not a candidate response, so the absence of a title "+
			"proves nothing: %s", body)
	}
	if strings.Contains(body, "華強北公模採購") {
		t.Errorf("a proposal title reached the candidate list: %s\n"+
			"Anyone typing this name now sees WHICH identity holds the work "+
			"and can take that one deliberately — worse than the coin flip "+
			"this feature replaced", body)
	}
	if strings.Contains(body, "titles") {
		t.Errorf("the response carries a titles field: %s", body)
	}
}

// TestFrontendReportsProposalCountAfterJoining is the detectability guard
// (constraint 3): a user who picks the wrong identity must be able to notice.
//
// ★ IT MUST MEASURE, NOT ECHO. Reporting the count the candidate list already
// showed would agree with reality whenever the pick was right and be wrong in
// exactly the case it exists to catch — it describes the row the user TAPPED,
// not the identity they actually joined. So the frontend re-queries
// /my/proposals after joining. This is a source guard because the difference
// between the two implementations is invisible in any passing scenario.
func TestFrontendReportsProposalCountAfterJoining(t *testing.T) {
	src, err := os.ReadFile("../../web/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	app := string(src)

	idx := strings.Index(app, "async function joinIdentity")
	if idx < 0 {
		t.Fatal("joinIdentity is gone from app.js; the candidate flow has no " +
			"join path, or it was renamed and this guard has silently stopped " +
			"checking anything")
	}
	body := app[idx:]
	if end := strings.Index(body[1:], "\nasync function "); end > 0 {
		body = body[:end]
	}

	if !strings.Contains(body, "/my/proposals") {
		t.Error("joinIdentity does not re-query /my/proposals after joining. " +
			"Without a MEASURED count, a user who picked the wrong identity " +
			"sees a success toast and an empty app, with nothing telling them " +
			"which of the two happened")
	}
	// Control arm: prove the extracted region is really joinIdentity's body and
	// the search above could have failed.
	if !strings.Contains(body, "register") {
		t.Errorf("the extracted region does not look like joinIdentity, so the "+
			"assertion above had no discriminating power: %.200s", body)
	}
}

// TestCandidateSummaryOmitsProposalsMerelyVotedIn bounds what the list reveals.
//
// ★ ProposalsForUser returns proposals the uid OWNS *or voted in*. Summarising
// with it would print a third party's proposal title to whoever types a name —
// someone who never took part in this reattach at all. candidateViews filters
// to owned; this is the vector that makes the difference observable, because
// both versions return a correct-looking list.
func TestCandidateSummaryOmitsProposalsMerelyVotedIn(t *testing.T) {
	e := newEnv(t)
	thirdParty := e.newClient("第三人")
	pv := thirdParty.createProposal("第三人的私人提案")
	withOption := thirdParty.addOption(pv.PID, "2026-03-01", "2026-03-03")
	if len(withOption.Options) == 0 {
		t.Fatal("no option to vote on")
	}

	voter := e.newClient("駱子逸")
	if code := voter.do("POST", "/api/proposals/"+pv.PID+"/votes",
		map[string]any{"optionId": withOption.Options[0].OID, "weight": 1.0},
		nil); code != http.StatusOK {
		t.Fatalf("vote: status %d", code)
	}
	splitIdentity(t, e, "UEMPTY", "empty-seed", "駱子逸")

	var out candidatesResp
	if code := e.anonClient().do("POST", "/api/register",
		map[string]any{"name": "駱子逸"}, &out); code != http.StatusOK {
		t.Fatalf("register: status %d", code)
	}
	if len(out.Candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(out.Candidates))
	}
	for _, c := range out.Candidates {
		if c.Proposals != 0 {
			t.Errorf("candidate %q reports %d proposals; it OWNS none and only "+
				"voted in someone else's — a third party's work was counted "+
				"into a reattach they never took part in", c.UID, c.Proposals)
		}
	}
}

// healthProbeCount reads the candidate-probe counter out of /api/health.
func healthProbeCount(t *testing.T, e *env) uint64 {
	t.Helper()
	var h struct {
		CandidateProbes uint64 `json:"candidateProbesTotal"`
	}
	if code := e.anonClient().do("GET", "/api/health", nil, &h); code != http.StatusOK {
		t.Fatalf("health: status %d", code)
	}
	return h.CandidateProbes
}

// TestCandidateProbesAreCounted closes the one branch of this endpoint that an
// unauthenticated caller can reach WITHOUT changing any state.
//
// ★ WHY THIS IS NOT PARANOIA. Every other anonymous outcome of /api/register
// ends in a store mutation — mint, attach, rename — so the event log records
// that it happened and an operator can see it later. The multi-candidate branch
// only reads. Before this counter, seven probes of a two-candidate name grew
// the event log by exactly zero lines, so "nobody ever asked about this name"
// and "somebody enumerated it all week" were the same empty log. That is the
// absence/failure conflation R8 avoids structurally elsewhere by having no
// listing route at all; here the route has to exist, so the trace has to.
//
// ★ THIS ASSERTS THE COUNTER MOVES, NOT THAT THE FIELD EXISTS. A test that
// only decoded candidateProbesTotal would pass forever against a counter that
// is never incremented — the exact defect being fixed, wearing the shape of
// its own fix.
func TestCandidateProbesAreCounted(t *testing.T) {
	e := newEnv(t)

	owner := e.newClient("駱子逸")
	e.legacyProposal(owner, "PLEGACY5", "華強北公模採購")
	splitIdentity(t, e, "UEMPTY", "empty-seed", "駱子逸")

	before := healthProbeCount(t, e)

	const probes = 7
	for i := 0; i < probes; i++ {
		var out candidatesResp
		if code := e.anonClient().do("POST", "/api/register",
			map[string]any{"name": "駱子逸"}, &out); code != http.StatusOK {
			t.Fatalf("probe %d: status %d", i, code)
		}
		if len(out.Candidates) != 2 {
			t.Fatalf("probe %d: got %d candidates, want 2 — this test only "+
				"means anything while the multi-candidate branch is the one "+
				"being taken", i, len(out.Candidates))
		}
	}

	after := healthProbeCount(t, e)
	if got := after - before; got != probes {
		t.Errorf("candidateProbesTotal rose by %d after %d anonymous "+
			"multi-candidate probes, want %d — the one request shape that "+
			"reads this service without changing it is invisible again",
			got, probes, probes)
	}
}

// TestNonProbeRegistrationsDoNotInflateTheCounter is the control group for the
// test above: it proves the counter tracks the multi-candidate branch
// specifically, not "requests to /api/register".
//
// ★ A counter that also ticked on ordinary registration would still make the
// assertion above pass, and would be useless for the thing it exists to show —
// a climbing number would no longer mean somebody is enumerating a name.
func TestNonProbeRegistrationsDoNotInflateTheCounter(t *testing.T) {
	e := newEnv(t)

	before := healthProbeCount(t, e)

	// A brand-new name: mints an identity, no candidates involved.
	e.newClient("新來的人")
	// An existing single-candidate name: attaches directly, still no list.
	e.newClient("獨一無二")
	second := e.anonClient()
	var out candidatesResp
	if code := second.do("POST", "/api/register",
		map[string]any{"name": "獨一無二"}, &out); code != http.StatusOK {
		t.Fatalf("single-candidate attach: status %d", code)
	}
	if len(out.Candidates) != 0 {
		t.Fatalf("single same-name should attach directly, got %d candidates",
			len(out.Candidates))
	}

	if got := healthProbeCount(t, e) - before; got != 0 {
		t.Errorf("candidateProbesTotal rose by %d on registrations that never "+
			"returned a candidate list; the counter is measuring traffic, not "+
			"probes, so a rising number would no longer mean anything", got)
	}
}
