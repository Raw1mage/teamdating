package httpapi

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"teamdating/internal/auth"
	"teamdating/internal/mailer"
	"teamdating/internal/store"
)

// Error codes for the email-verification flow (errors.md).
const (
	codeEmailRequiredForProposal = "EMAIL_REQUIRED_FOR_PROPOSAL"
	codeVerifyTokenExpired       = "VERIFY_TOKEN_EXPIRED"
	codeVerifyTokenInvalid       = "VERIFY_TOKEN_INVALID"
	codeVerifyTokenUsed          = "VERIFY_TOKEN_USED"
	codeSMTPSendFailed           = "SMTP_SEND_FAILED"
	codeNameSuffixExhausted      = "NAME_SUFFIX_EXHAUSTED"
	codeEmailInvalidFormat       = "EMAIL_INVALID_FORMAT"
	codeNameRequired             = "NAME_REQUIRED"
	codeNameCollision            = "NAME_COLLISION_CHOICE_REQUIRED"
	// codeEmailTaken answers an address already held by another identity
	// (owner-inheritance, 使用者裁示 Q5).
	//
	// ★ A DISTINCT CODE, NOT PERSIST_FAILED. Left unmapped, store.ErrEmailTaken
	// falls through to a 500 — so an ordinary collision that the user can fix by
	// typing a different address would be reported as a server fault they can do
	// nothing about, and the frontend would have no way to tell the two apart.
	codeEmailTaken = "EMAIL_TAKEN"
)

// writeSetEmailErr maps a SetEmail failure onto its HTTP answer.
//
// ★ ONE HELPER FOR ALL THREE CALL SITES. The mapping was originally written
// inline at each one, which is how the register path and the identity path
// came to answer differently for the same store error; a shared function makes
// the divergence unrepresentable rather than merely unlikely.
func writeSetEmailErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrEmailTaken) {
		writeErr(w, http.StatusConflict, codeEmailTaken,
			"這個 email 已經綁在另一個身分上，請換一個或先解除原本的綁定")
		return
	}
	writeErr(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
}

// verifyEmailReq starts the magic-link flow for a same-name login.
//
// ★ THERE IS NO uid FIELD, AND THERE MUST NEVER BE ONE (DD-8). The identity to
// be merged into is derived server-side from (name, email); accepting a uid
// here would let a caller nominate any identity and have the mail sent to an
// address they control, which is the entire authorisation gate handed to the
// attacker. The absence of the field is the guarantee — a check on it could be
// forgotten, a missing field cannot.
type verifyEmailReq struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// handleVerifyEmail is step 4 of the GRAFCET: mint a link, store it in memory,
// mail it. It never binds anything by itself.
func (a *api) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req verifyEmailReq
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	email := strings.TrimSpace(req.Email)
	if name == "" {
		writeErr(w, http.StatusBadRequest, codeNameRequired, "請輸入姓名")
		return
	}
	if err := store.ValidateEmail(email); err != nil {
		writeErr(w, http.StatusBadRequest, codeEmailInvalidFormat, "email 格式不正確")
		return
	}

	// Resolve WHICH identity this link would authorise — server-side, from the
	// roster, never from the request.
	target, claim, ok := a.resolveVerifyTarget(name, email)
	if !ok {
		// ★ SAME RESPONSE AS THE SUCCESS PATH, AND THIS IS NOT LAZINESS
		// (errors.md, DD Risks). Answering "no account with that email"
		// converts this endpoint into an oracle: an attacker submits a name
		// with candidate addresses and learns which one is registered. The
		// user who genuinely mistyped is not harmed by the uniform answer —
		// no mail arrives, which is the same thing they observe when their
		// own address is right but their inbox is slow.
		//
		// ★ AND NOTHING IS SENT. The uniform RESPONSE must not become a
		// uniform ACTION: mailing an unrelated address to keep the timing
		// identical would spam a stranger every time someone typos.
		log.Printf("verify-email: no matching identity for a submitted name/email pair (uniform response sent)")
		writeJSON(w, http.StatusOK, map[string]any{
			"pending": true,
			"msg":     "認證信已寄出，請到收件匣點連結完成認證",
		})
		return
	}

	plaintext, hash, err := newVerifyToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "TOKEN_FAILED", err.Error())
		return
	}

	// ★ MAIL FIRST, RECORD SECOND (TV-10). If the send fails, no pending entry
	// exists — so a failed send cannot leave a live magic link behind that
	// somebody could still stumble into. The reverse order would satisfy the
	// same test suite and quietly widen the window.
	link := a.verifyLink(r, plaintext)
	if err := a.mailer.Send(email, mailer.VerifySubject, mailer.VerifyBody(name, link)); err != nil {
		// fail-loud: the user is told, and NOTHING is bound (DD Risks).
		// ★ The SMTP error text is logged but NOT returned: it names the relay
		// host and can carry its banner, which is deployment detail a caller
		// has no business learning.
		log.Printf("verify-email: smtp send failed: %v", err)
		writeErr(w, http.StatusBadGateway, codeSMTPSendFailed,
			"認證信寄送失敗，請稍後再試或確認 email 是否正確")
		return
	}

	// ★ THE REQUESTER'S OWN IDENTITY, FROM THE COOKIE, CAPTURED NOW (DD-8).
	// Read here rather than at confirm time because the link is routinely
	// clicked in another browser — a phone mail app, a desktop session — where
	// the cookie is a different identity or none. Deriving it there would let
	// whoever opens the mail choose which account gets absorbed.
	//
	// Empty when the caller holds no identity yet: that is the ordinary
	// round-1 case (a fresh device joining an existing person), and it means
	// there is simply nothing to absorb.
	fromUID := identify(a.store, r).UID
	if fromUID == target.UID {
		// Already the same identity — asking to merge it into itself would be
		// refused by the store, so record nothing and let confirm just bind.
		fromUID = ""
	}

	pm := &pendingMerge{
		uid: target.UID, email: email, name: name, claim: claim,
		fromUID:   fromUID,
		expiresAt: a.pending.clock().Add(verifyTTL),
	}
	if err := a.pending.put(hash, pm); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "PENDING_FULL", err.Error())
		return
	}
	if claim {
		// DD-9 is a KNOWINGLY WEAK path, so it leaves a trace. Without this
		// line a claim of an email-less account and a genuine same-email merge
		// are indistinguishable afterwards — and the claim is the one whose
		// risk was accepted rather than eliminated.
		log.Printf("verify-email: DD-9 claim of an email-less account issued (uid=%s)", target.UID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pending": true,
		"msg":     "認證信已寄出，請到收件匣點連結完成認證",
	})
}

