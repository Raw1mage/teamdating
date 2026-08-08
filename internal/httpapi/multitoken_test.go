package httpapi

import (
	"net/http"
	"testing"

	"teamdating/internal/store"
)

// HTTP-level vectors for the multi-token identity model.

// tokensOf reports how many bearer tokens a uid currently holds.
func tokensOf(t *testing.T, e *env, uid string) int {
	t.Helper()
	u, ok := e.st.User(uid)
	if !ok {
		t.Fatalf("uid %q not on the roster", uid)
	}
	return len(u.Tokens)
}

// TestNameReattachJoinsExistingIdentity is TV-3: a device with no cookie that
// submits an existing name joins THAT identity rather than minting a new one.
func TestNameReattachJoinsExistingIdentity(t *testing.T) {
	e := newEnv(t)
	first := e.newClient("小明")
	if got := tokensOf(t, e, first.UID); got != 1 {
		t.Fatalf("fresh identity holds %d tokens, want 1", got)
	}

	// A second device: same person, same name, no cookie of its own.
	second := e.anonClient()
	var out meResp
	if code := second.do("POST", "/api/register",
		map[string]any{"name": "小明"}, &out); code != http.StatusOK {
		t.Fatalf("reattach register: status %d", code)
	}

	if out.UID != first.UID {
		t.Fatalf("second device got uid %q, want the existing %q — a new "+
			"identity was minted, so this device sees none of the user's "+
			"proposals or votes", out.UID, first.UID)
	}
	if len(second.jar) == 0 {
		t.Fatal("no cookie was set for the second device")
	}
	if got := tokensOf(t, e, first.UID); got != 2 {
		t.Errorf("identity holds %d tokens after reattach, want 2", got)
	}

	// Decisive half: BOTH devices must now work. A replacing implementation
	// would leave this next call authenticating while the first device — the
	// one nobody is looking at — silently stopped.
	var me1, me2 meResp
	if code := first.do("GET", "/api/me", nil, &me1); code != http.StatusOK {
		t.Fatalf("first device /me: status %d", code)
	}
	if me1.UID != first.UID || !me1.Registered {
		t.Errorf("the FIRST device lost its identity after the second joined: "+
			"%+v", me1)
	}
	if code := second.do("GET", "/api/me", nil, &me2); code != http.StatusOK {
		t.Fatalf("second device /me: status %d", code)
	}
	if me2.UID != first.UID || !me2.Registered {
		t.Errorf("the second device does not resolve to the shared identity: "+
			"%+v", me2)
	}
}

// TestLoggedInRenameDoesNotAddAToken is TV-4, the boundary vector separating
// handleRegister's first branch from its second.
//
// ★ Nothing about the response distinguishes the two paths — both return 200
// with the new name. The only observable difference is the token COUNT, and
// only from inside the store. Under a bug that routed cookie-bearing requests
// into AddToken, every user-visible behaviour stays correct while each rename
// silently burns one of the caller's five device slots.
func TestLoggedInRenameDoesNotAddAToken(t *testing.T) {
	e := newEnv(t)
	c := e.newClient("小明")
	before := tokensOf(t, e, c.UID)

	var out meResp
	if code := c.do("POST", "/api/register",
		map[string]any{"name": "小華"}, &out); code != http.StatusOK {
		t.Fatalf("rename: status %d", code)
	}

	if out.UID != c.UID {
		t.Errorf("rename changed the uid: %q -> %q", c.UID, out.UID)
	}
	if out.Name != "小華" {
		t.Errorf("name = %q, want 小華", out.Name)
	}
	if after := tokensOf(t, e, c.UID); after != before {
		t.Errorf("token count %d -> %d across a RENAME; a rename must not "+
			"mint a token. Every response still looks correct, so only this "+
			"count reveals it", before, after)
	}
}

