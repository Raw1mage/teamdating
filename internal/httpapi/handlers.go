package httpapi

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"teamdating/internal/auth"
	"teamdating/internal/store"
)

// registerReq is the body of POST /api/register.
type registerReq struct {
	Name string `json:"name"`
	// Recovery, when present, reattaches an existing identity instead of
	// minting a new one (DD-2).
	Recovery string `json:"recovery,omitempty"`
	// UID picks WHICH same-name identity to join, answering a candidate list
	// this endpoint returned a moment earlier.
	//
	// ★ THIS IS THE ONE PLACE A uid MAY ARRIVE IN A REQUEST BODY, AND IT IS
	// NOT A HOLE IN DD-8. DD-8 governs AUTHENTICATION — identify() derives the
	// caller's uid from the cookie token and nothing else — and that is
	// untouched: this field is read only on the branch where the caller has NO
	// cookie, i.e. where there is no authenticated identity to override.
	//
	// ★ IT IS ALSO CHECKED, AND THE CHECK IS THE WHOLE SECURITY ARGUMENT. The
	// value is honoured only if it appears in the candidate set for THIS name.
	// Without that test the endpoint would degrade from "knowing a name lets
	// you join it" — the collision risk the product accepted — into "knowing
	// any uid lets you join any identity", which nobody accepted and which
	// would leak nothing in the response to make it visible.
	UID string `json:"uid,omitempty"`
	// Email is OPTIONAL at registration (DD-6): email is not globally
	// mandatory, so someone who only picks dates never supplies it. It is
	// carried here so a user who already knows they will create a proposal can
	// set it in one step instead of being bounced to the gear modal.
	//
	// ★ IT IS NOT AN AUTHENTICATION INPUT ON THIS ENDPOINT. Supplying an email
	// here never merges anything: the merge decision lives behind the magic
	// link (/api/verify-email), and this field only ever writes the address
	// onto the caller's OWN identity. Treating it as proof of ownership here
	// would hand a stranger the merge that verification exists to gate.
	Email string `json:"email,omitempty"`
	// Choice answers the真撞名不同人 prompt (DD-3 step 6): "suffix" accepts a
	// forced `-N`, anything else means the user will retype a different name.
	//
	// ★ NO AUTOMATIC SUFFIX (DD-3). Silently appending `-1` renames a person
	// without asking, and they discover it only when someone else cannot find
	// them. The explicit choice is the feature, so the server refuses rather
	// than guessing when this field is absent.
	Choice string `json:"choice,omitempty"`
}

// countProbe records one traceless answer to an unauthenticated caller.
//
// ★ EVERY BRANCH THAT ANSWERS AN ANONYMOUS CALLER WITHOUT WRITING TO THE STORE
// MUST COME THROUGH HERE, and "every" is the load-bearing word rather than a
// tidiness preference. The event log is the only durable record of activity, so
// a branch that changes nothing leaves nothing behind: uncounted, "nobody ever
// probed this name" and "somebody enumerated it all week" are the same empty
// log. The multi-candidate branch was made countable for exactly that reason;
// the email split then added three more answers of the same shape, and
// counting only the original one restored the blind spot through a new door.
//
// ★ IT ALSO KEEPS THE COUNTER FROM BECOMING AN ORACLE. candidateProbesTotal is
// published on /api/health, so a probe that increments on one outcome and not
// on its sibling would let two reads around a guess reveal which outcome
// occurred — the membership answer the uniform verify-email response exists to
// withhold. Call this ABOVE the branch that splits on a secret, never inside
// one arm of it.
//
// reason is for the operator log only. The submitted NAME is never logged: it
// is user-supplied and is precisely the secret a probe is trying to confirm, so
// writing it would turn the log into the searchable roster this system has no
// route for.
func (a *api) countProbe(reason string) {
	n := a.candidateProbes.Add(1)
	log.Printf("register: answered an unauthenticated caller without changing state (%s) (probe #%d)", reason, n)
}

// candidateView is one same-name identity offered for the user to pick.
//
// ★ WHY ANY CONTENT IS ATTACHED AT ALL. Two identities named "駱子逸" are
// indistinguishable by name — that is what makes them candidates. A list of
// bare uids would be a coin flip with extra steps, which is exactly the defect
// being fixed. The user recognises their own identity by what is inside it.
//
// ★ NO PROPOSAL TITLES. An earlier version of this struct carried a truncated
// sample of them, on the reasoning that typing a name already on the roster
// ALREADY granted the typist that whole identity, so a preview disclosed
// strictly less than the same request could obtain anyway. That reasoning is
// true about DISCLOSURE and misses the larger harm, because it ignores who the
// extra information helps:
//
//	the owner    knows which identity is theirs; a title only confirms it
//	an impostor  knows nothing, and a title tells them WHICH ONE TO TAKE
//
// The old behaviour handed a mistyper a coin flip. A list showing "建立了 1 個
// 提案：華強北公模採購" hands them a menu, and they will not pick the empty
// one. Self-healing must not convert a random collision into a guided one.
//
// So the rule is: carry the MINIMUM that decides. The live case that motivated
// this feature is 1 proposal versus 0 — the COUNT already decides it, and the
// title adds nothing to that decision while adding everything to a stranger's.
//
// ★ CreatedAt is here for the case the count cannot settle (both zero, the
// common shape for vote-only identities). It has the opposite ratio to a
// title: the owner knows roughly when they started using this, and an impostor
// has no way to evaluate it at all.
//
// ★ THE RESIDUAL IS REAL AND IS NOT CLAIMED AWAY. Proposals is a count, and a
// count still tells an impostor which identity holds content — it moves them
// from 50% to ~100% at picking the one worth taking. It is kept because
// without it the feature does not work: the owner would be back to guessing,
// which is the defect being fixed. What removing titles buys is that the
// impostor learns THAT one identity has content, never WHAT it is, and the
// name-collision risk the product accepted stays bounded to "knowing a name".
type candidateView struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
	// Proposals is how many proposals this identity OWNS — the count only.
	Proposals int `json:"proposals"`
	// CreatedAt lets a returning user recognise their identity when the counts
	// are equal.
	CreatedAt string `json:"createdAt"`
}

