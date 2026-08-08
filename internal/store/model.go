// Package store holds the authoritative in-memory state and its write-ahead
// event log (DD-14). Zero third-party dependencies (DD-13).
//
// Concurrency contract: every field below is guarded by Store.mu. No caller
// outside this package ever receives a reference to a live map — accessors
// return copies (tasks 2.1).
package store

import (
	"strings"
	"time"
)

// TokenRef is one bearer token belonging to a user (multi-token DD-1).
//
// ★ Hash is the hex-encoded SHA-256 of the token (DD-16); the plaintext is
// NEVER stored, neither in memory nor in the log.
//
// ★ LastSeenAt exists so eviction can pick the LEAST RECENTLY USED key rather
// than the oldest-added one. Those two orderings agree on every test that adds
// tokens in usage order, so a mistake here is invisible unless a vector
// deliberately separates them (TV-6).
type TokenRef struct {
	Hash       string `json:"hash"`
	AddedAt    string `json:"addedAt"`
	LastSeenAt string `json:"lastSeenAt"`
}

// User is the roster entry (DD-10). The name lives here and ONLY here;
// proposals and votes store a uid and join at display time, so a rename takes
// effect everywhere at once.
type User struct {
	UID string `json:"uid"`
	// Tokens are the bearer keys for this identity, one per device (DD-1).
	//
	// ★ A SLICE, not a primary field plus extras. The rejected shape kept
	// User.TokenHash as "the main one" with a parallel ExtraTokens list; under
	// it, code that read only the main field passed every test, because the
	// first token is always in the main field. Storing one fact in one place
	// makes "looked at only half of them" inexpressible.
	Tokens []TokenRef `json:"tokens"`
	Name   string     `json:"name"`
	// Email is the authorisation anchor for same-name merge (DD-3) and the
	// proposal gate (DD-7). EMPTY STRING MEANS "NOT SET", and that is a
	// first-class state, not a degenerate one (DD-6): email is deliberately
	// NOT globally mandatory — someone who only picks dates never supplies it.
	//
	// ★ A PLAIN STRING, NOT A *string OR A Verified bool ALONGSIDE. The
	// rejected shape carried `Email string` + `EmailVerified bool`, which makes
	// "set but never verified" expressible — and every merge check would then
	// have to remember to test both. It cannot be stored here unverified: the
	// ONLY writer is SetEmail, and the only caller that reaches it on the merge
	// path runs AFTER the magic-link confirm (DD-1). One field means "has an
	// email" and "has a verified email" cannot drift apart.
	//
	// ★ Compared case-insensitively at the DECISION points (NormalizeEmail),
	// never normalised on the way in, so what the user typed is what is shown
	// back to them.
	Email string `json:"email"`
	// CreatedAt is an RFC3339 wall-clock stamp for audit. This is deliberately
	// NOT a floating date: DD-9 constrains *trip* dates, not system time.
	CreatedAt string `json:"createdAt"`
}

// MaxTokensPerUser bounds how many devices one identity may hold at once.
// Reaching it is a NORMAL path, not an error: the least-recently-used token is
// evicted and the new one accepted (errors.md — there is deliberately no
// TOKEN_LIMIT_REACHED code).
const MaxTokensPerUser = 5

// TouchInterval is the minimum age of LastSeenAt before identify() will spend a
// log write refreshing it (DD-2).
//
// ★ Without throttling, every authenticated request becomes an fsync. With it,
// LRU resolution is hour-grained — which is the honest trade: the purpose is
// "evict the phone nobody has used for months", and that decision is unchanged
// by hour-level imprecision.
const TouchInterval = time.Hour

// clone returns a copy safe to hand outside the package.
//
// ★ EXISTS BECAUSE `*u` STOPPED BEING A COPY. While User held only value
// fields (UID/Name/CreatedAt and the old single TokenHash string), a plain
// dereference in read.go was a complete copy and the package's "callers never
// hold a reference into live state" contract held for free. Adding Tokens
// []TokenRef changed the meaning of that same line without changing the line:
// the slice header is copied, the backing array is SHARED. The caller's copy
// then aliases store memory, so eviction (removeToken compacts in place via
// u.Tokens[:0]) rewrites a User the caller is concurrently reading — a data
// race on the live HTTP path, and a silently mutating "copy" even on one
// goroutine.
//
// Every accessor that hands a User out MUST go through here. A future field
// with reference semantics (map, pointer, slice) has exactly one place to be
// handled, instead of reintroducing the same defect at whichever call site was
// forgotten.
func (u *User) clone() User {
	c := *u
	c.Tokens = append([]TokenRef(nil), u.Tokens...)
	return c
}

// FindToken returns the ref matching hash.
//
// ★ Returns a POINTER into u.Tokens so a caller holding the store lock can
// update LastSeenAt in place. Callers outside this package receive User copies
// (read.go), so the pointer they get points into their own copy — mutating it
// cannot corrupt store state.
func (u *User) FindToken(hash string) (*TokenRef, bool) {
	for i := range u.Tokens {
		if u.Tokens[i].Hash == hash {
			return &u.Tokens[i], true
		}
	}
	return nil, false
}

// removeToken drops the ref with the given hash, preserving the order of the
// rest. Unexported: eviction is a store-level decision that must also update
// byToken, and an exported half-operation would let a caller leave the index
// pointing at a token the user no longer holds.
func (u *User) removeToken(hash string) {
	out := u.Tokens[:0]
	for _, t := range u.Tokens {
		if t.Hash != hash {
			out = append(out, t)
		}
	}
	u.Tokens = out
}