// TestReattachedIdentityKeepsItsProposals is the point of TV-3: the reattached
// device inherits the identity's actual state, not just its uid.
func TestReattachedIdentityKeepsItsProposals(t *testing.T) {
	e := newEnv(t)
	first := e.newClient("小明")
	// ★ Seeded through the store because this test is about the LEGACY reattach
	// path: an identity that holds an email answers a same-name login with
	// emailRequired (the merge must go through the magic link), so giving this
	// owner an address to satisfy the DD-7 proposal gate would retarget the
	// test onto a branch it is not about.
	pv := e.legacyProposal(first, "PTOKYO1", "東京六天")

	second := e.anonClient()
	var out meResp
	second.do("POST", "/api/register", map[string]any{"name": "小明"}, &out)

	var list struct {
		Proposals []proposalView `json:"proposals"`
	}
	if code := second.do("GET", "/api/my/proposals", nil, &list); code != http.StatusOK {
		t.Fatalf("my proposals: status %d", code)
	}
	if !containsPID(list.Proposals, pv.PID) {
		t.Errorf("the reattached device cannot see the identity's own "+
			"proposal %s; it was given a uid but not the identity", pv.PID)
	}
}

// TestSixthDeviceEvictsOverHTTP exercises the cap through the real endpoint.
//
// ★ IT ASSERTS HOW MANY SURVIVE, NOT WHICH ONE DIED — and that is a
// correctness requirement of the test, not a weaker assertion.
// LastSeenAt is an RFC3339 string with SECOND resolution, so six devices
// registered inside one second carry IDENTICAL stamps and the eviction falls
// through to the deterministic hash tie-break. That is inside DD-2's declared
// hour-grained approximation, but it means "the first device is the LRU" is
// simply false at this timescale. A test asserting it would fail for a reason
// that has nothing to do with the code under test.
//
// Ordering semantics belong to TV-6 (TestEvictionOrdersByLastSeenNotAdded),
// which controls the timestamps directly in the store package. What is
// verifiable HERE is the wiring: the cap holds, exactly one device loses its
// identity, and losing it is indistinguishable from never having had one.
func TestSixthDeviceEvictsOverHTTP(t *testing.T) {
	e := newEnv(t)
	first := e.newClient("小明")

	devices := []*client{first}
	for i := 0; i < store.MaxTokensPerUser; i++ {
		d := e.anonClient()
		var out meResp
		if code := d.do("POST", "/api/register",
			map[string]any{"name": "小明"}, &out); code != http.StatusOK {
			t.Fatalf("device %d register: status %d", i, code)
		}
		if out.UID != first.UID {
			t.Fatalf("device %d joined uid %q, want %q", i, out.UID, first.UID)
		}
		devices = append(devices, d)
	}

	// Six devices asked to join; the cap is five.
	if got := tokensOf(t, e, first.UID); got != store.MaxTokensPerUser {
		t.Fatalf("token count = %d after %d devices, want the cap %d",
			got, len(devices), store.MaxTokensPerUser)
	}

	alive, evicted := 0, 0
	for i, d := range devices {
		var me meResp
		// ★ An evicted device gets a normal 200 with registered:false, NOT an
		// error. The server cannot say "you were evicted" — that token no
		// longer exists, so the request is indistinguishable from a first-time
		// visitor's. That is exactly why the frontend has to remember locally
		// that it once had an identity (web/app.js, td_had_identity).
		if code := d.do("GET", "/api/me", nil, &me); code != http.StatusOK {
			t.Fatalf("device %d /me: status %d, want a normal 200", i, code)
		}
		switch {
		case me.Registered && me.UID == first.UID:
			alive++
		case !me.Registered && me.UID == "":
			evicted++
		default:
			t.Errorf("device %d is in neither state: %+v", i, me)
		}
	}

	if alive != store.MaxTokensPerUser {
		t.Errorf("%d devices still authenticate, want exactly the cap %d",
			alive, store.MaxTokensPerUser)
	}
	if evicted != 1 {
		t.Errorf("%d devices were evicted, want exactly 1 (six joined, cap is "+
			"%d). 0 would mean the cap never engaged; more than 1 would mean "+
			"eviction is over-firing", evicted, store.MaxTokensPerUser)
	}
}