// meResp describes the caller to itself.
type meResp struct {
	UID        string `json:"uid"`
	Name       string `json:"name"`
	Registered bool   `json:"registered"`
	// Email is the caller's OWN address, echoed back so the gear modal can
	// show what is currently set (DD-6).
	//
	// ★ It appears ONLY in a response describing the CALLER to themselves —
	// never in candidateView, never in a voterRef, never in a proposal view.
	// An address attached to any roster entry other than your own would turn
	// the same-name candidate list into an address-harvesting surface, which
	// is the opposite of what this field is for.
	//
	// ★ NOT omitempty: the frontend must be able to tell "no email set" from
	// "the server did not say", because the first opens the gear modal to
	// collect one and the second means something is wrong. omitempty makes
	// those two the same absent key.
	Email string `json:"email"`
	// Recovery is returned ONLY on the request that mints the token. It is the
	// single opportunity to show it — the server keeps a hash and cannot
	// reproduce the plaintext later (DD-16).
	Recovery string `json:"recovery,omitempty"`
}

// handleMe reports the current identity. Unregistered is a normal 200 with
// registered:false, not an error: "no identity yet" is the expected first
// state, and returning 401 would make the frontend treat a fresh visitor as a
// failure (errors.md BACKEND_UNREACHABLE must stay distinguishable from
// "you simply have not registered").
func (a *api) handleMe(w http.ResponseWriter, r *http.Request) {
	id := identify(a.store, r)
	// ★ The email is read from the roster, not carried on `identity`. identity
	// is the AUTHENTICATION result (who the token proves you are); the address
	// is ordinary profile data. Widening identity to carry it would put a
	// mutable field on the struct every authorisation decision reads, and the
	// next person to add a check there would have no way to tell which fields
	// are load-bearing.
	var email string
	if id.UID != "" {
		if u, ok := a.store.User(id.UID); ok {
			email = u.Email
		}
	}
	writeJSON(w, http.StatusOK, meResp{
		UID: id.UID, Name: id.Name, Email: email, Registered: id.Registered,
	})
}

