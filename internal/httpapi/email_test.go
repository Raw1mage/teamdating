package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"teamdating/internal/store"
)

// HTTP-level vectors for identity_access-control-email-auth, mapped to
// test-vectors.json. Every "rejects" vector is paired with the control arm the
// plan names (TV-*C), because a refusal that is really an unconditional
// `return error` satisfies the negative half on its own.

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// fakeSender records what would have been mailed and can be told to fail.
//
// ★ THE FAILURE MODE IS THE POINT. A real relay cannot be made to refuse on
// demand, so without this TV-10 ("SMTP_SEND_FAILED must refuse the binding")
// could never be executed — and an implementation that bound the device anyway
// would look identical to a correct one in every other test.
type fakeSender struct {
	mu     sync.Mutex
	fail   error
	sent   []sentMail
	closed bool
}

type sentMail struct{ to, subject, body string }

func (f *fakeSender) Send(to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, sentMail{to, subject, body})
	return nil
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeSender) last() (sentMail, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return sentMail{}, false
	}
	return f.sent[len(f.sent)-1], true
}

// mailEnv is env plus the injected sender and the api handle (for the clock).
type mailEnv struct {
	*env
	mail *fakeSender
	api  *api
}

func newMailEnv(t *testing.T) *mailEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fakeSender{}
	h, a := newWithSender(st, emptyFS{}, "test", "", "", f)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &mailEnv{
		env:  &env{t: t, srv: srv, st: st, dir: dir},
		mail: f,
		api:  a,
	}
}

// newMailEnvPublicURL is newMailEnv with an operator-declared canonical origin.
//
// ★ A SEPARATE CONSTRUCTOR RATHER THAN A FIELD MUTATED AFTERWARDS. verifyLink
// reads a.publicURL, and a test that set it post-construction would also pass
// against an implementation that read the flag at request time from somewhere
// else — this one exercises the same wiring path production uses.
func newMailEnvPublicURL(t *testing.T, publicURL string) *mailEnv {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fakeSender{}
	h, a := newWithSender(st, emptyFS{}, "test", "", publicURL, f)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &mailEnv{
		env:  &env{t: t, srv: srv, st: st, dir: dir},
		mail: f,
		api:  a,
	}
}

// linkToken extracts the ?t= value from the most recent mail. Reading the token
// out of the MAIL rather than out of the server's memory is deliberate: it is
// the only path a real user has, so a link that is malformed or never actually
// carried the token fails here instead of passing on an internal shortcut.
func (m *mailEnv) linkToken(t *testing.T) string {
	t.Helper()
	sent, ok := m.mail.last()
	if !ok {
		t.Fatal("no mail was sent, so there is no link to follow")
	}
	i := strings.Index(sent.body, "?t=")
	if i < 0 {
		t.Fatalf("the mail body carries no ?t= link:\n%s", sent.body)
	}
	tok := sent.body[i+3:]
	if j := strings.IndexAny(tok, "\r\n "); j >= 0 {
		tok = tok[:j]
	}
	if tok == "" {
		t.Fatal("the link carries an empty token")
	}
	return tok
}

// confirmMagicLink follows the link the way a mail client would, then asks the
// server WHO this device now is.
//
// ★ WHY THE SECOND REQUEST. The confirm response deliberately no longer
// carries a uid or a recovery code (confirmResp): this path renders in a
// browser, and the recovery code it used to return is a standing credential
// that reattaches an identity through /api/register. The ASSERTION every
// caller of this helper makes — "the device is now the intended identity" — is
// unchanged; only where the answer is read from moved. Reading it from /api/me
// is in fact the stronger form: it derives the identity from the cookie that
// was actually set, so a confirm that returned the right uid in its body while
// setting the wrong cookie (or none) now fails here instead of passing.
func (c *client) confirmMagicLink(tok string) (int, meResp) {
	c.t.Helper()
	var cr confirmResp
	code := c.do("GET", "/api/verify-email/confirm?t="+tok, nil, &cr)
	if code != http.StatusOK {
		return code, meResp{}
	}
	if !cr.Verified || cr.Code != codeVerifyOK {
		c.t.Fatalf("confirm answered 200 but did not report success: %+v", cr)
	}
	var me meResp
	if code := c.do("GET", "/api/me", nil, &me); code != http.StatusOK {
		c.t.Fatalf("after confirm, /api/me: status %d", code)
	}
	return http.StatusOK, me
}

