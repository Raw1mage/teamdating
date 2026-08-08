package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"
)

// verifyTokenBytes is 32 bytes = 256 bits (DD-1, same standard as the bearer
// token). A magic link IS a credential for the duration of its life; a shorter
// or predictable value would let it be guessed, and a guessed link binds a
// stranger's device to someone else's identity with no error anywhere.
const verifyTokenBytes = 32

// verifyTTL is how long a magic link stays usable (DD-1).
const verifyTTL = 15 * time.Minute

// maxPending bounds the in-memory table (DD Risks: pending map leak).
//
// ★ A CAP PLUS LAZY EXPIRY, not either alone. Expiry alone leaves a burst of
// requests inside one TTL window unbounded; a cap alone would let stale-but-
// unexpired entries push out fresh ones. Both together mean the table is
// bounded by min(cap, arrivals-per-TTL) and never grows without limit.
const maxPending = 10000

var verifyEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// pendingMerge is one outstanding magic link (DD-4).
//
// ★ IT HOLDS THE uid, AND THE uid NEVER TRAVELS OVER THE WIRE. That is what
// keeps DD-8 intact through this flow: the confirm request carries ONLY the
// opaque token, and the identity it authorises is looked up here, server-side.
// A design that put the uid in the link (or in the confirm body) would let
// anyone bind their device to any identity by editing a query parameter, and
// every legitimate request would still work — so nothing would ever report it.
//
// ★ NOT PERSISTED TO events.jsonl (DD-4). It is a 15-minute secret; an
// append-only log would keep it forever and grow replay cost with every login
// attempt. A restart drops the table and the user simply requests a new link.
type pendingMerge struct {
	// uid is the identity that will receive the new device token.
	uid string
	// email is the address that was verified, stored AS TYPED so the confirm
	// step can persist it (the DD-9 claim path sets it on an account that had
	// none).
	email string
	// name is carried only to render the mail; it is not an authorisation
	// input.
	name string
	// fromUID is the identity the REQUESTER already held when they asked for
	// the link, or "" for a device with no identity yet. On confirm it is
	// absorbed into uid (owner-inheritance, 使用者裁示 Q2c).
	//
	// ★ IT COMES FROM THE COOKIE AT REQUEST TIME, NEVER FROM THE BODY (DD-8),
	// and it is stored HERE rather than re-read at confirm time for two
	// reasons. The link may be clicked in a different browser — phone mail app,
	// desktop session — so the confirming request's cookie is not reliably the
	// requester's. And re-reading it at confirm would let whoever opens the
	// link decide which identity gets absorbed, which is an account-deletion
	// primitive handed to anyone who can read the mail.
	fromUID string
	// claim marks the DD-9 path: the target account had NO email, so what was
	// proven is the claimer's own address, not ownership of the account. It is
	// recorded so the confirm step can log the weaker authorisation rather than
	// letting the two paths become indistinguishable after the fact.
	claim     bool
	expiresAt time.Time
	// used makes the link one-time. It is a FLAG rather than a delete so a
	// second click is answered VERIFY_TOKEN_USED instead of the identical
	// "invalid" a deletion would produce — the user learns the link already
	// worked, rather than believing it never did.
	used bool
}

// pendingTable is the in-memory magic-link store.
type pendingTable struct {
	mu sync.Mutex
	m  map[string]*pendingMerge
	// high is the largest size the table has ever reached.
	//
	// ★ A RATCHET, NOT A GAUGE, AND THAT IS THE ENTIRE POINT. The live size was
	// once published on /api/health, which quietly rebuilt the oracle
	// verify-email refuses to be: a matching address mints an entry and a
	// non-matching one does not, so two reads around a guess answered "is this
	// address on file" — exactly what the uniform response withholds. A value
	// that never decreases cannot be used that way: one probe moves it by at
	// most one and never moves it back, so the difference between two reads
	// carries no per-guess signal once the table has been busy at all.
	//
	// It still answers the operational question the gauge was there for. A sweep
	// that stopped running shows up as a mark climbing toward maxPending, while
	// a working one leaves it parked at the true concurrent peak.
	high int
	// now is the clock, injectable so an expiry test does not have to sleep 15
	// real minutes. Production leaves it nil and time.Now is used.
	//
	// ★ Without this seam TV-3 (expiry rejects) cannot be executed at all, and
	// an expiry check nothing ever exercises is indistinguishable from no
	// expiry check.
	now func() time.Time
}

func newPendingTable() *pendingTable {
	return &pendingTable{m: map[string]*pendingMerge{}}
}