// handleRegister resolves a submitted name to an identity. Exactly three
// outcomes, and telling them apart IS the feature (DD-8):
//
//	有 cookie                  -> rename; the token set is untouched
//	無 cookie + name on roster -> AddToken to THAT uid (a second device)
//	無 cookie + name unknown   -> mint a new uid and its first token
//
// ★ The middle row is the whole point of multi-token, and it is also where the
// name-collision risk is amplified: someone who mistypes a name that happens to
// be taken lands a durable key on a stranger's identity. That trade is accepted
// deliberately (design.md Risks; read.go documents the same policy for
// UIDByName) — the product has no sensitive data and favours convenience over
// isolation. It is recorded here so a future reader does not "fix" it by
// accident.
func (a *api) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	email := strings.TrimSpace(req.Email)

	if strings.TrimSpace(req.Recovery) != "" {
		a.reattach(w, r, req.Recovery)
		return
	}

	if n := len([]rune(name)); n == 0 || n > store.MaxNameLen {
		writeErr(w, http.StatusBadRequest, "NAME_INVALID",
			"姓名必須是 1 到 16 個字")
		return
	}

	// An existing token means a rename, not a second identity. Minting a new
	// one here would orphan the caller's previous votes with no way back.
	//
	// ★ This branch must stay ABOVE the reattach branch below. Routing a
	// cookie-bearing request into AddToken would hand the caller a second token
	// for the identity it already holds — every response still looks correct,
	// and the only symptom is that renaming burns a token slot until eviction
	// starts logging the user's own devices out. TV-4 is the vector that
	// separates these two paths.
	if id := identify(a.store, r); id.UID != "" {
		// A rename must respect the roster-wide uniqueness invariant too
		// (DD-3): without it the suffix allocator's guarantee could be undone
		// through the rename door, and every test on the register path would
		// still be green.
		//
		// ★ THE TEST IS INSIDE RenameUser, NOT HERE. Asking NameTaken first and
		// then renaming is a check-then-act race — the two calls take different
		// locks, so concurrent renames onto one free name all observe "free" and
		// all succeed. Store-side the decision and the write share one critical
		// section, so only one can win; this handler's job is just to report
		// which error came back.
		if err := a.store.RenameUser(id.UID, name); err != nil {
			if errors.Is(err, store.ErrNameTaken) {
				writeErr(w, http.StatusConflict, codeNameCollision,
					"這個名字已經有人使用，請換一個")
				return
			}
			writeErr(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
			return
		}
		if email != "" {
			if err := store.ValidateEmail(email); err != nil {
				writeErr(w, http.StatusBadRequest, codeEmailInvalidFormat, "email 格式不正確")
				return
			}
			if err := a.store.SetEmail(id.UID, email); err != nil {
				writeSetEmailErr(w, err)
				return
			}
		}
		u, _ := a.store.User(id.UID)
		writeJSON(w, http.StatusOK, meResp{UID: id.UID, Name: name, Email: u.Email, Registered: true})
		return
	}

	// No cookie, but the name is already on the roster: this is another device
	// of that identity asking to join (the "name is the recovery key"
	// convenience). Give it its OWN token rather than minting a new identity,
	// so the caller's existing proposals and votes come with them.
	//
	// ★ ALL same-name identities, not the first one. See UsersByName: picking
	// one by map order was a coin flip that landed on an empty identity for a
	// real user on live data.
	cands := a.store.UsersByName(name)

	// The caller is answering a candidate list this endpoint returned a moment
	// ago. Honour the pick ONLY if it is in the current candidate set for this
	// name — that check is what keeps the privilege at "knowing a name" instead
	// of widening it to "knowing any uid".
	if picked := strings.TrimSpace(req.UID); picked != "" {
		for _, c := range cands {
			if c.UID != picked {
				continue
			}
			// ★ THE EMAIL GATE MUST NOT BE BYPASSABLE THROUGH THIS DOOR. The
			// pick path predates email verification and binds a device with no
			// proof beyond knowing the name (DD-6 寬容期). That remains
			// acceptable for a legacy account which has NOTHING to verify
			// against — but the moment an identity HAS an email, honouring a
			// bare uid pick would hand a stranger exactly the merge the magic
			// link exists to gate, while every response still looked normal.
			if strings.TrimSpace(c.Email) != "" {
				a.countProbe("uid-pick against an identity that holds an email")
				writeJSON(w, http.StatusOK, map[string]any{
					"needEmailVerify": true,
					"name":            name,
					"msg":             "這個身分已設定 email，請輸入它的 email 並完成信件認證才能接回",
				})
				return
			}
			a.attachDevice(w, r, c.UID, c.Name)
			return
		}
		// Not a candidate for this name. Deliberately the same response for
		// "no such uid" and "that uid exists but is named something else": a
		// distinguishable answer would turn this endpoint into an oracle for
		// probing which uids exist.
		writeErr(w, http.StatusBadRequest, "CANDIDATE_INVALID",
			"選取的身分已不在這個名字的候選名單中，請重新輸入名字")
		return
	}

	// ---------------------------------------------------------------
	// Same-name arbitration (identity plan A2/A3/A4, GRAFCET step 3)
	//
	// ★ THE SPLIT IS ON EMAIL EQUALITY, NOT ON NAME EQUALITY (DD-3). Sharing a
	// name is what makes two people CANDIDATES; it is not evidence they are the
	// same person. Before this block existed, a matching name alone was enough
	// to land a durable token on a stranger's identity — the gap this plan
	// closes.
	// ---------------------------------------------------------------
	if len(cands) > 0 {
		if email != "" {
			if err := store.ValidateEmail(email); err != nil {
				writeErr(w, http.StatusBadRequest, codeEmailInvalidFormat, "email 格式不正確")
				return
			}
			// (a) email matches an existing same-name identity, or the DD-9
			// claimable case. Answer "go verify" — and bind NOTHING here. The
			// binding happens only after the magic link is clicked (step 5);
			// doing it now would make the mail decorative.
			//
			// ★ COUNTED BEFORE THE SPLIT, SO BOTH OUTCOMES COST THE SAME. The
			// counter is published on /api/health, so incrementing it on a match
			// but not on a miss would rebuild the very oracle verify-email's
			// uniform response exists to deny: read health, submit a guess, read
			// health again, and the delta answers "is this address on file".
			// Counting here — above both branches — makes the two indistinguishable
			// through that channel.
			a.countProbe("same-name login carrying an email")
			if _, _, ok := a.resolveVerifyTarget(name, email); ok {
				writeJSON(w, http.StatusOK, map[string]any{
					"needEmailVerify": true,
					"name":            name,
					"msg":             "這個名字已有既有身分，請完成信件認證以接回它",
				})
				return
			}
			// (b) email matches nobody → genuinely a different person with the
			// same name (GRAFCET step 6).
			a.nameCollisionChoice(w, r, name, email, req.Choice)
			return
		}
		// No email supplied. If ANY same-name identity has one, this is a claim
		// attempt against a verifiable account and email is mandatory here
		// (DD-6 case (b): 撞名認領時). Otherwise fall through to the legacy
		// candidate-pick path below (DD-6 寬容期).
		for _, c := range cands {
			if strings.TrimSpace(c.Email) != "" {
				a.countProbe("same-name login with no email against a verifiable account")
				writeJSON(w, http.StatusOK, map[string]any{
					"emailRequired": true,
					"name":          name,
					"msg":           "這個名字已有人使用。如果是你本人，請輸入該身分的 email 完成認證；如果是同名的別人，請改名或接受自動編號",
				})
				return
			}
		}
	}

	// Two or more people (or one person's pre-existing split identities) share
	// this name. Do not guess — hand back the candidates and let a human say
	// which one is theirs.
	if len(cands) > 1 {
		// ★ ONE OF SEVERAL BRANCHES THAT ANSWER AN UNAUTHENTICATED CALLER
		// WITHOUT CHANGING ANY STATE, and a request that changes nothing writes
		// nothing to the event log. Every other anonymous outcome here (mint /
		// attach / rename) ends in a store mutation that the log records, so
		// before this branch was counted the service could not be questioned
		// without leaving a trace. Left uncounted, "nobody ever probed a name"
		// and "somebody enumerated one name all week" are the same empty log —
		// the absence/failure conflation R8 avoids structurally by having no
		// listing route at all.
		//
		// ★ IT WAS THE ONLY SUCH BRANCH WHEN THIS COUNTER WAS ADDED, AND THAT
		// CLAIM IS NOW FALSE. The email split introduced three more answers of
		// the same shape (emailRequired, and both arms of the same-name login
		// carrying an email); all of them go through countProbe. This sentence
		// is kept accurate deliberately — a reader can check it against the
		// file, and a comment that claims uniqueness while three siblings exist
		// stops them looking for the others.
		//
		// ★ Counted here, NOT logged as an event. The event log is replay
		// state: appending from an unauthenticated read path would let anyone
		// grow startup cost without bound, and would add another event type
		// old binaries reject (DD-5 is already not rollback-safe). The
		// counter is process-local and rate-shaped on purpose — it answers
		// "is someone probing", never "who", which would need the very
		// per-caller identity this path is defined not to have.
		// The name is NOT logged — see countProbe.
		a.countProbe(fmt.Sprintf("returned %d same-name candidates", len(cands)))
		writeJSON(w, http.StatusOK, map[string]any{
			"candidates": a.candidateViews(cands),
		})
		return
	}

	if len(cands) == 1 {
		a.attachDevice(w, r, cands[0].UID, cands[0].Name)
		return
	}

	token, hash, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "TOKEN_FAILED", err.Error())
		return
	}
	uid, err := newID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ID_FAILED", err.Error())
		return
	}
	if err := a.store.CreateUser(uid, hash, name); err != nil {
		writeErr(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}
	setTokenCookie(w, token, a.base, r.TLS != nil)
	writeJSON(w, http.StatusOK, meResp{
		UID: uid, Name: name, Registered: true,
		Recovery: auth.RecoveryCode(token),
	})
}