// registerWithEmail creates an identity that already holds a verified-shaped
// email, the starting state for the merge vectors.
func (m *mailEnv) registerWithEmail(name, email string) *client {
	m.t.Helper()
	c := m.newClient(name)
	var out meResp
	if code := c.do("POST", "/api/identity",
		map[string]any{"name": name, "email": email}, &out); code != http.StatusOK {
		m.t.Fatalf("set email for %s: status %d", name, code)
	}
	if out.Email != email {
		m.t.Fatalf("identity reports email %q, want %q", out.Email, email)
	}
	return c
}

// ---------------------------------------------------------------------------
// TV-2 / TV-2C — same name + same email, verified, merges; control: before the
// merge that device has NO management right.
// ---------------------------------------------------------------------------

func TestTV2MergeGrantsCrossDeviceManagement(t *testing.T) {
	m := newMailEnv(t)
	owner := m.registerWithEmail("駱子逸", "luo@ex.com")
	p := owner.createProposal("華強北公模採購")

	// --- TV-2C CONTROL ARM (runs FIRST, on purpose) ------------------------
	// Before the merge, the second device must NOT be able to manage the
	// owner's proposal. Without this arm the post-merge assertion proves
	// nothing: if management were granted to everyone, the merge test would
	// pass while doing no work at all.
	second := m.anonClient()
	code, ec := second.errCode("PATCH", "/api/proposals/"+p.PID,
		map[string]any{"title": "被冒名改掉", "desc": ""})
	if code == http.StatusOK {
		t.Fatal("TV-2C control arm FAILED: an unmerged device could already edit " +
			"the proposal, so the merge assertion below would be vacuously true")
	}
	_ = ec

	// --- TV-2 ---------------------------------------------------------------
	var reg map[string]any
	if code := second.do("POST", "/api/register",
		map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, &reg); code != http.StatusOK {
		t.Fatalf("same-name register: status %d", code)
	}
	if reg["needEmailVerify"] != true {
		t.Fatalf("same-name + same-email login did not route to verification: %+v", reg)
	}
	// ★ Nothing may be bound yet. The response above is an instruction to go
	// read mail, not an authorisation — a device bound here would make the mail
	// decorative.
	var meBefore meResp
	second.do("GET", "/api/me", nil, &meBefore)
	if meBefore.UID == owner.UID {
		t.Fatal("the device was bound BEFORE the link was clicked — the magic " +
			"link is then decorative and anyone who knows a name can merge")
	}

	if code := second.do("POST", "/api/verify-email",
		map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, nil); code != http.StatusOK {
		t.Fatalf("verify-email: status %d", code)
	}
	if m.mail.count() != 1 {
		t.Fatalf("mails sent: %d, want 1", m.mail.count())
	}

	tok := m.linkToken(t)
	code2, confirmed := second.confirmMagicLink(tok)
	if code2 != http.StatusOK {
		t.Fatalf("confirm: status %d", code2)
	}
	if confirmed.UID != owner.UID {
		t.Fatalf("confirm bound to uid %q, want the existing %q", confirmed.UID, owner.UID)
	}

	// The identity now holds two tokens (the merge is an AddToken, DD-2).
	u, _ := m.st.User(owner.UID)
	if len(u.Tokens) != 2 {
		t.Errorf("identity holds %d tokens after merge, want 2", len(u.Tokens))
	}

	// The decisive half: management right actually crossed to this device.
	var pv proposalView
	if code := second.do("PATCH", "/api/proposals/"+p.PID,
		map[string]any{"title": "改好了", "desc": ""}, &pv); code != http.StatusOK {
		t.Fatalf("after merge the second device still cannot manage the proposal: status %d", code)
	}
	if pv.Title != "改好了" {
		t.Errorf("title is %q after the managed edit", pv.Title)
	}
	// And the FIRST device must still work — a merge that replaced tokens
	// would log the original device out silently.
	var me1 meResp
	if code := owner.do("GET", "/api/me", nil, &me1); code != http.StatusOK || me1.UID != owner.UID {
		t.Errorf("the original device lost its identity after the merge: %+v", me1)
	}
}