func (p *pendingTable) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// newVerifyToken mints a magic-link token and returns the plaintext with its
// hash. Only the hash is ever stored (DD-1, same rule as bearer tokens).
func newVerifyToken() (plaintext, hash string, err error) {
	b := make([]byte, verifyTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate verify token: %w", err)
	}
	plaintext = verifyEnc.EncodeToString(b)
	return plaintext, hashVerifyToken(plaintext), nil
}

func hashVerifyToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// put records a pending merge under the token hash.
//
// ★ Sweeps expired entries BEFORE the cap check, so a table full of dead
// entries does not refuse a legitimate request. The reverse order fails in the
// worst way: the service starts rejecting logins with a resource error while
// every entry it is protecting has already expired.
func (p *pendingTable) put(hash string, pm *pendingMerge) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked()
	if len(p.m) >= maxPending {
		return fmt.Errorf("too many pending verifications")
	}
	p.m[hash] = pm
	// ★ THE PEAK GOES TO THE OPERATOR LOG, NOT TO ANY HTTP RESPONSE. This is
	// the observability that /api/health used to provide and deliberately no
	// longer does: a mark climbing toward maxPending means the sweep has
	// stopped, and an operator reading the log can see that, while an anonymous
	// caller — who could otherwise diff two health reads around a guessed
	// address — cannot. The signal did not disappear; it moved to a channel
	// that requires host access.
	//
	// Logged only on a NEW peak, so an idle service stays quiet and a genuine
	// climb is visible rather than buried in per-request noise.
	if len(p.m) > p.high {
		p.high = len(p.m)
		log.Printf("verify: pending magic-link table reached a new peak of %d (cap %d)", p.high, maxPending)
	}
	return nil
}

// verifyOutcome is the three-state result of presenting a magic link.
//
// ★ THREE NAMED STATES, NOT (found bool). "no such token", "expired" and
// "already used" are DIFFERENT facts about what happened, and a boolean makes
// them one. The user-facing messages are deliberately similar in tone
// (errors.md: do not turn this endpoint into an oracle for pending state), but
// the SERVER must still know which occurred — otherwise a genuine expiry and a
// forged token are indistinguishable in the logs, and nobody can tell a broken
// clock from an attack.
type verifyOutcome int

const (
	verifyInvalid verifyOutcome = iota
	verifyExpired
	verifyUsed
	verifyOK
)

// consume validates the token and marks it used, atomically.
//
// ★ CHECK AND MARK UNDER ONE LOCK. Splitting them — a lookup, then a separate
// mark — is a check-then-act race: two concurrent clicks on the same link both
// observe used==false and both bind a device. The one-time guarantee would
// hold in every sequential test and fail exactly when someone double-clicks.
func (p *pendingTable) consume(plaintext string) (*pendingMerge, verifyOutcome) {
	hash := hashVerifyToken(plaintext)
	p.mu.Lock()
	defer p.mu.Unlock()
	pm, ok := p.m[hash]
	if !ok {
		return nil, verifyInvalid
	}
	// Used is checked BEFORE expiry: a link that was successfully used and has
	// since aged past its TTL should still say "already used", which is the
	// truthful account of what happened to it.
	if pm.used {
		return nil, verifyUsed
	}
	if !p.clock().Before(pm.expiresAt) {
		return nil, verifyExpired
	}
	pm.used = true
	return pm, verifyOK
}

// sweepLocked drops entries that are expired AND already used, plus anything
// long past its expiry.
//
// ★ A USED entry is kept until its original expiry passes, on purpose. Delete
// it immediately and a second click on the same link reports "invalid" instead
// of "already used" — the user is told the link never worked when in fact it
// did, and will keep requesting new ones.
func (p *pendingTable) sweepLocked() {
	now := p.clock()
	for h, pm := range p.m {
		if !now.Before(pm.expiresAt) {
			delete(p.m, h)
		}
	}
}

// pendingCount reports the LIVE table size.
//
// ★ NOT FOR ANY PUBLIC RESPONSE. This value moves by one when a submitted
// address matches an identity on file and not at all when it does not, so
// exporting it anywhere an anonymous caller can read hands out the membership
// answer verify-email deliberately withholds. It exists for tests (which need
// to observe that an entry was in fact created) and for internal bookkeeping.
//
// ★ THE HIGH-WATER MARK (p.high) IS NOT PUBLISHED EITHER, and the reason is
// worth keeping because a monotonic mark LOOKS like it would be safe there. It
// is not: on an idle service the first correct guess moves it 0→1, which is
// exactly as legible as the live count. Its only reader is the operator log
// line in put, inline — there is deliberately no accessor, because an accessor
// is the first step toward someone wiring it into a response.
func (p *pendingTable) pendingCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.m)
}