// nameCollisionChoice handles GRAFCET step 6: the submitted email matches no
// same-name identity, so this really is a DIFFERENT PERSON who happens to share
// a name.
//
// ★ IT REFUSES TO ACT WITHOUT AN EXPLICIT CHOICE (DD-3), and that refusal is
// the feature rather than an unfinished branch. Auto-appending `-1` would be
// one line shorter and would rename a human being without asking: they would
// keep introducing themselves as 駱子逸 while the roster showed 駱子逸-1, and
// they would find out only when a friend could not spot them in the list. The
// two legal outcomes are "I will retype a different name" (step 7, handled
// entirely by the client re-POSTing) and "I accept a number" (step 8).
//
// ★ The uniqueness invariant is satisfied EITHER WAY. That is what makes the
// choice safe to offer: retyping loops back through the same collision check,
// and accepting a suffix allocates inside one store commit.
func (a *api) nameCollisionChoice(w http.ResponseWriter, r *http.Request, name, email, choice string) {
	if strings.TrimSpace(choice) != "suffix" {
		// Step 6: report the collision and WAIT. No state changes here — the
		// caller has not yet said what they want.
		writeJSON(w, http.StatusOK, map[string]any{
			"nameCollision": true,
			"name":          name,
			"msg":           "這個名字已經有別人在用（你填的 email 不屬於任何一個同名身分）。請改用另一個名字，或接受系統自動加上的編號。",
		})
		return
	}

	// Step 8: allocate the lowest free `-N` and create, as ONE commit.
	token, hash, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "TOKEN_FAILED", err.Error())
		return
	}
	uid, err := newID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ID_FAILED", err.Error())
		return
	}
	// ★ CreateUserWithSuffix, not "pick a suffix then CreateUser". The second
	// shape is a check-then-act race: two collisions arriving together both
	// read "-1 is free" and both take it, and the roster ends up holding the
	// duplicate this whole path exists to prevent — with no error on either
	// request.
	finalName, err := a.store.CreateUserWithSuffix(uid, hash, name, email)
	if err != nil {
		if errors.Is(err, store.ErrNameSuffixExhausted) {
			writeErr(w, http.StatusConflict, codeNameSuffixExhausted,
				"這個姓名的可用編號已用盡，請改用其他名字")
			return
		}
		writeErr(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}
	setTokenCookie(w, token, a.base, r.TLS != nil)
	writeJSON(w, http.StatusOK, meResp{
		UID: uid, Name: finalName, Email: email, Registered: true,
		Recovery: auth.RecoveryCode(token),
	})
}