// resolveVerifyTarget picks the identity a magic link would authorise, and
// reports whether this is the DD-9 claim path.
//
// ★ TWO PATHS, RETURNED DISTINCTLY. An exact email match (DD-3) is real proof
// that the requester controls the address already on the account. An
// email-less account (DD-9) is NOT: nothing on it can be compared, so what the
// link proves is only that the CLAIMER's own address works. Collapsing the two
// into one bool would erase, at the only place it is still visible, which kind
// of authorisation happened.
func (a *api) resolveVerifyTarget(name, email string) (store.User, bool, bool) {
	if u, ok := a.store.UserByNameAndEmail(name, email); ok {
		return u, false, true
	}
	// DD-9 (單純版, 使用者明示): a same-name account with NO email may be
	// claimed by someone who proves their own address.
	//
	// ★ THE HOLE IS REAL AND IS RECORDED, NOT PATCHED HERE. An account with no
	// email cannot distinguish "its owner returning" from "a stranger who knows
	// the name" — there is nothing to compare against. This is an accepted,
	// explicitly-deferred trade (design.md DD-9), not an oversight; do not
	// "fix" it silently, and do not present it as safe.
	if cands := a.store.UsersByNameWithoutEmail(name); len(cands) == 1 {
		return cands[0], true, true
	}
	return store.User{}, false, false
}

