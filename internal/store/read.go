package store

import (
	"sort"
	"strings"
)

// Read accessors. Every one of these takes s.mu and returns COPIES — no caller
// outside this package ever holds a reference into the live maps, which is what
// makes "no handler touches a bare map" (tasks 2.1) enforceable rather than a
// convention.
//
// ★ "COPY" MEANS DEEP FOR ANY REFERENCE-TYPED FIELD, and that is a live
// obligation, not a description. `return *u` was a complete copy only while
// User held nothing but value fields; adding Tokens []TokenRef made the same
// line share a backing array with the store, so a caller's "copy" aliased
// live memory and eviction rewrote it mid-read (a data race on the HTTP
// identify path). Nothing in the type system flagged that — the accessor
// signature did not change. Hand Users out via u.clone() and nothing else; a
// new reference-typed field on User must be handled there before it can
// silently break this paragraph again.

// UIDByTokenHash resolves a token hash to a uid. The caller is responsible for
// the constant-time comparison of the token itself (DD-16); this lookup is on
// the hash, which is not secret.
func (s *Store) UIDByTokenHash(hash string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	uid, ok := s.byToken[hash]
	return uid, ok
}

// User returns a copy of the roster entry.
func (s *Store) User(uid string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[uid]
	if !ok {
		return User{}, false
	}
	return u.clone(), true
}