// candidateViews summarises each same-name identity enough for its owner to
// recognise it, and no further.
//
// ★ OWNED proposals only — deliberately NOT ProposalsForUser's raw result,
// which also counts proposals the uid merely VOTED in. Those belong to someone
// else, and that third party never took part in this reattach. The narrower
// query is the guard; it is also the sharper recognition signal, because "I
// created something" identifies an identity better than "I voted somewhere".
//
// ★ A COUNT, NEVER THE CONTENT. See candidateView: the count is the minimum
// that lets an owner decide, and titles would additionally tell a stranger
// which identity is worth taking.
func (a *api) candidateViews(users []store.User) []candidateView {
	out := make([]candidateView, 0, len(users))
	for _, u := range users {
		cv := candidateView{UID: u.UID, Name: u.Name, CreatedAt: u.CreatedAt}
		for _, p := range a.store.ProposalsForUser(u.UID) {
			if p.OwnerUID != u.UID {
				continue
			}
			cv.Proposals++
		}
		out = append(out, cv)
	}
	return out
}

// attachDevice mints a fresh token for an EXISTING identity and hands it to
// this device (multi-token DD-1).
//
// ★ A NEW token, not a copy of one the identity already holds. Two devices
// sharing one token would work — and would make the two indistinguishable to
// LastSeenAt, so eviction could not tell an abandoned phone from an active
// laptop. One key per device is what makes the LRU ordering meaningful at all.
func (a *api) attachDevice(w http.ResponseWriter, r *http.Request, uid, name string) {
	token, hash, err := auth.NewToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "TOKEN_FAILED", err.Error())
		return
	}
	if err := a.store.AddToken(uid, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, "PERSIST_FAILED", err.Error())
		return
	}
	setTokenCookie(w, token, a.base, r.TLS != nil)
	writeJSON(w, http.StatusOK, meResp{
		UID: uid, Name: name, Registered: strings.TrimSpace(name) != "",
		// The recovery code is shown for the token just minted — it is the only
		// moment the plaintext exists (DD-16), and this device needs its own.
		Recovery: auth.RecoveryCode(token),
	})
}

// reattach resolves a recovery code back to its identity (DD-2).
//
// ★ The presented code identifies the IDENTITY; the device then gets its OWN
// freshly minted token (multi-token DD-1). Re-serving the recovered token
// would put the same key on two devices, which collapses their LastSeenAt into
// one value and makes LRU unable to distinguish them.
func (a *api) reattach(w http.ResponseWriter, r *http.Request, code string) {
	token, err := auth.ParseRecoveryCode(code)
	if err != nil {
		// ★ A malformed code and an unknown one get DIFFERENT answers on
		// purpose. Collapsing them would leave a user who mistyped one
		// character being told their code is wrong, with nothing to act on.
		writeErr(w, http.StatusBadRequest, "RECOVERY_MALFORMED", err.Error())
		return
	}
	uid, ok := a.store.UIDByTokenHash(auth.HashToken(token))
	if !ok {
		writeErr(w, http.StatusNotFound, "RECOVERY_UNKNOWN",
			"這個復原碼查不到對應的身分，請確認是否輸入正確")
		return
	}
	u, _ := a.store.User(uid)
	a.attachDevice(w, r, uid, u.Name)
}

// ---------------------------------------------------------------------------
// Proposals
// ---------------------------------------------------------------------------

type proposalReq struct {
	Title string `json:"title"`
	Desc  string `json:"desc"`
}

// proposalView is the wire shape. Names are joined at render time from the
// roster (DD-10) — never copied into the stored record, so a rename shows up
// everywhere at once.
type proposalView struct {
	PID       string `json:"pid"`
	OwnerUID  string `json:"ownerUid"`
	OwnerName string `json:"ownerName"`
	Title     string `json:"title"`
	Desc      string `json:"desc"`
	CreatedAt string `json:"createdAt"`
	// DecidedOptionID is "" when the proposal is open (DD-5). The frontend
	// needs the value itself, not a boolean: "frozen" and "frozen ON THIS
	// option" are different renderings, and a bool cannot express the second.
	DecidedOptionID string       `json:"decidedOptionId"`
	Options         []optionView `json:"options"`
	// OwnerTakeoverChallenge asks the viewer to prove, by magic link, that they
	// are the owner they share a name with (owner-inheritance).
	//
	// ★ A PROMPT, NOT A GATE (使用者裁示 Q4). Knowing the pid IS the entry
	// credential (R8), so the proposal stays fully readable and votable while
	// this is true. Blocking would punish the genuinely different person who
	// merely shares a name — they would lose access to a trip they were invited
	// to, over an ambiguity that is not their fault. What the challenge decides
	// is only whether they are absorbed into the owner or asked to change name.
	//
	// ★ omitempty so the field is ABSENT for the overwhelming majority of
	// views. A field that is present-and-false on every response invites a
	// frontend to read it as "the check ran and cleared" — which it also would
	// on a build where the detection was never wired at all.
	OwnerTakeoverChallenge bool `json:"ownerTakeoverChallenge,omitempty"`
}

type optionView struct {
	OID        string     `json:"oid"`
	AuthorUID  string     `json:"authorUid"`
	AuthorName string     `json:"authorName"`
	StartDate  string     `json:"startDate"`
	EndDate    string     `json:"endDate"`
	Note       string     `json:"note"`
	Voters     []voterRef `json:"voters"`
	VoteCount  int        `json:"voteCount"`
	// OthersVoteCount drives the frontend's lock hint. It is computed, never
	// stored (risk E) — a stored count is a forgeable one.
	OthersVoteCount int  `json:"othersVoteCount"`
	CanEdit         bool `json:"canEdit"`
	// Decided marks THIS option as the chosen one (DD-5).
	Decided bool `json:"decided"`
	// Yes/No/Score are the two-state tally (DD-6, task 6.8). The counts travel
	// ALONGSIDE the score, never folded into it: an option with no votes reports
	// Yes=0 No=0, distinct from one everyone rejected (Yes=0 No=n).
	Yes   int     `json:"yes"`
	No    int     `json:"no"`
	Score float64 `json:"score"`
	// MyWeight is the viewer's own stance, or nil when they have not responded.
	// A pointer for the same reason as voteReq.Weight: 不行 is 0, and a plain
	// float64 would make "沒表態" render as "表態不行".
	MyWeight *float64 `json:"myWeight"`
}