// TestBodyUIDIsIgnored is TV-13: uid comes from the token and only from the
// token. This is a load-bearing security contract, not a nicety.
func TestBodyUIDIsIgnored(t *testing.T) {
	e := newEnv(t)
	victim := e.newClient("小明")
	attacker := e.newClient("小華")

	pv := attacker.createProposal("攻擊者的提案")
	withOption := attacker.addOption(pv.PID, "2026-03-01", "2026-03-03")
	if len(withOption.Options) == 0 {
		t.Fatal("no option to vote on")
	}
	oid := withOption.Options[0].OID

	// The attacker votes while claiming to be the victim in the body.
	code, _ := attacker.errCode("POST", "/api/proposals/"+pv.PID+"/votes",
		map[string]any{"optionId": oid, "weight": 1.0, "uid": victim.UID})

	// Either outcome is acceptable; a third is not.
	switch code {
	case http.StatusBadRequest:
		// DisallowUnknownFields refused the body outright.
	case http.StatusOK:
		// Accepted — but it must have been recorded as the ATTACKER.
		if e.st.HasVote(pv.PID, oid, victim.UID) {
			t.Fatal("a uid in the request body was honoured: the vote was " +
				"recorded against the victim. Anyone could then act as anyone")
		}
		if !e.st.HasVote(pv.PID, oid, attacker.UID) {
			t.Error("the vote was recorded against neither party")
		}
	default:
		t.Fatalf("unexpected status %d", code)
	}
}

// TestReattachedDevicesEachGetTheirOwnToken pins the one-key-per-device rule.
//
// ★ Serving the SAME token to two devices would work — both authenticate, both
// see the right data. The damage is invisible until eviction: two devices
// sharing one key share one LastSeenAt, so LRU cannot tell an abandoned phone
// from the laptop in daily use, and it starts evicting the wrong one.
func TestReattachedDevicesEachGetTheirOwnToken(t *testing.T) {
	e := newEnv(t)
	first := e.newClient("小明")

	second := e.anonClient()
	var out meResp
	second.do("POST", "/api/register", map[string]any{"name": "小明"}, &out)

	tokenOf := func(c *client) string {
		for _, ck := range c.jar {
			if ck.Name == cookieName && ck.Value != "" {
				return ck.Value
			}
		}
		return ""
	}
	a, b := tokenOf(first), tokenOf(second)
	if a == "" || b == "" {
		t.Fatalf("missing cookie: first=%q second=%q", a, b)
	}
	if a == b {
		t.Fatal("both devices carry the SAME token; their LastSeenAt is then " +
			"one shared value and LRU can no longer distinguish them")
	}
	u, _ := e.st.User(first.UID)
	if len(u.Tokens) != 2 {
		t.Errorf("identity holds %d tokens, want 2 distinct ones", len(u.Tokens))
	}
}

// TestRecoveryCodeMintsAFreshToken covers the same rule on the recovery path.
func TestRecoveryCodeMintsAFreshToken(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("小明")
	if owner.Recov == "" {
		t.Fatal("no recovery code was issued at registration")
	}

	other := e.anonClient()
	var out meResp
	if code := other.do("POST", "/api/register",
		map[string]any{"recovery": owner.Recov}, &out); code != http.StatusOK {
		t.Fatalf("recovery reattach: status %d", code)
	}
	if out.UID != owner.UID {
		t.Fatalf("recovery gave uid %q, want %q", out.UID, owner.UID)
	}
	if got := tokensOf(t, e, owner.UID); got != 2 {
		t.Errorf("token count = %d after a recovery reattach, want 2 — the "+
			"recovering device must get its OWN key, not a copy of the one "+
			"encoded in the code", got)
	}
	// The original device keeps working.
	var me meResp
	owner.do("GET", "/api/me", nil, &me)
	if me.UID != owner.UID || !me.Registered {
		t.Errorf("the original device lost its identity after a recovery "+
			"reattach: %+v", me)
	}
}