// UsersByName returns EVERY registered user whose display name matches
// exactly, oldest first. It backs the "name is the recovery key" convenience:
// a caller who types a name already on the roster reattaches to that identity
// instead of minting a new one.
//
// ★ Name is NOT a unique key here — by design. Two people who both type "小明"
// are deliberately allowed to share one identity (the product owner accepted
// the name-collision risk explicitly: convenience over isolation, no sensitive
// data). A blank name never matches — an unregistered roster entry has an
// empty Name, and returning it would let anyone reattach to a nameless stub.
//
// ★ IT RETURNS ALL OF THEM, AND THAT REPLACED A SINGLE-VALUE UIDByName THAT
// RETURNED THE FIRST. The old accessor's own comment argued the choice did not
// matter — "every registered user with this exact name is an equivalent target
// under the collision policy" — and that reasoning was WRONG IN PRODUCTION, in
// the one case it had to be right. Identities created BEFORE name-reattach
// existed can already be split across several uids for one person: measured on
// live data, one human held two, only one of which owned any proposals. Map
// iteration is randomised, so reattaching picked between them by coin flip and
// half the time landed on the identity with nothing in it — the user saw an
// empty app and no error anywhere. Equivalence held for the token model and
// never held for the CONTENT hanging off the uid.
//
// So the caller must be handed the whole set and let a human pick. Returning
// one and calling it equivalent is the defect, not an implementation detail.
//
// ★ Sorted, because map order is randomised and an unstable list would reorder
// itself under the user's finger between one request and the next. Oldest
// first puts the original identity at the top, which is the one a returning
// user most often wants.
func (s *Store) UsersByName(name string) []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []User{}
	if strings.TrimSpace(name) == "" {
		return out
	}
	for _, u := range s.users {
		if u.Name == name {
			// clone(), not *u: Tokens is a slice, so a plain dereference would
			// hand the caller a header sharing the store's backing array.
			out = append(out, u.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// UserByNameAndEmail returns the same-name identity whose email matches, and
// reports whether exactly one was found (DD-3 step 3: the merge/suffix split).
//
// ★ THE SPLIT IS DECIDED ON EMAIL EQUALITY, NEVER ON NAME EQUALITY. Sharing a
// name is what makes two identities CANDIDATES; it is not evidence they are
// the same person, which is the whole gap this plan closes. A caller that
// branched on len(UsersByName(name)) > 0 alone would hand the merge path to
// anyone who types a name already on the roster.
//
// ★ AN UNSET EMAIL MATCHES NOTHING (store.SameEmail). Without that, every
// pre-email account would compare equal to every other one on the "" value,
// and a stranger supplying no email would be merged into the first legacy
// identity with the same name — silently, with full管理權. DD-9's claim of an
// email-less old account is a SEPARATE path that still requires the claimer to
// prove their OWN address via the magic link; it does not come through here.
//
// ★ It returns (User, bool) rather than a slice because more than one match is
// not a normal state — two identities holding the same name AND the same email
// would already be a merge that should have happened. Reporting false in that
// case sends the caller to the explicit-choice path instead of picking one by
// map order, which is precisely the coin flip UsersByName was written to end.
func (s *Store) UserByNameAndEmail(name, email string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if strings.TrimSpace(name) == "" || NormalizeEmail(email) == "" {
		return User{}, false
	}
	var found *User
	for _, u := range s.users {
		if u.Name != name || !SameEmail(u.Email, email) {
			continue
		}
		if found != nil {
			// Ambiguous: refuse rather than choose. See the note above.
			return User{}, false
		}
		found = u
	}
	if found == nil {
		return User{}, false
	}
	return found.clone(), true
}

// UsersByNameWithoutEmail returns the same-name identities that have NO email
// set — the DD-9 claimable set.
//
// ★ SEPARATE FROM UserByNameAndEmail ON PURPOSE, because they answer opposite
// questions and collapsing them would hide which one authorised a merge. This
// one is the KNOWINGLY WEAK path: an account with no email cannot be
// distinguished from an impostor who merely knows the name, because there is
// nothing on the account to compare against (DD-9, accepted risk, recorded so
// a later reader does not mistake it for an oversight). What the claimer does
// prove is that THEIR OWN address is reachable; that is not proof of
// ownership, and this comment is the only place that says so.
func (s *Store) UsersByNameWithoutEmail(name string) []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []User{}
	if strings.TrimSpace(name) == "" {
		return out
	}
	for _, u := range s.users {
		if u.Name == name && NormalizeEmail(u.Email) == "" {
			out = append(out, u.clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// Users returns a copy of the whole roster. This is the denominator for
// "who hasn't voted yet" (DD-10) — without a roster that question has no
// answer at all.
func (s *Store) Users() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u.clone())
	}
	return out
}

func (s *Store) UserCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

// TokenStats reports the token-population figures behind the health endpoint
// (observability.md).
//
// ★ One call returning both numbers, not two accessors. Taken separately the
// two would be read under different lock acquisitions, so Total could describe
// a moment AtLimit does not — and "average devices per user" computed from a
// mismatched pair is wrong in a way no error reports.
type TokenStats struct {
	// Total is the sum over all users. Against UserCount() it gives "average
	// devices per person"; a ratio approaching MaxTokensPerUser means the cap
	// is too tight.
	Total int `json:"total"`
	// AtLimit is how many users hold the maximum. This is the ONLY empirical
	// evidence about whether the cap is set correctly: constantly zero means it
	// has never engaged, and a large share means users are being evicted often.
	AtLimit int `json:"atLimit"`
}

func (s *Store) TokenStats() TokenStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var st TokenStats
	for _, u := range s.users {
		st.Total += len(u.Tokens)
		if len(u.Tokens) >= MaxTokensPerUser {
			st.AtLimit++
		}
	}
	return st
}

// Evictions is how many tokens LRU has evicted since this process started.
//
// ★ Deliberately process-local and reset by a restart. An eviction is a
// user-visible harm (a device silently logged out), so the number worth
// watching is "how often is this happening lately". The lifetime total is
// already recoverable from the log — every user_add_token carries the hash it
// evicted — so persisting a second copy would only create a field that can
// disagree with the log.
func (s *Store) Evictions() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.evictions
}

// Proposal returns a deep copy of the proposal subtree.
func (s *Store) Proposal(pid string) (Proposal, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.proposals[pid]
	if !ok {
		return Proposal{}, false
	}
	return *deepCopy(p), true
}

// ProposalSnapshot returns a proposal copy and its commit sequence under the
// writer lock, so an SSE subscriber can replay every later event safely.
func (s *Store) ProposalSnapshot(pid string) (Proposal, uint64, bool) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.proposals[pid]
	if !ok {
		return Proposal{}, s.seq, false
	}
	return *deepCopy(p), s.seq, true
}

func deepCopy(p *Proposal) *Proposal {
	c := *p
	c.Options = make(map[string]*Option, len(p.Options))
	for k, v := range p.Options {
		o := *v
		c.Options[k] = &o
	}
	c.Votes = make(map[string]*Vote, len(p.Votes))
	for k, v := range p.Votes {
		vv := *v
		c.Votes[k] = &vv
	}
	return &c
}

// ProposalsForUser returns the proposals uid owns or has voted in (DD-20).
//
// ★ This is a subject-scoped query, NOT an enumeration endpoint. There is
// deliberately no Proposals() that returns everything: R8's "you need the URL"
// is structural precisely because the backend cannot list them (risk B).
func (s *Store) ProposalsForUser(uid string) []Proposal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Proposal{}
	for _, p := range s.proposals {
		if p.OwnerUID == uid {
			out = append(out, *deepCopy(p))
			continue
		}
		for _, v := range p.Votes {
			if v.UID == uid {
				out = append(out, *deepCopy(p))
				break
			}
		}
	}
	return out
}