// ---------------------------------------------------------------------------
// TV-3 / TV-3C — expiry refuses; control: the same token, unexpired, binds.
// ---------------------------------------------------------------------------

func TestTV3ExpiredLinkRefusedAndControlBinds(t *testing.T) {
	// --- TV-3: expired -----------------------------------------------------
	t.Run("TV-3 expired refuses", func(t *testing.T) {
		m := newMailEnv(t)
		owner := m.registerWithEmail("駱子逸", "luo@ex.com")
		second := m.anonClient()
		if code := second.do("POST", "/api/verify-email",
			map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, nil); code != http.StatusOK {
			t.Fatalf("verify-email: status %d", code)
		}
		tok := m.linkToken(t)

		// Advance the pending table's clock past the TTL. Sleeping fifteen real
		// minutes is not an option, and without this seam the expiry branch is
		// never executed by anything.
		m.api.pending.now = func() time.Time {
			return time.Now().Add(pendingTTLForTest() + time.Minute)
		}

		code, ec := second.errCode("GET", "/api/verify-email/confirm?t="+tok, nil)
		if ec != codeVerifyTokenExpired {
			t.Fatalf("expired link returned status=%d code=%q, want %s",
				code, ec, codeVerifyTokenExpired)
		}
		// Nothing may have been bound.
		u, _ := m.st.User(owner.UID)
		if len(u.Tokens) != 1 {
			t.Errorf("identity holds %d tokens after a REFUSED confirm, want 1 — "+
				"the refusal reported an error and bound the device anyway", len(u.Tokens))
		}
	})

	// --- TV-3C CONTROL ARM: unexpired binds --------------------------------
	// Proves the expiry check actually discriminates. Without it, a confirm
	// handler that refused every token would pass the vector above.
	t.Run("TV-3C unexpired binds", func(t *testing.T) {
		m := newMailEnv(t)
		owner := m.registerWithEmail("駱子逸", "luo@ex.com")
		second := m.anonClient()
		if code := second.do("POST", "/api/verify-email",
			map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, nil); code != http.StatusOK {
			t.Fatalf("verify-email: status %d", code)
		}
		tok := m.linkToken(t)

		code, out := second.confirmMagicLink(tok)
		if code != http.StatusOK {
			t.Fatalf("TV-3C control arm FAILED: an unexpired link was refused "+
				"(status %d), so the expiry rejection above proves nothing", code)
		}
		if out.UID != owner.UID {
			t.Errorf("control arm bound to %q, want %q", out.UID, owner.UID)
		}
	})
}

// ---------------------------------------------------------------------------
// TV-9 / TV-9C — one-time use; control: the first use succeeds.
// ---------------------------------------------------------------------------

func TestTV9LinkIsSingleUse(t *testing.T) {
	m := newMailEnv(t)
	owner := m.registerWithEmail("駱子逸", "luo@ex.com")
	second := m.anonClient()
	if code := second.do("POST", "/api/verify-email",
		map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, nil); code != http.StatusOK {
		t.Fatalf("verify-email: status %d", code)
	}
	tok := m.linkToken(t)

	// TV-9C CONTROL ARM: the FIRST use must succeed.
	code1, first := second.confirmMagicLink(tok)
	if code1 != http.StatusOK {
		t.Fatalf("TV-9C control arm FAILED: the first use of a fresh link was "+
			"refused (status %d), so the second-use rejection proves nothing", code1)
	}
	if first.UID != owner.UID {
		t.Fatalf("first use bound to %q, want %q", first.UID, owner.UID)
	}

	// TV-9: the second use is refused, and refused DISTINGUISHABLY — "already
	// used" tells the user the link did work, so they stop suspecting the
	// service; "invalid" would send them to request link after link.
	third := m.anonClient()
	code, ec := third.errCode("GET", "/api/verify-email/confirm?t="+tok, nil)
	if ec != codeVerifyTokenUsed {
		t.Fatalf("re-used link returned status=%d code=%q, want %s", code, ec, codeVerifyTokenUsed)
	}
	// And it granted nothing: the identity still holds exactly the two tokens
	// from the original device plus the first (legitimate) merge.
	u, _ := m.st.User(owner.UID)
	if len(u.Tokens) != 2 {
		t.Errorf("identity holds %d tokens after a refused re-use, want 2", len(u.Tokens))
	}
}