type voterRef struct {
	UID    string  `json:"uid"`
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
}

// handleCreateProposal is the SINGLE automatic trigger of the email
// requirement (DD-7).
//
// ★ THE GATE IS HERE AND NOWHERE ELSE, WHICH IS THE DESIGN (DD-6). Requiring
// an email at registration would tax every participant — most of whom only
// pick dates — for a property only a proposal OWNER needs: an owner holds a
// resource other people depend on, so they must be reachable and recoverable.
// Someone who votes and leaves needs neither.
//
// ★ IT RUNS BEFORE THE STORE CALL. Checking afterwards would create the
// proposal and then report failure, leaving an owner-less-in-spirit record
// behind while the user was told nothing happened.
func (a *api) handleCreateProposal(w http.ResponseWriter, r *http.Request) {
	id := identify(a.store, r)
	if !id.Registered {
		writeErr(w, http.StatusForbidden, auth.CodeIdentityUnregistered,
			"請先登記姓名才能建立提案")
		return
	}
	// ★ Re-read the roster rather than trusting anything on the request. The
	// address is the gate, so taking it from the caller would let the caller
	// open the gate.
	owner, ok := a.store.User(id.UID)
	if !ok || strings.TrimSpace(owner.Email) == "" {
		writeErr(w, http.StatusForbidden, codeEmailRequiredForProposal,
			"發起提案需先設定 email，請於齒輪設定中補填")
		return
	}
	var req proposalReq
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	pid, err := newID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ID_FAILED", err.Error())
		return
	}
	if err := a.store.CreateProposal(pid, id.UID, strings.TrimSpace(req.Title), req.Desc); err != nil {
		writeErr(w, http.StatusBadRequest, "PROPOSAL_INVALID", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.viewProposal(pid, id))
}

// handleGetProposal serves one proposal by pid.
//
// ★ Readable by anyone holding the pid, registered or not — that IS the access
// model (R8): knowing the URL grants entry. The identity gate applies to
// WRITES; requiring registration to read would break the shared-link flow the
// whole product is built around.
func (a *api) handleGetProposal(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	if _, ok := a.store.Proposal(pid); !ok {
		writeErr(w, http.StatusNotFound, auth.CodeProposalNotFound, "找不到此提案")
		return
	}
	writeJSON(w, http.StatusOK, a.viewProposal(pid, identify(a.store, r)))
}

