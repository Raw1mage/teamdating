package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// rawResp is the UNPARSED answer.
//
// ★ The existing client.do decodes into a struct, which is exactly the wrong
// instrument for these tests: a field that should not be there is invisible
// once it has been decoded into a type that has no place to put it, and an
// HTML body would decode as a JSON error rather than as itself. These
// assertions are about the bytes on the wire, so they read the bytes.
type rawResp struct {
	status       int
	body         string
	contentType  string
	cacheControl string
}

// raw issues the request with NO Accept header — the API-default case.
//
// ★ It takes no body on purpose: every caller here is a GET, and a parameter
// that is accepted and silently discarded is how a test comes to believe it
// sent something it did not.
func (c *client) raw(method, path string) rawResp {
	c.t.Helper()
	return c.rawAccept(method, path, "")
}

func (c *client) rawAccept(method, path, accept string) rawResp {
	c.t.Helper()
	req, err := http.NewRequest(method, c.srv.URL+path, bytes.NewReader(nil))
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	for _, ck := range c.jar {
		req.AddCookie(ck)
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if cs := resp.Cookies(); len(cs) > 0 {
		c.jar = cs
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return rawResp{
		status:       resp.StatusCode,
		body:         string(b),
		contentType:  resp.Header.Get("Content-Type"),
		cacheControl: resp.Header.Get("Cache-Control"),
	}
}

// doWithHeaders is client.do with extra request headers, for the forged
// forwarded-header arms.
func (c *client) doWithHeaders(method, path string, body any, hdr map[string]string) int {
	c.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.srv.URL+path, rdr)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, ck := range c.jar {
		req.AddCookie(ck)
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if cs := resp.Cookies(); len(cs) > 0 {
		c.jar = cs
	}
	return resp.StatusCode
}

// Tests for the four defects measured on the live magic-link flow, each with
// the control arm that proves the assertion is not vacuously true.

// ---------------------------------------------------------------------------
// ① The recovery code must not ride the confirm response.
// ---------------------------------------------------------------------------

// TestConfirmNeverReturnsRecoveryCode is the security half.
//
// ★ WHY THE CONTROL ARM IS THE WHOLE TEST. "the response contains no recovery
// code" is satisfied by deleting the field everywhere, by breaking the endpoint,
// by returning an empty body — an absence proves nothing on its own. The arm
// below shows the register path STILL mints and returns one, so what is being
// asserted is "this path stopped leaking it", not "the feature is gone".
//
// Measured on live: the confirm page rendered
//
//	{"uid":"4QUE…","name":"駱子逸","email":"…","recovery":"GNH3-UGBF-…"}
//
// in a browser the user reached from their mail client. RecoveryCode(token)
// reattaches an identity through /api/register (handlers.go reattach), so it is
// a standing password — displayed on a screen, kept in history, screenshotted.
// Nothing consumed it: web/app.js never calls this endpoint at all.
func TestConfirmNeverReturnsRecoveryCode(t *testing.T) {
	m := newMailEnv(t)
	owner := m.registerWithEmail("駱子逸", "luo@ex.com")

	// --- CONTROL ARM FIRST: the register path DOES return a recovery code ---
	// Without this, blanking Recovery across the whole codebase would pass the
	// assertion below while destroying the recovery feature.
	fresh := m.anonClient()
	var reg meResp
	if code := fresh.do("POST", "/api/register",
		map[string]any{"name": "另一個人"}, &reg); code != http.StatusOK {
		t.Fatalf("control arm: register status %d", code)
	}
	if strings.TrimSpace(reg.Recovery) == "" {
		t.Fatal("CONTROL ARM FAILED: /api/register no longer returns a recovery " +
			"code, so the absence asserted below would merely mean the field was " +
			"deleted everywhere rather than removed from the leaking path")
	}

	// --- The vector: confirm's body carries no recovery code ---------------
	second := m.anonClient()
	if code := second.do("POST", "/api/verify-email",
		map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, nil); code != http.StatusOK {
		t.Fatalf("verify-email: status %d", code)
	}
	tok := m.linkToken(t)

	raw := second.raw("GET", "/api/verify-email/confirm?t="+tok)
	if raw.status != http.StatusOK {
		t.Fatalf("confirm: status %d", raw.status)
	}
	// The recovery code is a rendering of the token, so assert on BOTH shapes:
	// the field name and the value that would appear if some other field
	// carried it.
	if strings.Contains(raw.body, "recovery") {
		t.Errorf("the confirm response mentions a recovery field:\n%s", raw.body)
	}
	// And no uid: a machine caller that wants to know who it is asks /api/me
	// with the cookie it was just handed.
	if strings.Contains(raw.body, owner.UID) {
		t.Errorf("the confirm response carries the uid %q:\n%s", owner.UID, raw.body)
	}

	// The binding itself still happened — otherwise "leaks nothing" would be
	// satisfied by an endpoint that does nothing.
	var me meResp
	if code := second.do("GET", "/api/me", nil, &me); code != http.StatusOK || me.UID != owner.UID {
		t.Fatalf("after a clean confirm the device is %+v, want uid %q", me, owner.UID)
	}
}

// TestConfirmResponseTypeCannotCarryARecoveryCode pins the fix STRUCTURALLY.
//
// ★ The behavioural test above passes the moment a handler stops setting the
// field; this one fails if the field can exist at all. The difference matters
// because the leak was introduced by writing `meResp` — a struct that is
// entirely correct for /api/me — at a handler whose output goes to a browser.
// A response type that cannot express the secret makes the mistake unavailable
// rather than merely unmade.
func TestConfirmResponseTypeCannotCarryARecoveryCode(t *testing.T) {
	src, err := os.ReadFile("verify.go")
	if err != nil {
		t.Fatalf("read verify.go: %v", err)
	}
	code := stripGoComments(string(src))

	// ★ SCOPED TO handleVerifyConfirm'S BODY, not the whole file. handleIdentity
	// lives here too and answers the GEAR MODAL — a caller asking about itself
	// over XHR — where meResp is exactly right. A file-wide matcher would report
	// that legitimate use as the leak (it did, on the first run of this test),
	// and a guard that cries wolf gets an exemption bolted on rather than being
	// fixed. The claim is about ONE handler, so the matcher looks at one handler.
	bodyOf := func(src, name string) string {
		start := strings.Index(src, "func (a *api) "+name+"(")
		if start < 0 {
			return ""
		}
		rest := src[start:]
		// The body ends where the next top-level func begins.
		if end := strings.Index(rest[1:], "\nfunc "); end >= 0 {
			return rest[:end+1]
		}
		return rest
	}

	confirm := bodyOf(code, "handleVerifyConfirm")
	if confirm == "" {
		// A parse that found nothing would make every assertion below pass on an
		// empty string — the absent/failure collapse this whole file is about.
		t.Fatal("handleVerifyConfirm not found in verify.go: it was renamed or " +
			"removed, and this guard is measuring nothing")
	}

	// Control group: the extractor really does isolate ONE function — the
	// legitimate meResp in handleIdentity must be OUTSIDE what it returned. If
	// the extractor silently grabbed the rest of the file, the assertions below
	// would fire on a use that is correct.
	if ident := bodyOf(code, "handleIdentity"); ident == "" || !strings.Contains(ident, "meResp{") {
		t.Fatal("control group failed: handleIdentity's body was not isolated or no " +
			"longer builds a meResp, so this extractor's verdict on handleVerifyConfirm " +
			"cannot be trusted")
	}
	if strings.Contains(confirm, "meResp{") && strings.Contains(confirm, "handleIdentity") {
		t.Fatal("control group failed: the extractor swallowed handleIdentity into " +
			"handleVerifyConfirm's body")
	}

	if strings.Contains(confirm, "auth.RecoveryCode") {
		t.Error("handleVerifyConfirm calls auth.RecoveryCode — this path renders in " +
			"a BROWSER the user reached from their mail client, so a recovery code " +
			"returned here is a standing credential displayed on screen and kept in " +
			"history. Nothing reads it: web/app.js never calls this endpoint")
	}
	if strings.Contains(confirm, "meResp{") {
		t.Error("handleVerifyConfirm constructs a meResp; that type carries Recovery " +
			"and Email, and using it here is exactly how the leak arrived. Use " +
			"confirmResp, whose fields cannot express a credential")
	}
}

// ---------------------------------------------------------------------------
// ② The browser path must get a page, not raw JSON.
// ---------------------------------------------------------------------------

// TestConfirmRendersAPageForBrowsers covers all four branches plus the control
// that each one is distinguishable.
func TestConfirmRendersAPageForBrowsers(t *testing.T) {
	// --- success -----------------------------------------------------------
	t.Run("success page", func(t *testing.T) {
		m := newMailEnv(t)
		owner := m.registerWithEmail("駱子逸", "luo@ex.com")
		second := m.anonClient()
		if code := second.do("POST", "/api/verify-email",
			map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, nil); code != http.StatusOK {
			t.Fatalf("verify-email: status %d", code)
		}
		tok := m.linkToken(t)

		raw := second.rawAccept("GET", "/api/verify-email/confirm?t="+tok,
			"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		if raw.status != http.StatusOK {
			t.Fatalf("confirm: status %d", raw.status)
		}
		if ct := raw.contentType; !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("a browser Accept got Content-Type %q, want text/html", ct)
		}
		if !strings.Contains(raw.body, "<!DOCTYPE html>") {
			t.Errorf("the answer is not a document:\n%s", raw.body)
		}
		// ★ The page must SAY it succeeded. Without this a blank shell would
		// satisfy "returns HTML" while telling the user nothing — which is the
		// same information content as the JSON it replaced.
		if !strings.Contains(raw.body, "認證完成") {
			t.Errorf("the success page carries no success message:\n%s", raw.body)
		}
		// ★ It names WHICH identity was joined. A page saying only "done" leaves
		// a user with two same-name accounts unable to tell which one they merged
		// into — and the name is already in the mail they clicked from, so it
		// discloses nothing new.
		if !strings.Contains(raw.body, "駱子逸") {
			t.Errorf("the success page does not say which identity was joined:\n%s", raw.body)
		}
		// And it must leak neither the credential nor the identifier.
		for _, forbidden := range []string{"recovery", owner.UID, "uid"} {
			if strings.Contains(raw.body, forbidden) {
				t.Errorf("the success page contains %q:\n%s", forbidden, raw.body)
			}
		}
		// ★ CONTROL for the name assertion: the FAILURE pages must NOT carry a
		// name. They are reached by whoever holds a spent or forged link — which
		// may be a mail scanner or a stranger — and telling them whose account the
		// link pointed at would make the endpoint an oracle on (link → person).
		// Without this arm, hardcoding the name into the template would satisfy
		// the assertion above while leaking on every refused attempt.
		stranger := m.anonClient().rawAccept("GET",
			"/api/verify-email/confirm?t=THIS-TOKEN-NEVER-EXISTED", "text/html")
		if strings.Contains(stranger.body, "駱子逸") {
			t.Errorf("a REFUSED link's page names an identity — whoever holds a "+
				"spent or forged link learns whose account it pointed at:\n%s", stranger.body)
		}
		// A single-use result must not be cached and re-shown to the next
		// person who opens the same URL.
		if cc := raw.cacheControl; !strings.Contains(cc, "no-store") {
			t.Errorf("Cache-Control is %q, want no-store", cc)
		}
	})

	// --- the three failure branches, each distinguishable ------------------
	// ★ CONTROL: the four branches must not share one page. If they did, a
	// user whose link expired would be told the same thing as one who followed
	// a forged link, and "the service is broken" would be the only available
	// conclusion for both.
	t.Run("failure pages are distinguishable", func(t *testing.T) {
		browser := "text/html,*/*;q=0.8"

		// expired
		mExp := newMailEnv(t)
		mExp.registerWithEmail("駱子逸", "luo@ex.com")
		cExp := mExp.anonClient()
		cExp.do("POST", "/api/verify-email",
			map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, nil)
		tokExp := mExp.linkToken(t)
		mExp.api.pending.now = func() time.Time {
			return time.Now().Add(pendingTTLForTest() + time.Minute)
		}
		expired := cExp.rawAccept("GET", "/api/verify-email/confirm?t="+tokExp, browser)

		// used
		mUsed := newMailEnv(t)
		mUsed.registerWithEmail("駱子逸", "luo@ex.com")
		cUsed := mUsed.anonClient()
		cUsed.do("POST", "/api/verify-email",
			map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, nil)
		tokUsed := mUsed.linkToken(t)
		if code, _ := cUsed.confirmMagicLink(tokUsed); code != http.StatusOK {
			t.Fatalf("control: the first use of a fresh link failed with %d, so "+
				"the used-branch below would not be exercising what it claims", code)
		}
		used := mUsed.anonClient().rawAccept("GET", "/api/verify-email/confirm?t="+tokUsed, browser)

		// invalid
		mInv := newMailEnv(t)
		mInv.registerWithEmail("駱子逸", "luo@ex.com")
		invalid := mInv.anonClient().rawAccept("GET",
			"/api/verify-email/confirm?t=THIS-TOKEN-NEVER-EXISTED", browser)

		branches := []struct {
			name    string
			r       rawResp
			code    string
			wantMsg string
		}{
			{"expired", expired, codeVerifyTokenExpired, "已過期"},
			{"used", used, codeVerifyTokenUsed, "使用過"},
			{"invalid", invalid, codeVerifyTokenInvalid, "無效"},
		}
		seen := map[string]string{}
		for _, b := range branches {
			if !strings.HasPrefix(b.r.contentType, "text/html") {
				t.Errorf("%s: Content-Type %q, want text/html", b.name, b.r.contentType)
				continue
			}
			// ★ The status must stay a failure status. A page is a different
			// REPRESENTATION of the outcome, not a different outcome — a 200
			// here would make a refused link look accepted to every log, probe
			// and proxy that reads status rather than body.
			if b.r.status != http.StatusBadRequest {
				t.Errorf("%s: status %d, want 400 — an error page answering 200 "+
					"makes a refusal indistinguishable from a success to anything "+
					"that reads status", b.name, b.r.status)
			}
			if !strings.Contains(b.r.body, b.wantMsg) {
				t.Errorf("%s page does not say %q:\n%s", b.name, b.wantMsg, b.r.body)
			}
			// The machine-readable code survives into the page, so an operator
			// reading a screenshot can still tell the branches apart.
			if !strings.Contains(b.r.body, b.code) {
				t.Errorf("%s page carries no %s marker", b.name, b.code)
			}
			if prev, dup := seen[b.r.body]; dup {
				t.Errorf("%s and %s render the BYTE-IDENTICAL page — the branches "+
					"are indistinguishable to the user", b.name, prev)
			}
			seen[b.r.body] = b.name
		}
	})

	// --- CONTROL: a non-browser caller still gets JSON ---------------------
	// Without this, "returns HTML" could be satisfied by an endpoint that has
	// no JSON representation at all, breaking every programmatic caller.
	t.Run("API callers still get JSON", func(t *testing.T) {
		m := newMailEnv(t)
		m.registerWithEmail("駱子逸", "luo@ex.com")
		second := m.anonClient()
		second.do("POST", "/api/verify-email",
			map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, nil)
		tok := m.linkToken(t)

		raw := second.rawAccept("GET", "/api/verify-email/confirm?t="+tok, "application/json")
		if !strings.HasPrefix(raw.contentType, "application/json") {
			t.Fatalf("an application/json Accept got %q", raw.contentType)
		}
		if strings.Contains(raw.body, "<!DOCTYPE") {
			t.Errorf("a JSON caller received a document:\n%s", raw.body)
		}
		if !strings.Contains(raw.body, codeVerifyOK) {
			t.Errorf("the JSON success answer carries no %s code:\n%s", codeVerifyOK, raw.body)
		}
	})
}

// ---------------------------------------------------------------------------
// ③ The magic link's scheme.
// ---------------------------------------------------------------------------

// TestVerifyLinkSchemeIsOperatorDeclared covers the flag, its absence, and the
// forged-header case.
func TestVerifyLinkSchemeIsOperatorDeclared(t *testing.T) {
	ask := func(m *mailEnv, hdr map[string]string) string {
		t.Helper()
		m.registerWithEmail("駱子逸", "luo@ex.com")
		c := m.anonClient()
		if code := c.doWithHeaders("POST", "/api/verify-email",
			map[string]any{"name": "駱子逸", "email": "luo@ex.com"}, hdr); code != http.StatusOK {
			t.Fatalf("verify-email: status %d", code)
		}
		sent, ok := m.mail.last()
		if !ok {
			t.Fatal("no mail was sent, so there is no link to inspect")
		}
		i := strings.Index(sent.body, "http")
		if i < 0 {
			t.Fatalf("the mail body carries no link:\n%s", sent.body)
		}
		link := sent.body[i:]
		if j := strings.IndexAny(link, "\r\n "); j >= 0 {
			link = link[:j]
		}
		return link
	}

	// --- the vector: with -public-url the link uses it ---------------------
	t.Run("declared origin is used", func(t *testing.T) {
		link := ask(newMailEnvPublicURL(t, "https://vote.example.com"), nil)
		if !strings.HasPrefix(link, "https://vote.example.com/api/verify-email/confirm?t=") {
			t.Errorf("link is %q, want it built from the declared origin", link)
		}
	})

	// --- CONTROL: without the flag the r.TLS derivation still governs -------
	// Without this arm, `return "https://" + …` unconditionally would satisfy
	// the vector above while breaking every direct-serve deployment (and
	// producing a link that does not resolve).
	t.Run("undeclared falls back to nothing — r.TLS governs", func(t *testing.T) {
		link := ask(newMailEnv(t), nil)
		if !strings.HasPrefix(link, "http://") {
			t.Errorf("with no -public-url and no TLS the link is %q; the scheme "+
				"must still come from r.TLS, or the flag has been written as a "+
				"constant true", link)
		}
		if strings.Contains(link, "vote.example.com") {
			t.Errorf("link %q carries a host nobody configured", link)
		}
	})

	// --- CONTROL: a forged X-Forwarded-Proto must change NOTHING -----------
	// ★ This is the arm that pins the fix to the right mechanism. An
	// implementation that read X-Forwarded-Proto would pass BOTH arms above —
	// and would hand any client the ability to choose the scheme of a link this
	// service mails to a third party. This codebase trusts no forwarded header
	// anywhere (identity.go, httpapi.go requireLoopback); the flag exists
	// precisely so that contract does not have to be reopened.
	t.Run("forwarded headers are not consulted", func(t *testing.T) {
		for _, h := range []map[string]string{
			{"X-Forwarded-Proto": "https"},
			{"X-Forwarded-Protocol": "https"},
			{"Forwarded": "proto=https"},
			{"X-Forwarded-Scheme": "https"},
			{"X-Url-Scheme": "https"},
		} {
			link := ask(newMailEnv(t), h)
			if strings.HasPrefix(link, "https://") {
				t.Errorf("header %v produced %q — a client-supplied header decided "+
					"the scheme of a mailed authentication link", h, link)
			}
		}
	})
}

// TestVerifyLinkReadsNoForwardedHeader is the source guard, matching the one
// identity.go already carries.
//
// ★ A behavioural test can only probe the header names somebody thought of.
// The list above is not exhaustive and cannot be; this covers the rest.
func TestVerifyLinkReadsNoForwardedHeader(t *testing.T) {
	src, err := os.ReadFile("verify.go")
	if err != nil {
		t.Fatalf("read verify.go: %v", err)
	}
	code := stripGoComments(string(src))

	// Control group: prove the matcher fires on planted source.
	planted := stripGoComments(`scheme := r.Header.Get("X-Forwarded-Proto")`)
	if !strings.Contains(planted, "X-Forwarded-Proto") {
		t.Fatal("control group failed: the matcher cannot see a planted header read")
	}

	for _, h := range []string{"X-Forwarded", "Forwarded", "X-Url-Scheme", "X-Real-IP"} {
		if strings.Contains(code, h) {
			t.Errorf("verify.go reads %s — the external scheme must come from the "+
				"operator-declared -public-url, never from a header any client can set", h)
		}
	}
	// And the declared origin must actually be consulted, or the flag is
	// decorative and the whole fix is absent while these guards stay green.
	if !strings.Contains(code, "a.publicURL") {
		t.Error("verify.go never reads a.publicURL — the operator-declared origin " +
			"is not wired into the link builder")
	}
}

// ---------------------------------------------------------------------------
// ④ The mail body is Traditional Chinese throughout.
// ---------------------------------------------------------------------------

// TestVerifyMailHasNoSimplifiedCharacters reads the shipped source.
//
// ★ WITH ITS CONTROL. Asserting "the file contains no 应" passes trivially for
// an empty file, a file that failed to load, or a matcher that cannot see CJK
// at all — absence and breakage share one output. The second half proves the
// matcher does see Traditional characters in this exact file.
func TestVerifyMailHasNoSimplifiedCharacters(t *testing.T) {
	src, err := os.ReadFile("../mailer/mailer.go")
	if err != nil {
		t.Fatalf("read mailer.go: %v", err)
	}
	text := string(src)

	// Control group: the matcher must find Traditional characters here, or a
	// clean verdict below would mean nothing.
	if !strings.Contains(text, "應") {
		t.Fatal("control group failed: no 應 found in mailer.go, so this matcher " +
			"cannot see CJK in this file and its verdict on 应 is worthless")
	}

	// Simplified forms that could plausibly appear in this file's prose. Each
	// one has a Traditional counterpart that IS used in this file, so a hit here
	// is a genuine mixed-script defect and not a false alarm on a shared glyph.
	for _, ch := range []string{"应", "这", "个", "认", "证", "连", "结", "装", "过", "点", "请", "会", "们", "已经"} {
		if strings.Contains(text, ch) {
			t.Errorf("mailer.go contains the simplified character %q", ch)
		}
	}
}