// TestUnknownTokenIsInvalid keeps the third state distinct from the other two.
func TestUnknownTokenIsInvalid(t *testing.T) {
	m := newMailEnv(t)
	m.registerWithEmail("駱子逸", "luo@ex.com")
	c := m.anonClient()
	_, ec := c.errCode("GET", "/api/verify-email/confirm?t=THIS-TOKEN-NEVER-EXISTED", nil)
	if ec != codeVerifyTokenInvalid {
		t.Errorf("a forged token returned %q, want %s", ec, codeVerifyTokenInvalid)
	}
}

// ---------------------------------------------------------------------------
// TV-10 / TV-10C — send failure is fail-loud; control: success enters pending.
// ---------------------------------------------------------------------------

func TestTV10SendFailureRefusesAndControlProceeds(t *testing.T) {
	m := newMailEnv(t)
	owner := m.registerWithEmail("駱子逸", "luo@ex.com")

	// --- TV-10: the relay refuses ------------------------------------------
	m.mail.fail = &net0pErr{}
	second := m.anonClient()
	code, ec := second.errCode("POST", "/api/verify-email",
		map[string]any{"name": "駱子逸", "email": "luo@ex.com"})
	if ec != codeSMTPSendFailed {
		t.Fatalf("failed send returned status=%d code=%q, want %s", code, ec, codeSMTPSendFailed)
	}
	// ★ No pending entry may exist. A live link left behind by a failed send is
	// a credential nobody was told about.
	if n := m.api.pending.pendingCount(); n != 0 {
		t.Errorf("%d pending entries after a FAILED send, want 0 — a magic link "+
			"was minted for a mail that never went out", n)
	}
	u, _ := m.st.User(owner.UID)
	if len(u.Tokens) != 1 {
		t.Errorf("identity holds %d tokens after a failed send, want 1 — the "+
			"binding was allowed to proceed despite fail-loud", len(u.Tokens))
	}

	// --- TV-10C CONTROL ARM: with the relay working, it proceeds ------------
	// Proves the refusal above is about the send and not an unconditional
	// rejection of this endpoint.
	m.mail.fail = nil
	if code := second.do("POST", "/api/verify-email",
		map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, nil); code != http.StatusOK {
		t.Fatalf("TV-10C control arm FAILED: a working relay still returned %d", code)
	}
	if n := m.api.pending.pendingCount(); n != 1 {
		t.Errorf("control arm: %d pending entries after a SUCCESSFUL send, want 1", n)
	}
	if m.mail.count() != 1 {
		t.Errorf("control arm: %d mails sent, want 1", m.mail.count())
	}
	sent, _ := m.mail.last()
	if sent.to != "luo@ex.com" {
		t.Errorf("mail addressed to %q, want luo@ex.com", sent.to)
	}
	if !strings.Contains(sent.body, "/api/verify-email/confirm?t=") {
		t.Errorf("the mail body carries no confirm link:\n%s", sent.body)
	}
}

// net0pErr is a stand-in transport failure.
type net0pErr struct{}

func (*net0pErr) Error() string { return "simulated relay refusal" }

// ---------------------------------------------------------------------------
// TV-8 / TV-8C — the proposal email gate; control: with an email it passes.
// ---------------------------------------------------------------------------