func (a *api) handleUpdateProposal(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	id := identify(a.store, r)
	if d := auth.Authorize(id.actor(), auth.ActionProposalUpdate,
		auth.Target{PID: pid}, a.store); !d.Allowed {
		writeDeny(w, d)
		return
	}
	var req proposalReq
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if err := a.store.UpdateProposal(pid, strings.TrimSpace(req.Title), req.Desc); err != nil {
		writeErr(w, http.StatusBadRequest, "PROPOSAL_INVALID", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.viewProposal(pid, id))
}

// handleDeleteProposal removes a proposal and everything under it. The cascade
// happens inside one lock in the store (risk I), so no half-deleted tree is
// ever observable — and replay after a restart reproduces exactly that.
func (a *api) handleDeleteProposal(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	id := identify(a.store, r)
	if d := auth.Authorize(id.actor(), auth.ActionProposalDelete,
		auth.Target{PID: pid}, a.store); !d.Allowed {
		writeDeny(w, d)
		return
	}
	if err := a.store.DeleteProposal(pid); err != nil {
		writeErr(w, http.StatusBadRequest, "PROPOSAL_INVALID", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMyProposals is the subject-scoped query (DD-20).
//
// ★ This is NOT an enumeration endpoint. It answers "which proposals am I in?"
// by filtering on the caller's uid inside the store; there is deliberately no
// route that returns all proposals (R8, risk B). The distinction is invisible
// in a response body — which is why the guard is that no such route exists at
// all, not that this one filters.
func (a *api) handleMyProposals(w http.ResponseWriter, r *http.Request) {
	id := identify(a.store, r)
	out := []proposalView{}
	if id.UID != "" {
		for _, p := range a.store.ProposalsForUser(id.UID) {
			out = append(out, a.viewProposal(p.PID, id))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": out})
}

// ---------------------------------------------------------------------------
// Options and votes
// ---------------------------------------------------------------------------

type optionReq struct {
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
	Note      string `json:"note"`
}

func (a *api) handleCreateOption(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	id := identify(a.store, r)
	if d := auth.Authorize(id.actor(), auth.ActionOptionCreate,
		auth.Target{PID: pid}, a.store); !d.Allowed {
		writeDeny(w, d)
		return
	}
	var req optionReq
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	oid, err := newID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ID_FAILED", err.Error())
		return
	}
	if err := a.store.CreateOption(pid, oid, id.UID, req.StartDate, req.EndDate, req.Note); err != nil {
		writeErr(w, optionErrStatus(err), optionErrCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.viewProposal(pid, id))
}

func (a *api) handleUpdateOption(w http.ResponseWriter, r *http.Request) {
	pid, oid := r.PathValue("pid"), r.PathValue("oid")
	id := identify(a.store, r)
	// ★ Authorize BEFORE touching state (3.6). The others'-vote count it reads
	// is computed on the spot under the store's lock, never a stored field.
	if d := auth.Authorize(id.actor(), auth.ActionOptionUpdate,
		auth.Target{PID: pid, OID: oid}, a.store); !d.Allowed {
		writeDeny(w, d)
		return
	}
	var req optionReq
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if err := a.store.UpdateOption(pid, oid, req.StartDate, req.EndDate, req.Note); err != nil {
		writeErr(w, optionErrStatus(err), optionErrCode(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.viewProposal(pid, id))
}

func (a *api) handleDeleteOption(w http.ResponseWriter, r *http.Request) {
	pid, oid := r.PathValue("pid"), r.PathValue("oid")
	id := identify(a.store, r)
	if d := auth.Authorize(id.actor(), auth.ActionOptionDelete,
		auth.Target{PID: pid, OID: oid}, a.store); !d.Allowed {
		writeDeny(w, d)
		return
	}
	if err := a.store.DeleteOption(pid, oid); err != nil {
		writeErr(w, http.StatusBadRequest, "OPTION_INVALID", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.viewProposal(pid, id))
}

// voteReq carries the option being voted on.
//
// ★ There is NO uid field, by design. A uid in the body would be the forgery
// surface TV-5 describes; the server derives it from the token every time, so
// "vote as someone else" is not expressible in the wire format at all.
// decideReq carries the option to freeze on. An EMPTY optionId is the release
// path — the same endpoint both ways, so a client that can freeze can always
// also unfreeze, and there is no state reachable with no way back out.
type decideReq struct {
	OptionID string `json:"optionId"`
}

func (a *api) handleDecide(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	id := identify(a.store, r)
	if d := auth.Authorize(id.actor(), auth.ActionProposalDecide,
		auth.Target{PID: pid}, a.store); !d.Allowed {
		writeDeny(w, d)
		return
	}
	var req decideReq
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if err := a.store.SetDecision(pid, req.OptionID); err != nil {
		writeErr(w, http.StatusBadRequest, "DECISION_INVALID", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.viewProposal(pid, id))
}

// handleRepid rotates the proposal's pid (task 6.9, risk B).
//
// ★ The response carries the NEW pid and the old one becomes a hard 404 — not
// a redirect. A redirect would keep the leaked URL working while reporting
// that it had been revoked, which is worse than not offering revocation at all.
func (a *api) handleRepid(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("pid")
	id := identify(a.store, r)
	if d := auth.Authorize(id.actor(), auth.ActionProposalRepid,
		auth.Target{PID: pid}, a.store); !d.Allowed {
		writeDeny(w, d)
		return
	}
	newPID, err := newID()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ID_GENERATION_FAILED", err.Error())
		return
	}
	if err := a.store.RepID(pid, newPID); err != nil {
		writeErr(w, http.StatusBadRequest, "REPID_INVALID", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.viewProposal(newPID, id))
}

type voteReq struct {
	OptionID string `json:"optionId"`
	// Weight is a POINTER on purpose (DD-6, task 6.8): 不行 is weight 0, and a
	// plain float64 makes "沒帶 weight 欄位" and "明確表態不行" decode to the
	// same 0. A client omitting the field would then be recorded as rejecting
	// the option — the exact opposite of what it meant, with no error anywhere.
	Weight *float64 `json:"weight"`
}

func (a *api) handleSetVote(w http.ResponseWriter, r *http.Request) {
	a.vote(w, r, true)
}

func (a *api) handleDeleteVote(w http.ResponseWriter, r *http.Request) {
	a.vote(w, r, false)
}

func (a *api) vote(w http.ResponseWriter, r *http.Request, set bool) {
	pid := r.PathValue("pid")
	id := identify(a.store, r)

	oid := r.PathValue("oid")
	// Default 可以 (1.0): a client that knows nothing about stances keeps the
	// pre-6.8 behaviour instead of silently recording 不行.
	weight := store.WeightYes
	if oid == "" {
		var req voteReq
		if err := decodeJSON(w, r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
			return
		}
		oid = req.OptionID
		if req.Weight != nil {
			weight = *req.Weight
		}
	}

	action := auth.ActionVoteSet
	if !set {
		action = auth.ActionVoteDelete
	}
	// Target.UID is the caller's own uid, taken from the token. Authorize
	// still checks it matches — two independent barriers, so a future handler
	// that took uid from elsewhere would be refused rather than trusted.
	if d := auth.Authorize(id.actor(), action,
		auth.Target{PID: pid, OID: oid, UID: id.UID}, a.store); !d.Allowed {
		writeDeny(w, d)
		return
	}

	var err error
	if set {
		err = a.store.SetVote(pid, oid, id.UID, weight)
	} else {
		err = a.store.DeleteVote(pid, oid, id.UID)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "VOTE_INVALID", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.viewProposal(pid, id))
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// ownerTakeoverChallenge reports whether this viewer should be asked to prove,
// by magic link, that they are the owner they share a name with.
//
// FOUR ARMS, AND EVERY ONE OF THEM MUST BE ABLE TO ANSWER false FOR ITS OWN
// REASON — a predicate that returns false because it never ran is
// indistinguishable from one that ran and cleared:
//
//	viewer has no identity        -> false. Nothing to challenge; they have no
//	                                 name yet, so no claim to the owner's.
//	viewer IS the owner           -> false. The common case. Comparing names
//	                                 without this would challenge every owner
//	                                 on their own proposal, forever.
//	different name                -> false. Sharing nothing, claiming nothing.
//	owner has NO email            -> false. DD-9 寬容: an owner with no address
//	                                 has nothing to verify against, so the
//	                                 proposal stays claimable by any same-name
//	                                 visitor and there is no link to send.
//	same name, different uid,
//	owner HAS an email            -> TRUE. The only challenging state.
//
// ★ THE UID COMPARISON IS NOT REDUNDANT WITH THE NAME COMPARISON. Names are
// unique on the roster, so "same name" very nearly implies "same identity" —
// which is exactly why omitting the uid test would pass every ordinary test:
// it only diverges in the window this feature exists for, where two rows
// briefly hold one name.
//
// ★ IT READS THE OWNER'S EMAIL, NOT THE VIEWER'S. The question is whether the
// OWNER can be verified against, not whether the viewer happens to have an
// address of their own.
func ownerTakeoverChallenge(viewer identity, owner store.User) bool {
	if viewer.UID == "" || strings.TrimSpace(viewer.Name) == "" {
		return false
	}
	if viewer.UID == owner.UID {
		return false
	}
	if viewer.Name != owner.Name {
		return false
	}
	return strings.TrimSpace(owner.Email) != ""
}

// viewProposal joins stored records against the roster (DD-10).
func (a *api) viewProposal(pid string, viewer identity) proposalView {
	p, ok := a.store.Proposal(pid)
	if !ok {
		return proposalView{PID: pid}
	}
	owner, _ := a.store.User(p.OwnerUID)
	v := proposalView{
		PID: p.PID, OwnerUID: p.OwnerUID, OwnerName: owner.Name,
		Title: p.Title, Desc: p.Desc, CreatedAt: p.CreatedAt,
		DecidedOptionID: p.DecidedOptionID,
		Options:         []optionView{},
		OwnerTakeoverChallenge: ownerTakeoverChallenge(viewer, owner),
	}
	for _, o := range p.Options {
		ov := optionView{
			OID: o.OID, AuthorUID: o.AuthorUID,
			StartDate: o.StartDate, EndDate: o.EndDate, Note: o.Note,
			Voters:          []voterRef{},
			VoteCount:       a.store.VoteCount(pid, o.OID),
			OthersVoteCount: a.store.OthersVoteCount(pid, o.OID),
		}
		if u, ok := a.store.User(o.AuthorUID); ok {
			ov.AuthorName = u.Name
		}
		for _, vote := range p.Votes {
			if vote.OptionID != o.OID {
				continue
			}
			ref := voterRef{UID: vote.UID, Weight: vote.Weight}
			if u, ok := a.store.User(vote.UID); ok {
				ref.Name = u.Name
			}
			ov.Voters = append(ov.Voters, ref)
		}
		// CanEdit is a HINT for the UI, derived from the same Authorize call
		// the write path uses. It is never the protection — the write handler
		// asks again (design.md A6: the frontend hiding a button is
		// convenience, the server decision is the boundary).
		ov.CanEdit = auth.Authorize(viewer.actor(), auth.ActionOptionUpdate,
			auth.Target{PID: pid, OID: o.OID}, a.store).Allowed
		ov.Decided = p.DecidedOptionID == o.OID
		tl := a.store.Tally(pid, o.OID)
		ov.Yes, ov.No, ov.Score = tl.Yes, tl.No, tl.Score
		if v, ok := p.Votes[store.VoteKey(o.OID, viewer.UID)]; ok && viewer.UID != "" {
			w := v.Weight
			ov.MyWeight = &w
		}
		v.Options = append(v.Options, ov)
	}
	sortOptions(v.Options)
	return v
}

// sortOptions gives the response a stable order. Map iteration in Go is
// randomised, so without this the same state would serialise differently on
// every request and a diff-rendering client would see phantom changes.
func sortOptions(o []optionView) {
	for i := 1; i < len(o); i++ {
		for j := i; j > 0 && optionLess(o[j], o[j-1]); j-- {
			o[j], o[j-1] = o[j-1], o[j]
		}
	}
}

func optionLess(a, b optionView) bool {
	if a.StartDate != b.StartDate {
		return a.StartDate < b.StartDate
	}
	if a.EndDate != b.EndDate {
		return a.EndDate < b.EndDate
	}
	return a.OID < b.OID
}

func optionErrCode(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "already exists") && strings.Contains(msg, "range"):
		return "DUPLICATE_OPTION"
	case strings.Contains(msg, "must not be after") || strings.Contains(msg, "range"):
		return "DATE_RANGE_INVERTED"
	}
	return "OPTION_INVALID"
}

func optionErrStatus(err error) int {
	if optionErrCode(err) == "DUPLICATE_OPTION" {
		return http.StatusConflict
	}
	return http.StatusBadRequest
}