// verifyLink builds the absolute URL that goes in the mail.
//
// ★ WHY publicURL EXISTS (measured on the live deployment). Behind a reverse
// proxy the TLS is terminated at the proxy and the hop to this process is
// plain HTTP, so r.TLS is nil for EVERY request no matter how the user
// arrived. The derivation below is not wrong, its premise is inverted by the
// deployment — and nothing in this process can observe that. Result: an
// authentication token was mailed on an http:// URL to real users.
//
// ★ AND THE FIX IS NOT X-Forwarded-Proto. This codebase deliberately trusts no
// forwarded header (identity.go, httpapi.go:296, guarded by
// TestNoForwardedHeaderIsRead) because behind a proxy any client can send one
// with any value. Honouring it here would overturn that contract through a new
// door. The operator states the canonical URL instead — a value a caller
// cannot influence at all.
//
// ★ NOT A FALLBACK. Unset means "nobody told us", and the r.TLS derivation is
// the correct answer for the direct-serve/dev case it describes. Two named
// modes, each right for its deployment — not one mode papering over the other.
func (a *api) verifyLink(r *http.Request, token string) string {
	path := strings.TrimSuffix(a.base, "/") + "/api/verify-email/confirm?t=" + token
	if a.publicURL != "" {
		return a.publicURL + path
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

// verifyOutcome writes the confirm flow's answer in whichever representation
// the caller asked for (verifypage.go).
//
// ★ ONE FUNCTION FOR ALL FIVE EXITS. The negotiation used to be absent, and
// writing it inline at each branch is how one of them would come to answer
// JSON to a browser again — the divergence has to be unrepresentable, not
// merely unlikely.
func (a *api) verifyOutcome(w http.ResponseWriter, r *http.Request,
	status int, code, msg, heading string, ok bool, name string) {
	if prefersHTML(r) {
		a.renderVerifyPage(w, status, verifyPageData{
			OK: ok, Code: code, Heading: heading, Message: msg, Name: name,
		})
		return
	}
	if ok {
		// Never meResp here: see confirmResp.
		writeJSON(w, status, confirmResp{Code: code, Msg: msg, Name: name, Verified: true})
		return
	}
	writeErr(w, status, code, msg)
}

// confirmResp is the JSON representation of a SUCCESSFUL confirm.
//
// ★ IT IS NOT meResp, AND THAT IS THE WHOLE POINT (the recovery-code leak).
// This handler used to answer with meResp, whose Recovery field is a standing
// credential — RecoveryCode(token) reattaches an identity through
// /api/register, so it is a password, and this path renders in a BROWSER the
// user reached from their mail client. It went into the page, the scrollback,
// the screenshot they sent to ask why it looked broken. Nothing read it: the
// frontend never calls this endpoint at all, so its only effect was exposure.
//
// ★ Fixed by TYPE, not by remembering to blank a field. A response struct with
// no Recovery field cannot regain one by an edit to a handler; someone would
// have to add it here, under this comment. The register path keeps its
// recovery code (that IS the moment the plaintext exists and the user is told
// to write it down) — see handlers.go:423/483/536.
//
// ★ There is no uid either. A caller that wants to know who it now is asks
// /api/me with the cookie it was just given, which is the same answer derived
// from the credential rather than asserted alongside it.
type confirmResp struct {
	Code     string `json:"code"`
	Msg      string `json:"msg"`
	Name     string `json:"name"`
	Verified bool   `json:"verified"`
}

// codeVerifyOK marks the success branch so a machine caller can switch on one
// field rather than on the absence of an error.
const codeVerifyOK = "VERIFY_OK"

// handleVerifyConfirm is GRAFCET step 5: the user clicked the link.
//
// ★ Reached by GET (a click in a mail client) as well as POST. The GET is a
// state-changing request, which is normally wrong — it is accepted here
// because a magic link has no other shape: a mail client cannot POST. The
// mitigations that make it defensible are the ones already in place: the token
// is single-use, expires in 15 minutes, and is unguessable, so a prefetching
// mail scanner burns the link (the user requests another) rather than granting
// anything to a third party.
func (a *api) handleVerifyConfirm(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("t"))
	if token == "" {
		a.verifyOutcome(w, r, http.StatusBadRequest, codeVerifyTokenInvalid,
			"認證連結無效，請重新申請認證信", "認證連結無效", false, "")
		return
	}
	pm, outcome := a.pending.consume(token)
	switch outcome {
	case verifyExpired:
		a.verifyOutcome(w, r, http.StatusBadRequest, codeVerifyTokenExpired,
			"認證連結已過期，請重新申請認證信", "認證連結已過期", false, "")
		return
	case verifyUsed:
		a.verifyOutcome(w, r, http.StatusBadRequest, codeVerifyTokenUsed,
			"此認證連結已使用過，如需再次認證請重新申請", "連結已經使用過", false, "")
		return
	case verifyInvalid:
		a.verifyOutcome(w, r, http.StatusBadRequest, codeVerifyTokenInvalid,
			"認證連結無效，請重新申請認證信", "認證連結無效", false, "")
		return
	}

	// The identity comes from the SERVER-SIDE pending entry, never from the
	// request (DD-8). The only thing the caller supplied is the opaque token.
	token2, hash, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "TOKEN_FAILED", err.Error())
		return
	}
	if err := a.store.AddToken(pm.uid, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}

	// ★ ABSORB THE REQUESTER'S OLD IDENTITY (使用者裁示 Q2c). The link proved
	// the two records are one person, so they become one record: tokens, votes,
	// option authorship and proposal ownership all move to pm.uid, and the old
	// row leaves the roster — which is what keeps the displayed name unique.
	//
	// ★ THE SURVIVOR IS THE VERIFIED IDENTITY, NOT THE REQUESTER, and the
	// direction is load-bearing. Absorbing the other way would strand every
	// proposal the verified account owns: its uid would stay the owner while no
	// device could authenticate as it again, and because that account HAS an
	// email, each of its proposals would keep challenging every same-name
	// visitor — with the mail going to an address the survivor no longer holds.
	//
	// ★ A MERGE FAILURE DOES NOT FAIL THE REQUEST. The device is already bound
	// at this point; answering with an error would tell a user whose
	// authentication genuinely succeeded that it did not, and they would retry
	// into a link that is now spent. Logged loudly instead — the residue is a
	// duplicate row an operator can see, not a lost binding.
	if pm.fromUID != "" && pm.fromUID != pm.uid {
		if err := a.store.MergeUsers(pm.fromUID, pm.uid); err != nil {
			log.Printf("verify-confirm: bound uid=%s but failed to absorb prior identity %s: %v",
				pm.uid, pm.fromUID, err)
		}
	}
	// Persist the verified address. On the DD-9 claim path the account had
	// none, so this is what stops the SAME account being claimable again by
	// the next person who knows the name — the hole is left open for exactly
	// one claim, not permanently.
	u, _ := a.store.User(pm.uid)
	if !store.SameEmail(u.Email, pm.email) {
		if err := a.store.SetEmail(pm.uid, pm.email); err != nil {
			// The device is already bound at this point; refusing here would
			// leave the user authenticated but told they failed. Log loudly
			// and continue — the binding is the thing they asked for.
			log.Printf("verify-confirm: bound uid=%s but failed to persist email: %v", pm.uid, err)
		}
	}
	setTokenCookie(w, token2, a.base, r.TLS != nil)
	u, _ = a.store.User(pm.uid)
	a.verifyOutcome(w, r, http.StatusOK, codeVerifyOK,
		"這台裝置已經接上你的身分，回到行程統合就能直接使用。",
		"認證完成", true, u.Name)
}