func TestTV8ProposalRequiresEmail(t *testing.T) {
	m := newMailEnv(t)

	// --- TV-8: no email → refused, and NO proposal is created --------------
	c := m.newClient("沒填信箱的人")
	before := m.st.ProposalCount()
	code, ec := c.errCode("POST", "/api/proposals",
		map[string]any{"title": "秋季日本團", "desc": ""})
	if ec != codeEmailRequiredForProposal {
		t.Fatalf("proposal without email returned status=%d code=%q, want %s",
			code, ec, codeEmailRequiredForProposal)
	}
	if got := m.st.ProposalCount(); got != before {
		t.Errorf("proposal count went %d→%d despite the refusal — the gate "+
			"reported an error and created the proposal anyway", before, got)
	}

	// --- TV-8C CONTROL ARM: same identity, email set → it goes through ------
	// Without this the gate could be refusing everyone and the vector above
	// would still pass.
	var idOut meResp
	if code := c.do("POST", "/api/identity",
		map[string]any{"name": "沒填信箱的人", "email": "now@ex.com"}, &idOut); code != http.StatusOK {
		t.Fatalf("setting the email: status %d", code)
	}
	var pv proposalView
	if code := c.do("POST", "/api/proposals",
		map[string]any{"title": "秋季日本團", "desc": ""}, &pv); code != http.StatusOK {
		t.Fatalf("TV-8C control arm FAILED: with an email set the proposal was "+
			"still refused (status %d), so the gate is not discriminating", code)
	}
	if pv.PID == "" {
		t.Error("control arm: proposal created but no pid returned")
	}
	if got := m.st.ProposalCount(); got != before+1 {
		t.Errorf("control arm: proposal count %d, want %d", got, before+1)
	}
}

// ---------------------------------------------------------------------------
// TV-4 / TV-5 / TV-6C — genuinely different person with the same name.
// ---------------------------------------------------------------------------

// TestTV4RenameChoiceCreatesNothing is GRAFCET step 6→7: reporting the
// collision must not mutate anything, so the user can still choose.
func TestTV4CollisionIsReportedWithoutMutating(t *testing.T) {
	m := newMailEnv(t)
	m.registerWithEmail("駱子逸", "luo@ex.com")
	usersBefore := m.st.UserCount()

	other := m.anonClient()
	var out map[string]any
	if code := other.do("POST", "/api/register",
		map[string]any{"name": "駱子逸", "email": "other@ex.com"}, &out); code != http.StatusOK {
		t.Fatalf("collision register: status %d", code)
	}
	if out["nameCollision"] != true {
		t.Fatalf("a non-matching email did not report a collision: %+v", out)
	}
	if got := m.st.UserCount(); got != usersBefore {
		t.Errorf("user count went %d→%d while merely REPORTING a collision — "+
			"the user was renamed or an account was minted before they chose",
			usersBefore, got)
	}
	if m.mail.count() != 0 {
		t.Errorf("%d mails sent on the collision path, want 0 — mailing a "+
			"stranger every time someone typos is spam", m.mail.count())
	}

	// Step 7: the user retypes a different name and it goes through cleanly.
	var made meResp
	if code := other.do("POST", "/api/register",
		map[string]any{"name": "駱子逸B", "email": "other@ex.com"}, &made); code != http.StatusOK {
		t.Fatalf("retyped name: status %d", code)
	}
	if made.Name != "駱子逸B" {
		t.Errorf("retyped registration produced name %q", made.Name)
	}
	assertNamesUnique(t, m.st)
}