// lruTokenHash returns the hash of the least-recently-seen token.
//
// ★ Ordered by LastSeenAt, NOT AddedAt (DD-2). The two agree whenever tokens
// are used in the order they were added, which is what every naive test does —
// so picking the wrong field yields correct-looking eviction counts and evicts
// the wrong device. TV-6 is the only vector that separates them.
//
// Ties break on Hash so the choice is deterministic: a map-order-dependent
// eviction would replay differently than it ran.
func (u *User) lruTokenHash() string {
	if len(u.Tokens) == 0 {
		return ""
	}
	best := 0
	for i := 1; i < len(u.Tokens); i++ {
		switch {
		case u.Tokens[i].LastSeenAt < u.Tokens[best].LastSeenAt:
			best = i
		case u.Tokens[i].LastSeenAt == u.Tokens[best].LastSeenAt &&
			u.Tokens[i].Hash < u.Tokens[best].Hash:
			best = i
		}
	}
	return u.Tokens[best].Hash
}

// Proposal is the access unit (R8): knowing the pid is what grants entry.
type Proposal struct {
	PID      string             `json:"pid"`
	OwnerUID string             `json:"ownerUid"`
	Title    string             `json:"title"`
	Desc     string             `json:"desc"`
	Options  map[string]*Option `json:"options"`
	// Votes is keyed by the deterministic composite key {oid}__{uid} (DD-8).
	// One-person-one-vote and no-proxy-voting are properties of the key
	// structure, not invariants needing a transaction.
	Votes map[string]*Vote `json:"votes"`
	// DecidedOptionID is M2 (DD-5). Empty string means "not decided" —
	// a *string would panic on nil deref; the zero value is correct here.
	DecidedOptionID string `json:"decidedOptionId"`
	CreatedAt       string `json:"createdAt"`
}

// Option is a candidate date range inside a proposal. Adding one is NOT a vote.
type Option struct {
	OID       string `json:"oid"`
	AuthorUID string `json:"authorUid"`
	// StartDate/EndDate are floating dates "YYYY-MM-DD" (DD-9). They are never
	// converted through time.Time: a timezone-bearing value makes members in
	// different zones see an off-by-one day, and that defect is invisible in
	// single-timezone local development.
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"` // inclusive
	Note      string `json:"note"`
}

// Vote is one participant's stance on one option.
type Vote struct {
	OptionID string `json:"optionId"`
	UID      string `json:"uid"`
	// Weight is the stance (DD-6, task 6.8): 1 = 可以, 0 = 不行.
	//
	// ★ A vote with Weight 0 is still a VOTE — the row exists. "Said 不行" and
	// "has not responded" are different facts and must not collapse into one:
	// the proposer needs to tell "three people rejected this" from "three people
	// have not looked at it yet", and a scheme that dropped zero-weight rows
	// would report the same number for both.
	Weight    float64 `json:"weight"`
	CreatedAt string  `json:"createdAt"`
}

// Stance weights (DD-6). The set is closed: an arbitrary float would let a
// client send 0.9 and quietly out-rank everyone else's 可以.
const (
	WeightYes = 1.0 // 可以
	WeightNo  = 0.0 // 不行
)

// ValidWeight reports whether w is one of the two stances.
//
// ★ Compared against the named constants, not a range check. A range would
// accept 0.9, and the resulting ranking defect is invisible in any test that
// only ever sends the legal values.
func ValidWeight(w float64) bool {
	return w == WeightYes || w == WeightNo
}

// VoteKey builds the deterministic composite key {oid}__{uid} (DD-8).
func VoteKey(oid, uid string) string { return oid + "__" + uid }

// Field length limits from data-schema.json.
const (
	MaxNameLen  = 16
	MaxTitleLen = 40
	MaxDescLen  = 300
	MaxNoteLen  = 30
	// MaxEmailLen bounds the stored address. RFC 5321 caps a path at 256
	// octets; this is the practical ceiling and exists so a 64KB body cannot
	// park an arbitrary blob in the roster.
	MaxEmailLen = 254
)

// NormalizeEmail folds an address for COMPARISON only (DD-3).
//
// ★ ITS RESULT IS NEVER STORED. The domain half of an address is
// case-insensitive by RFC 5321 and effectively every real mail host treats the
// local half that way too, so "Luo@Ex.com" and "luo@ex.com" must match at the
// merge decision — otherwise the same person typing a capital letter is routed
// to the suffix branch and ends up as 駱子逸-1, permanently split from their
// own proposals with no error anywhere. Storing the folded form instead would
// echo a mangled address back at the user, so folding happens at the decision
// and the typed value is what is kept.
func NormalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}

// SameEmail reports whether two addresses denote the same mailbox for the
// purposes of DD-3's merge/suffix split.
//
// ★ AN EMPTY ADDRESS MATCHES NOTHING, INCLUDING ANOTHER EMPTY ONE. Without
// this line, two users who both have no email would compare EQUAL, and the
// same-name branch would treat "neither of us has an email" as "we are proven
// to be the same person" — handing anyone who types a matching name the merge
// path that email verification exists to gate. The claim of an unset email is
// not a claim at all; DD-9's old-account claim is a SEPARATE, explicitly
// knowingly-risky path that goes through the magic link, not through here.
func SameEmail(a, b string) bool {
	na, nb := NormalizeEmail(a), NormalizeEmail(b)
	if na == "" || nb == "" {
		return false
	}
	return na == nb
}