// ProposalOwner returns the owning uid of a proposal.
//
// ★ Deliberately narrower than Proposal(): authorisation needs one string, and
// Proposal() deep-copies every option and vote to hand it over. Beyond the
// waste, the wide accessor lets an authorisation bug reach data it has no
// business reading. The narrow return type is the guard — it cannot express
// the wrong thing.
func (s *Store) ProposalOwner(pid string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.proposals[pid]
	if !ok {
		return "", false
	}
	return p.OwnerUID, true
}

// OptionAuthor returns the uid that added an option. Same narrowing rationale
// as ProposalOwner.
// DecidedOption returns the frozen option id and whether the proposal exists.
//
// ★ Two return values, not one. Collapsing "no such proposal" into the empty
// string would make a nonexistent pid read as "open for writing", and the
// freeze gate in auth would wave it through to a store call that then fails for
// an unrelated reason — turning a clear 404 into a confusing 400.
func (s *Store) DecidedOption(pid string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.proposals[pid]
	if !ok {
		return "", false
	}
	return p.DecidedOptionID, true
}

func (s *Store) OptionAuthor(pid, oid string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.proposals[pid]
	if !ok {
		return "", false
	}
	o, ok := p.Options[oid]
	if !ok {
		return "", false
	}
	return o.AuthorUID, true
}

// AllProposalsAdminOnly returns every proposal. It is the ONE enumeration in
// the codebase and exists solely for the loopback-bound admin route (DD-3).
//
// ★ The name is the guard. R8's "you need the URL to get in" is structural
// precisely because no public handler can list proposals (risk B); an
// innocuously-named Proposals() would eventually get wired into a public
// endpoint by someone who did not know that, and nothing would fail. Any
// caller of this outside the admin route is a defect.
func (s *Store) AllProposalsAdminOnly() []Proposal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Proposal, 0, len(s.proposals))
	for _, p := range s.proposals {
		out = append(out, *deepCopy(p))
	}
	return out
}

func (s *Store) ProposalCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.proposals)
}

// VoteCount is the total number of votes on an option.
func (s *Store) VoteCount(pid, oid string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.proposals[pid]
	if !ok {
		return 0
	}
	n := 0
	for _, v := range p.Votes {
		if v.OptionID == oid {
			n++
		}
	}
	return n
}

// OthersVoteCount counts votes on an option EXCLUDING the option author's own.
//
// ★ Counted here, on the spot, under the lock — never materialised into a
// writable field. The Firestore-era design had to store it, and a stored count
// is a forgeable one (risk E). It is also what separates TV-2 from TV-3: an
// implementation that used the total instead cannot tell those two apart.
func (s *Store) OthersVoteCount(pid, oid string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.proposals[pid]
	if !ok {
		return 0
	}
	o, ok := p.Options[oid]
	if !ok {
		return 0
	}
	n := 0
	for _, v := range p.Votes {
		if v.OptionID == oid && v.UID != o.AuthorUID {
			n++
		}
	}
	return n
}

// StanceTally is the two-state breakdown of one option (DD-6, task 6.8).
//
// ★ The counts are reported SEPARATELY from Score, not derived from it. "Said
// 不行" and "never responded" are different facts: No counts explicit 0-weight
// votes, and an option with no votes at all reports Yes=0 No=0 rather than
// pretending everyone rejected it.
type StanceTally struct {
	Yes   int     `json:"yes"`
	No    int     `json:"no"`
	Score float64 `json:"score"`
}

// Tally sums the stances on one option. Computed under the lock, never stored —
// a materialised score is a forgeable one (risk E, same rationale as
// OthersVoteCount).
func (s *Store) Tally(pid, oid string) StanceTally {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var t StanceTally
	p, ok := s.proposals[pid]
	if !ok {
		return t
	}
	for _, v := range p.Votes {
		if v.OptionID != oid {
			continue
		}
		// A zero-weight vote still counts as a response: it lands in No, and
		// Score is unaffected. Dropping it would make "said 不行" indistinguish-
		// able from "never responded".
		switch v.Weight {
		case WeightYes:
			t.Yes++
		default:
			t.No++
		}
		t.Score += v.Weight
	}
	return t
}

// HasVote reports whether uid has voted on oid.
func (s *Store) HasVote(pid, oid, uid string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.proposals[pid]
	if !ok {
		return false
	}
	return p.Votes[VoteKey(oid, uid)] != nil
}

// Option returns a copy of one option.
func (s *Store) Option(pid, oid string) (Option, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.proposals[pid]
	if !ok {
		return Option{}, false
	}
	o, ok := p.Options[oid]
	if !ok {
		return Option{}, false
	}
	return *o, true
}