// TestTV5SuffixChoiceAllocatesLowestFree covers TV-5 and its TV-6C control.
func TestTV5SuffixChoiceAllocatesLowestFree(t *testing.T) {
	// TV-5: one holder → the newcomer gets -1.
	t.Run("TV-5 first suffix", func(t *testing.T) {
		m := newMailEnv(t)
		m.registerWithEmail("駱子逸", "luo@ex.com")
		other := m.anonClient()
		var out meResp
		if code := other.do("POST", "/api/register", map[string]any{
			"name": "駱子逸", "email": "other@ex.com", "choice": "suffix",
		}, &out); code != http.StatusOK {
			t.Fatalf("suffix choice: status %d", code)
		}
		if out.Name != "駱子逸-1" {
			t.Errorf("allocated %q, want 駱子逸-1", out.Name)
		}
		if out.Email != "other@ex.com" {
			t.Errorf("the suffixed account holds email %q, want other@ex.com", out.Email)
		}
		assertNamesUnique(t, m.st)
	})

	// TV-6C CONTROL ARM: with -1 taken but -2 free, the allocator must choose
	// -2 — separating "lowest free" from "count+1" and from a fixed bump.
	t.Run("TV-6C lowest free not a fixed bump", func(t *testing.T) {
		m := newMailEnv(t)
		m.registerWithEmail("駱子逸", "luo@ex.com")
		first := m.anonClient()
		var a meResp
		first.do("POST", "/api/register", map[string]any{
			"name": "駱子逸", "email": "a@ex.com", "choice": "suffix",
		}, &a)
		if a.Name != "駱子逸-1" {
			t.Fatalf("setup: first suffix is %q, want 駱子逸-1", a.Name)
		}
		second := m.anonClient()
		var b meResp
		second.do("POST", "/api/register", map[string]any{
			"name": "駱子逸", "email": "b@ex.com", "choice": "suffix",
		}, &b)
		if b.Name != "駱子逸-2" {
			t.Errorf("allocated %q, want 駱子逸-2", b.Name)
		}
		assertNamesUnique(t, m.st)
	})
}

// assertNamesUnique is the invariant the whole A4 branch exists to hold.
func assertNamesUnique(t *testing.T, st *store.Store) {
	t.Helper()
	seen := map[string]string{}
	for _, u := range st.Users() {
		if prev, dup := seen[u.Name]; dup {
			t.Errorf("roster shows name %q on both %q and %q — the frontend "+
				"list can no longer distinguish two people", u.Name, prev, u.UID)
		}
		seen[u.Name] = u.UID
	}
}

// ---------------------------------------------------------------------------
// TV-7 — DD-9 claim of an email-less legacy account (knowingly weak).
// ---------------------------------------------------------------------------

func TestTV7LegacyAccountClaim(t *testing.T) {
	m := newMailEnv(t)
	// A legacy identity: name only, no email — the pre-plan shape.
	legacy := m.newClient("駱子逸")
	p := legacy.createProposalWithEmailBypass(t, m, "舊帳號的提案")

	claimer := m.anonClient()
	var reg map[string]any
	if code := claimer.do("POST", "/api/register",
		map[string]any{"name": "駱子逸", "email": "claimer@ex.com"}, &reg); code != http.StatusOK {
		t.Fatalf("claim register: status %d", code)
	}
	if reg["needEmailVerify"] != true {
		t.Fatalf("claiming an email-less account did not route to verification: %+v", reg)
	}
	if code := claimer.do("POST", "/api/verify-email",
		map[string]any{"name": "駱子逸", "email": "claimer@ex.com"}, nil); code != http.StatusOK {
		t.Fatalf("verify-email: status %d", code)
	}
	tok := m.linkToken(t)
	codeC, out := claimer.confirmMagicLink(tok)
	if codeC != http.StatusOK {
		t.Fatalf("confirm: status %d", codeC)
	}
	if out.UID != legacy.UID {
		t.Fatalf("claim bound to %q, want the legacy %q", out.UID, legacy.UID)
	}
	// The claimed account now HOLDS that email, which is what closes the hole
	// after exactly one claim rather than leaving it open forever.
	u, _ := m.st.User(legacy.UID)
	if u.Email != "claimer@ex.com" {
		t.Errorf("claimed account holds email %q, want claimer@ex.com — without "+
			"this the same account stays claimable by the next person who "+
			"knows the name", u.Email)
	}
	// The claimer really did gain management (that is the point of the claim).
	var pv proposalView
	if code := claimer.do("PATCH", "/api/proposals/"+p.PID,
		map[string]any{"title": "認領後改標題", "desc": ""}, &pv); code != http.StatusOK {
		t.Errorf("after the claim the device cannot manage the account's proposal: %d", code)
	}

	// A SECOND claimer must now be refused the weak path: the account has an
	// email, so only someone who can read THAT mailbox may merge.
	stranger := m.anonClient()
	var reg2 map[string]any
	stranger.do("POST", "/api/register",
		map[string]any{"name": "駱子逸", "email": "stranger@ex.com"}, &reg2)
	if reg2["needEmailVerify"] == true {
		t.Error("a second stranger was still offered the merge path — the DD-9 " +
			"hole must close once the account holds an address")
	}
}