// identityReq is the gear-modal body: change name and/or email (DD-6, 3.6).
//
// ★ Pointers so "field omitted" and "field set to empty" stay distinguishable.
// A plain string would make an omitted email indistinguishable from a request
// to CLEAR it, so renaming yourself would silently wipe your address and
// re-close the proposal gate with no error.
type identityReq struct {
	Name  *string `json:"name,omitempty"`
	Email *string `json:"email,omitempty"`
}

// handleIdentity is the gear-modal backend. Any logged-in identity may use it
// (DD-6): email is not privileged, it is just a field the owner maintains.
func (a *api) handleIdentity(w http.ResponseWriter, r *http.Request) {
	// ★ The subject is the COOKIE's identity and nothing else (DD-8). This
	// endpoint changes a name and an email — exactly the two values the merge
	// split reads — so a uid taken from the body here would let anyone rewrite
	// a stranger's address and then verify it as their own.
	id := identify(a.store, r)
	if id.UID == "" {
		writeErr(w, http.StatusForbidden, auth.CodeIdentityUnregistered,
			"請先登記姓名")
		return
	}
	var req identityReq
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if n := len([]rune(name)); n == 0 || n > store.MaxNameLen {
			writeErr(w, http.StatusBadRequest, "NAME_INVALID", "姓名必須是 1 到 16 個字")
			return
		}
		// The roster-wide uniqueness invariant applies to renames too: without
		// it a user could rename themselves onto an existing name and
		// reintroduce the duplicate the suffix allocator exists to prevent —
		// through a different door, with every test on the register path still
		// green.
		//
		// ★ ENFORCED INSIDE RenameUser, atomically with the write. A NameTaken
		// call here would be a check-then-act race: concurrent renames onto one
		// free name would each observe "free" and each succeed.
		if err := a.store.RenameUser(id.UID, name); err != nil {
			if errors.Is(err, store.ErrNameTaken) {
				writeErr(w, http.StatusConflict, codeNameCollision,
					"這個名字已經有人使用，請換一個")
				return
			}
			writeErr(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
			return
		}
	}

	if req.Email != nil {
		email := strings.TrimSpace(*req.Email)
		if email != "" {
			if err := store.ValidateEmail(email); err != nil {
				writeErr(w, http.StatusBadRequest, codeEmailInvalidFormat, "email 格式不正確")
				return
			}
		}
		if err := a.store.SetEmail(id.UID, email); err != nil {
			writeSetEmailErr(w, err)
			return
		}
	}

	u, _ := a.store.User(id.UID)
	writeJSON(w, http.StatusOK, meResp{
		UID: id.UID, Name: u.Name, Email: u.Email,
		Registered: strings.TrimSpace(u.Name) != "",
	})
}

// pendingTTLForTest exposes the TTL so a test can build an expired entry
// without duplicating the constant (a duplicated 15 can drift from the real
// one, and the test would then pass against a value production does not use).
func pendingTTLForTest() time.Duration { return verifyTTL }