// createProposalWithEmailBypass creates a proposal for a legacy identity whose
// email is deliberately unset.
//
// ★ It writes through the STORE, not the HTTP gate, and that is the honest way
// to build this fixture: the gate correctly refuses an email-less owner (TV-8),
// so a pre-plan account holding a proposal cannot be produced through the API
// at all. Setting an email first would destroy the very condition under test.
func (c *client) createProposalWithEmailBypass(t *testing.T, m *mailEnv, title string) proposalView {
	t.Helper()
	pid := "LEGACYPID" + title[:0]
	if err := m.st.CreateProposal("LEGACYPID1", c.UID, title, ""); err != nil {
		t.Fatalf("seed legacy proposal: %v", err)
	}
	_ = pid
	return proposalView{PID: "LEGACYPID1", OwnerUID: c.UID, Title: title}
}

// ---------------------------------------------------------------------------
// DD-8 — the load-bearing security contract.
// ---------------------------------------------------------------------------

// TestDD8VerifyFlowNeverReadsUIDFromBody is a wiring guard on the source.
//
// ★ A BEHAVIOURAL test cannot catch this. If a handler accepted a uid from the
// body, every legitimate request (which never sends one) would still behave
// exactly the same — the hole is only reachable by an attacker who knows to
// try it, so no ordinary test would ever execute the broken path. The
// assertion therefore has to be about the code.
func TestDD8VerifyFlowNeverReadsUIDFromBody(t *testing.T) {
	src, err := os.ReadFile("verify.go")
	if err != nil {
		t.Fatalf("read verify.go: %v", err)
	}
	code := stripGoComments(string(src))

	// Control group: prove the matcher can fire at all. A checker that finds
	// nothing in a string known to contain the needle is broken, and its clean
	// verdict on the real file would mean nothing.
	if !strings.Contains(stripGoComments("type x struct { UID string `json:\"uid\"` }"), "UID string") {
		t.Fatal("control group failed: the matcher cannot detect a known-bad shape")
	}

	for _, needle := range []string{
		`json:"uid"`,
		`json:"uid,omitempty"`,
	} {
		if strings.Contains(code, needle) {
			t.Errorf("verify.go declares a body field %s — the uid on this flow "+
				"must come from the server-side pending entry only (DD-8); a "+
				"caller-supplied uid lets anyone bind their device to any "+
				"identity while every normal request still works", needle)
		}
	}
}

// TestMailerIsWiredIntoNew is the other wiring guard.
//
// ★ Every behavioural test above injects a fake, so the ONE line that
// constructs the real transport is executed by nothing in the suite. That is
// the classic injection blind spot: a service that mails nowhere would pass
// all of the above. This reads the source to prove the production path exists.
func TestMailerIsWiredIntoNew(t *testing.T) {
	src, err := os.ReadFile("httpapi.go")
	if err != nil {
		t.Fatalf("read httpapi.go: %v", err)
	}
	code := stripGoComments(string(src))
	if !strings.Contains(code, "mailer.NewSMTPSender()") {
		t.Error("New() does not construct the real SMTP sender — every test " +
			"here runs against a fake, so nothing else would notice a service " +
			"that never mails anything")
	}
	if !strings.Contains(code, "newWithSender(") {
		t.Error("the injection seam is gone; TV-10's failure branch becomes unreachable")
	}
}

// NOTE: stripGoComments lives in httpapi_test.go — the source-reading guards
// above reuse it rather than declaring a second copy, so a fix to the stripper
// cannot apply to one guard and not the other.
