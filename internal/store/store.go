package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// append writes one event to the log and fsyncs it.
//
// ★ This is the write-ahead half of DD-14. commit() calls it BEFORE touching
// memory. If the process dies between the fsync and the memory write, replay
// re-applies the event — the user-visible outcome is "the write happened",
// which is what the user was told. The reverse order loses the record (TV-12).
func (s *Store) append(typ string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", typ, err)
	}
	ev := Event{T: time.Now().UTC().Format(time.RFC3339Nano), Type: typ, D: raw}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", typ, err)
	}
	line = append(line, '\n')

	if s.log == nil {
		return fmt.Errorf("event log is closed")
	}
	if _, err := s.log.Write(line); err != nil {
		return fmt.Errorf("append %s: %w", typ, err)
	}
	// Without Sync the bytes sit in the page cache and a kill -9 loses them,
	// which makes the whole write-ahead ordering pointless.
	if err := s.log.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", typ, err)
	}
	s.sinceCompact++
	return nil
}

// commit performs one mutation under the write-ahead contract.
//
// ★ Three properties, and the third is the one that is easy to lose:
//
//  1. writeMu serialises ALL mutations, so validate and apply are atomic with
//     respect to other writers. Without it, two concurrent callers can both
//     pass a "does this already exist?" check and both land — a check-then-act
//     race that no amount of locking *inside* each step would fix.
//  2. The event is durable before memory changes (DD-14).
//  3. apply returns nothing, ON PURPOSE. Once the event is in the log, replay
//     WILL apply it on the next start; an apply that could fail here would
//     produce a state where memory rejected a record that disk accepted, and
//     the divergence would only surface after a restart. Everything that can
//     be refused must be refused in validate, before the append.
//
// Readers are only excluded for the duration of apply, never across the fsync.
func (s *Store) commit(typ string, payload any, validate func() error, apply func()) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if validate != nil {
		s.mu.RLock()
		err := validate()
		s.mu.RUnlock()
		if err != nil {
			return err
		}
	}

	if s.unsafeOrder {
		// CONTROL GROUP ONLY (TV-12). Memory first, log second: a kill -9
		// landing in between loses a write the caller was told succeeded.
		s.mu.Lock()
		apply()
		s.mu.Unlock()
		s.fireCrashHook()
		if err := s.append(typ, payload); err != nil {
			return err
		}
		s.notify(typ, payload)
		return nil
	}

	// Production order: durable first, visible second.
	if err := s.append(typ, payload); err != nil {
		return err
	}
	s.fireCrashHook()
	s.mu.Lock()
	apply()
	s.mu.Unlock()
	s.notify(typ, payload)
	return nil
}

// notify assigns the commit sequence number and hands the event to the
// observer. Called under writeMu, after apply, on the success path only
// (DD-21).
//
// ★ A failed append must NOT advance seq: seq is the client's proof that it
// has seen every committed event, so burning a number on a mutation that never
// landed would make a correct client believe it had missed one.
func (s *Store) notify(typ string, payload any) {
	s.seq++
	if s.observer == nil {
		return
	}
	// Marshal errors cannot occur here: append already marshalled this exact
	// payload successfully. Dropping the event on an impossible error is still
	// better than panicking inside the writer lock.
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.observer(s.seq, Event{Type: typ, D: raw})
}

// Seq returns the number of committed mutations. The SSE handshake reads it
// while taking a snapshot so it can tell which buffered events to replay.
func (s *Store) Seq() uint64 {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.seq
}

// fireCrashHook marks the window between the two steps of a commit. It is a
// no-op unless a control-group harness installed a hook.
func (s *Store) fireCrashHook() {
	if s.crashHook != nil {
		s.crashHook()
	}
}

// ---------------------------------------------------------------------------
// Mutators. Static argument checks run first; anything that depends on current
// state goes in validate, which commit runs under the writer lock.
// ---------------------------------------------------------------------------

// CreateUser registers a roster entry. tokenHash is the hex SHA-256 of the
// bearer token; the plaintext token never reaches this package (DD-16).
func (s *Store) CreateUser(uid, tokenHash, name string) error {
	if uid == "" || tokenHash == "" {
		return fmt.Errorf("uid and tokenHash are required")
	}
	if len([]rune(name)) > MaxNameLen {
		return fmt.Errorf("name exceeds %d characters", MaxNameLen)
	}
	d := UserCreateData{UID: uid, TokenHash: tokenHash, Name: name,
		CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	return s.commit(EvUserCreate, d,
		func() error {
			if _, exists := s.users[uid]; exists {
				return fmt.Errorf("user %q already exists", uid)
			}
			if _, exists := s.byToken[tokenHash]; exists {
				return fmt.Errorf("token hash already registered")
			}
			return nil
		},
		func() {
			s.users[uid] = &User{
				UID: uid, Name: name, CreatedAt: d.CreatedAt,
				Tokens: []TokenRef{{Hash: tokenHash, AddedAt: d.CreatedAt, LastSeenAt: d.CreatedAt}},
			}
			s.byToken[tokenHash] = uid
		})
}

// AddToken grants an additional bearer token to an existing identity — one
// more device for the same person (multi-token DD-1).
//
// ★ APPEND, never replace. An implementation that assigned a fresh one-element
// slice would satisfy "the new device works" completely, and only the OLD
// device would stop working — silently, on someone else's phone, with no error
// on either side. TV-2 exists solely to separate those two implementations.
//
// ★ When the user is already at MaxTokensPerUser, the least-recently-used token
// is evicted and the new one accepted. Reaching the cap is a NORMAL path, not
// an error (errors.md): the product decision is LRU eviction, not refusal.
//
// The eviction is chosen in validate and CARRIED IN THE EVENT (DD-6), so replay
// reproduces the decision instead of recomputing it — see UserAddTokenData.
func (s *Store) AddToken(uid, tokenHash string) error {
	if uid == "" || tokenHash == "" {
		return fmt.Errorf("uid and tokenHash are required")
	}
	d := UserAddTokenData{UID: uid, Hash: tokenHash,
		AddedAt: time.Now().UTC().Format(time.RFC3339)}
	return s.commit(EvUserAddToken, &d,
		func() error {
			u, ok := s.users[uid]
			if !ok {
				return fmt.Errorf("unknown user %q", uid)
			}
			// Global uniqueness, not per-user: byToken maps a hash to exactly
			// one uid, so letting two identities share a hash would make the
			// index arbitrarily resolve one of them.
			if owner, exists := s.byToken[tokenHash]; exists {
				_ = owner
				return fmt.Errorf("token hash already registered")
			}
			// Decide the eviction HERE, under the writer lock, so the choice and
			// the write are atomic with respect to other writers (DD-6).
			if len(u.Tokens) >= MaxTokensPerUser {
				d.EvictedHash = u.lruTokenHash()
			}
			return nil
		},
		func() {
			u := s.users[uid]
			// Both ends move together: the slice is the record, byToken is the
			// lookup. Updating one without the other leaves an evicted token
			// still authenticating, or a live token that cannot be found.
			if d.EvictedHash != "" {
				u.removeToken(d.EvictedHash)
				delete(s.byToken, d.EvictedHash)
				s.evictions++
			}
			u.Tokens = append(u.Tokens, TokenRef{
				Hash: tokenHash, AddedAt: d.AddedAt, LastSeenAt: d.AddedAt,
			})
			s.byToken[tokenHash] = uid
		})
}

// RevokeToken removes one bearer token from an identity.
//
// No HTTP route reaches this yet — device management is a later plan. It exists
// now because the event type must be in the replay switch from the first
// release that could ever write one: a log containing a type the binary does
// not know refuses to start (ErrUnknownEventType), by design.
func (s *Store) RevokeToken(uid, tokenHash string) error {
	return s.commit(EvUserRevokeToken, UserRevokeTokenData{UID: uid, Hash: tokenHash},
		func() error {
			u, ok := s.users[uid]
			if !ok {
				return fmt.Errorf("unknown user %q", uid)
			}
			if _, ok := u.FindToken(tokenHash); !ok {
				return fmt.Errorf("user %q does not hold that token", uid)
			}
			return nil
		},
		func() {
			s.users[uid].removeToken(tokenHash)
			delete(s.byToken, tokenHash)
		})
}

// TouchTokenIfStale refreshes a token's LastSeenAt, at most once per
// TouchInterval, and reports whether it wrote an event.
//
// ★ THE READ PATH MUST NOT BECOME A WRITE PATH (DD-7). identify() runs on every
// authenticated request; appending an event each time would put an fsync on the
// read path and grow the log with traffic. So the staleness test happens under
// RLock and the overwhelming majority of calls return here, having taken no
// writer lock and written nothing.
//
// ★ The value must reach the LOG, not just memory (DD-2). A LastSeenAt kept
// only in RAM comes back from a restart equal to AddedAt, so LRU silently
// degrades to FIFO — correct-looking eviction counts, wrong device evicted, no
// error anywhere.
//
// Errors are returned but callers on the read path deliberately ignore them: a
// failed touch must not fail the request it rode in on.
func (s *Store) TouchTokenIfStale(uid, tokenHash string) (bool, error) {
	now := time.Now().UTC()

	s.mu.RLock()
	u, ok := s.users[uid]
	var last string
	if ok {
		if ref, found := u.FindToken(tokenHash); found {
			last = ref.LastSeenAt
		} else {
			ok = false
		}
	}
	s.mu.RUnlock()
	if !ok {
		return false, nil
	}
	if !staleEnough(last, now) {
		return false, nil
	}

	d := UserTouchTokenData{UID: uid, Hash: tokenHash, SeenAt: now.Format(time.RFC3339)}
	err := s.commit(EvUserTouchToken, d,
		func() error {
			u, ok := s.users[uid]
			if !ok {
				return fmt.Errorf("unknown user %q", uid)
			}
			ref, found := u.FindToken(tokenHash)
			if !found {
				return fmt.Errorf("user %q does not hold that token", uid)
			}
			// Re-check under the writer lock: two requests can both pass the
			// RLock test above and would otherwise both append.
			if !staleEnough(ref.LastSeenAt, now) {
				return errTouchNotDue
			}
			return nil
		},
		func() {
			if ref, found := s.users[uid].FindToken(tokenHash); found {
				ref.LastSeenAt = d.SeenAt
			}
		})
	if errors.Is(err, errTouchNotDue) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// errTouchNotDue aborts a touch that another request already performed. It is
// an internal sentinel, never surfaced: losing a race here is the throttle
// working, not a failure.
var errTouchNotDue = errors.New("touch not due")

// staleEnough reports whether last is older than TouchInterval before now.
//
// ★ An UNPARSEABLE timestamp returns true — refresh it. The alternative,
// treating it as fresh, would freeze a corrupt value forever and make that
// token permanently un-evictable, since LRU orders by exactly this field.
func staleEnough(last string, now time.Time) bool {
	if last == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, last)
	if err != nil {
		return true
	}
	return now.Sub(t) >= TouchInterval
}

// RenameUser changes the display name. Because the name lives only in the
// roster (DD-10), everything rendered by joining picks this up at once — no
// historical record carries a stale copy.
// RenameUser changes a display name, refusing one that another identity
// already holds.
//
// ★ THE UNIQUENESS TEST LIVES INSIDE THE VALIDATE CLOSURE, NOT IN THE CALLER.
// An HTTP handler that asks NameTaken and then calls this is a check-then-act
// race: the question and the write take different locks, so N concurrent
// renames onto one free name all read "free" and all succeed — every response
// 200, and the roster ends up holding N identities with the same name. That is
// precisely the duplicate the suffix allocator exists to prevent, arriving
// through the rename door instead, and no sequential test can see it because
// sequentially the check is correct.
//
// commit() runs validate under the same critical section as the write, which is
// what makes the decision and the mutation one indivisible step — the same
// shape CreateUserWithSuffix uses to allocate a suffix safely.
func (s *Store) RenameUser(uid, name string) error {
	if len([]rune(name)) > MaxNameLen {
		return fmt.Errorf("name exceeds %d characters", MaxNameLen)
	}
	return s.commit(EvUserRename, UserRenameData{UID: uid, Name: name},
		func() error {
			u, ok := s.users[uid]
			if !ok {
				return fmt.Errorf("unknown user %q", uid)
			}
			// Renaming to the name already held is a no-op, not a collision
			// with oneself; refusing it would break idempotent re-submits.
			if u.Name == name {
				return nil
			}
			for other, ou := range s.users {
				if other != uid && ou.Name == name {
					return ErrNameTaken
				}
			}
			return nil
		},
		func() { s.users[uid].Name = name })
}

// SetEmail records (or clears, with an empty string) an identity's email.
//
// ★ THE VERIFICATION HAPPENS ABOVE THIS LAYER, AND THAT IS DELIBERATE. The
// store's job is durability, not protocol: the magic-link confirm (DD-1) is
// what decides an address is provable, and it calls this only after the token
// checks out. Putting a "verified" flag here instead would create a state —
// stored-but-unverified — that every later reader would have to remember to
// test, and forgetting the test produces a silent authorisation hole rather
// than a failure.
//
// ★ An empty email is CLEARING, not a no-op. "Never set" and "deliberately
// removed" reach the same observable state on purpose (DD-6: unset is a normal
// state, not a degenerate one), but the write itself must land so a restart
// does not resurrect the old address and keep the proposal gate open.
func (s *Store) SetEmail(uid, email string) error {
	if uid == "" {
		return fmt.Errorf("uid is required")
	}
	email = strings.TrimSpace(email)
	if len(email) > MaxEmailLen {
		return fmt.Errorf("email exceeds %d characters", MaxEmailLen)
	}
	// A non-empty address must parse. The check runs BEFORE the append so a
	// malformed value can never reach the log: the log is replayed on every
	// start, and a line that memory rejects but disk accepted is exactly the
	// divergence commit()'s contract exists to prevent.
	if email != "" {
		if err := ValidateEmail(email); err != nil {
			return err
		}
	}
	return s.commit(EvUserSetEmail, UserSetEmailData{UID: uid, Email: email},
		func() error {
			if _, ok := s.users[uid]; !ok {
				return fmt.Errorf("unknown user %q", uid)
			}
			// ★ INSIDE THE VALIDATE CLOSURE, atomically with the write, for
			// the same reason RenameUser's name check is: an exported
			// EmailTaken() that a handler called first would be check-then-act
			// — two identities claiming one address concurrently would each
			// observe "free" and each succeed, and the invariant would fail
			// with no error on either request. Deciding here, under writeMu,
			// makes that interleaving unrepresentable.
			if s.emailTakenByOtherLocked(uid, email) {
				return ErrEmailTaken
			}
			return nil
		},
		func() { s.users[uid].Email = email })
}

// MaxNameSuffix bounds the search for a free `name-N` (DD-3).
//
// ★ Bounded so the allocator cannot spin forever under the writer lock, and
// exhaustion is REPORTED (ErrNameSuffixExhausted) rather than silently
// falling back to a random or appended-timestamp name. A silent fallback would
// satisfy the uniqueness invariant while handing the user a name they never
// chose and cannot predict.
const MaxNameSuffix = 999

// ErrNameSuffixExhausted is returned when every `name-1`..`name-MaxNameSuffix`
// is taken. The HTTP layer maps it to NAME_SUFFIX_EXHAUSTED (errors.md).
var ErrNameSuffixExhausted = errors.New("no free name suffix")

// ErrNameTaken reports that another identity already holds the requested name.
//
// ★ A DISTINGUISHABLE ERROR, not a generic failure: the handler must answer 409
// for this and 500 for a genuine persistence fault, and a caller that cannot
// tell them apart would report "server error" for an ordinary name clash.
var ErrNameTaken = errors.New("name already taken")

// ErrEmailTaken reports that another identity already holds the requested
// address.
//
// ★ THE UNIQUENESS IS ENFORCED ON WRITE ONLY, NEVER ON REPLAY. applyEvent
// accepts an EvUserSetEmail whose address is already held — deliberately, and
// the reason is not leniency: the log records what DID happen, and a replay
// that re-adjudicates history cannot load a file written before the rule
// existed. Identity+email shipped live without this check, so any duplicate
// already on disk would make the daemon refuse to start — the invariant would
// be enforced by denying service. New writes are refused; recorded facts are
// reconstructed.
//
// ★ Distinguishable from a persistence fault for the same reason as
// ErrNameTaken: the handler owes 409 for a clash and 500 for a real failure,
// and a caller that cannot tell them apart reports "server error" for an
// ordinary collision.
var ErrEmailTaken = errors.New("email already taken")

// emailTakenByOtherLocked reports whether any identity OTHER than uid already
// holds this address. Callers must hold writeMu (it is a validate-closure
// helper).
//
// ★ SETTING YOUR OWN ADDRESS AGAIN MUST SUCCEED. Without the uid exclusion,
// re-submitting an unchanged profile — which the identity form does on every
// save — would collide with the user's own row and report that their address
// is taken by someone else. The bug would only appear on the second save, so
// every first-save test stays green.
//
// ★ An EMPTY address is never "taken": clearing an email is always allowed,
// and two users with no address are not two users sharing one. Comparison is
// on NormalizeEmail so Case@Ex.com and case@ex.com cannot both be registered —
// they reach the same mailbox, so treating them as distinct would let two
// identities receive each other's magic links.
func (s *Store) emailTakenByOtherLocked(uid, email string) bool {
	n := NormalizeEmail(email)
	if n == "" {
		return false
	}
	for other, u := range s.users {
		if other == uid {
			continue
		}
		if NormalizeEmail(u.Email) == n {
			return true
		}
	}
	return false
}

// CreateUserWithSuffix mints a NEW identity under the first free `name-N`,
// choosing the suffix and creating the user as ONE atomic operation, and
// returns the name it settled on (DD-3 step 8).
//
// ★ THE ALLOCATION AND THE CREATE ARE ONE COMMIT, AND THAT IS THE WHOLE POINT.
// The obvious decomposition — an exported FreeSuffixedName() that a handler
// calls and then passes to CreateUser — is a check-then-act race: two people
// named 駱子逸 hitting the suffix branch concurrently both read "-1 is free"
// and both create 駱子逸-1. The store would accept both (uid is the key, not
// name), the roster would carry a duplicate, and the invariant this function
// exists to hold — "names in the roster are unique" — would fail with no error
// on either request. Deciding inside validate, under writeMu, makes that
// interleaving unrepresentable.
//
// ★ It scans for the LOWEST free N rather than len(sameName)+1. Those agree
// only while nobody has ever been renamed or deleted: with 駱子逸 and 駱子逸-2
// on the roster, counting yields -2 (taken, so a duplicate) while scanning
// yields -1. TV-6C is the vector that separates them.
func (s *Store) CreateUserWithSuffix(uid, tokenHash, name, email string) (string, error) {
	if uid == "" || tokenHash == "" {
		return "", fmt.Errorf("uid and tokenHash are required")
	}
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("name is required")
	}
	email = strings.TrimSpace(email)
	if len(email) > MaxEmailLen {
		return "", fmt.Errorf("email exceeds %d characters", MaxEmailLen)
	}
	if email != "" {
		if err := ValidateEmail(email); err != nil {
			return "", err
		}
	}

	d := UserCreateData{UID: uid, TokenHash: tokenHash,
		CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	err := s.commit(EvUserCreate, &d,
		func() error {
			if _, exists := s.users[uid]; exists {
				return fmt.Errorf("user %q already exists", uid)
			}
			if _, exists := s.byToken[tokenHash]; exists {
				return fmt.Errorf("token hash already registered")
			}
			taken := map[string]bool{}
			for _, u := range s.users {
				taken[u.Name] = true
			}
			// The bare name is tried first: the caller may have raced someone
			// who has since been renamed away, in which case no suffix is
			// needed at all and adding one would rename a user for no reason.
			if !taken[name] {
				d.Name = name
				return nil
			}
			for n := 1; n <= MaxNameSuffix; n++ {
				cand := name + "-" + strconv.Itoa(n)
				// The suffixed form must also fit MaxNameLen: a 16-character
				// name plus "-1" would otherwise be created here and then be
				// rejected by RenameUser later, so the roster would hold a
				// name the rename path considers invalid.
				if len([]rune(cand)) > MaxNameLen {
					return fmt.Errorf("name %q leaves no room for a suffix within %d characters", name, MaxNameLen)
				}
				if !taken[cand] {
					d.Name = cand
					return nil
				}
			}
			return ErrNameSuffixExhausted
		},
		func() {
			s.users[uid] = &User{
				UID: uid, Name: d.Name, Email: email, CreatedAt: d.CreatedAt,
				Tokens: []TokenRef{{Hash: tokenHash, AddedAt: d.CreatedAt, LastSeenAt: d.CreatedAt}},
			}
			s.byToken[tokenHash] = uid
		})
	if err != nil {
		return "", err
	}
	return d.Name, nil
}

// ErrMergeSelf reports an attempt to merge an identity into itself.
//
// ★ REFUSED RATHER THAN TREATED AS A NO-OP. A self-merge means the caller
// resolved two different identities to one uid — a real defect upstream — and
// silently succeeding would delete the user from the roster halfway through
// absorbing them into themselves.
var ErrMergeSelf = errors.New("cannot merge an identity into itself")

// mergeUsersLocked folds `from` wholly into `into`. Callers must hold s.mu for
// writing; it is the shared apply body for the live commit and for replay, so
// the two cannot drift.
//
// ★ EVICTIONS ARE READ FROM THE EVENT, NEVER RECOMPUTED HERE. See
// UserMergeData: LastSeenAt is a throttled approximation, so a replay that
// re-derived the choice could evict a different device than the live merge
// did, and the reconstructed state would silently disagree with the state that
// was actually served.
func mergeUsersLocked(s *Store, from, into *User, d UserMergeData) {
	evicted := make(map[string]bool, len(d.EvictedHashes))
	for _, h := range d.EvictedHashes {
		evicted[h] = true
	}

	// Tokens: `into` keeps its own, gains `from`'s, minus anything the
	// recorded decision evicted. Duplicates are skipped rather than appended —
	// one hash appearing twice would make removeToken drop only one copy and
	// leave byToken pointing at a token the user is no longer considered to
	// hold.
	kept := into.Tokens[:0:0]
	for _, t := range into.Tokens {
		if evicted[t.Hash] {
			delete(s.byToken, t.Hash)
			continue
		}
		kept = append(kept, t)
	}
	for _, t := range from.Tokens {
		if evicted[t.Hash] {
			delete(s.byToken, t.Hash)
			continue
		}
		dup := false
		for i := range kept {
			if kept[i].Hash == t.Hash {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		kept = append(kept, t)
	}
	into.Tokens = kept
	for _, t := range kept {
		s.byToken[t.Hash] = into.UID
	}

	// ★ EVERY PROPOSAL, NOT JUST THE ONE THE MERGE WAS TRIGGERED FROM
	// (使用者裁示 Q3). One-person-one-vote is a GLOBAL invariant: leaving the
	// other proposals untouched would keep both rows there, so the same human
	// counts twice wherever the merge did not reach — a known violation, left
	// behind by the very operation meant to end it.
	for _, p := range s.proposals {
		if p.OwnerUID == from.UID {
			// This is what makes the absorbed identity leave NO orphans. A
			// merge that moved only tokens would strand every proposal `from`
			// owned: its uid would remain the owner while no device could ever
			// authenticate as it again.
			p.OwnerUID = into.UID
		}
		for _, o := range p.Options {
			if o.AuthorUID == from.UID {
				o.AuthorUID = into.UID
			}
		}
		for key, v := range p.Votes {
			if v.UID != from.UID {
				continue
			}
			delete(p.Votes, key)
			target := VoteKey(v.OptionID, into.UID)
			moved := &Vote{OptionID: v.OptionID, UID: into.UID,
				Weight: v.Weight, CreatedAt: v.CreatedAt}
			existing, clash := p.Votes[target]
			if !clash {
				p.Votes[target] = moved
				continue
			}
			// ★ THE NEWER CreatedAt WINS (使用者裁示 Q3). Keeping the older row
			// would silently revert the stance the person expressed most
			// recently — they would watch their own vote change back with no
			// error anywhere. Ties keep the incumbent, so the outcome does not
			// depend on Go's randomised map iteration order.
			if moved.CreatedAt > existing.CreatedAt {
				p.Votes[target] = moved
			}
		}
	}

	// ★ THE ABSORBED IDENTITY LEAVES THE ROSTER. It must: the roster is what
	// the frontend renders, the name is now held by `into`, and two rows with
	// one name is exactly the duplicate the suffix allocator exists to prevent
	// — reintroduced through the merge door.
	delete(s.users, from.UID)
}

// mergeEvictionsLocked decides which token hashes must go when two identities'
// devices are combined. Callers must hold the read lock.
//
// ★ IT RETURNS THE DECISION INSTEAD OF APPLYING IT, so the same list can be
// written into the event and replayed verbatim. Computing it inside apply
// would leave nothing durable for replay to agree with.
func (s *Store) mergeEvictionsLocked(from, into *User) []string {
	combined := make([]TokenRef, 0, len(into.Tokens)+len(from.Tokens))
	seen := map[string]bool{}
	for _, t := range append(append([]TokenRef{}, into.Tokens...), from.Tokens...) {
		if seen[t.Hash] {
			continue
		}
		seen[t.Hash] = true
		combined = append(combined, t)
	}
	var evicted []string
	for len(combined) > MaxTokensPerUser {
		// Least-recently-seen first, ties broken on Hash — the same ordering
		// lruTokenHash uses (DD-2). A different rule here would evict a
		// different device than an ordinary AddToken would, for no reason a
		// reader could discover.
		worst := 0
		for i := 1; i < len(combined); i++ {
			switch {
			case combined[i].LastSeenAt < combined[worst].LastSeenAt:
				worst = i
			case combined[i].LastSeenAt == combined[worst].LastSeenAt &&
				combined[i].Hash < combined[worst].Hash:
				worst = i
			}
		}
		evicted = append(evicted, combined[worst].Hash)
		combined = append(combined[:worst], combined[worst+1:]...)
	}
	return evicted
}

// MergeUsers absorbs fromUID wholly into intoUID: tokens, votes, option
// authorship and proposal ownership, as ONE atomic commit.
//
// ★ THE AUTHORISATION IS NOT CHECKED HERE, AND THAT IS THE CALLER'S CONTRACT.
// This is the mechanical half; the proof that the two records are one person
// comes from the magic link, and the only caller is the verify-confirm path
// (DD-8: the uids come from the server-side pending entry, never from a
// request body). A second caller that skipped the link would make this an
// account-takeover primitive, so there must not be one.
//
// ★ ONE COMMIT, NOT A LOOP OF SMALLER ONES. A merge that emitted an AddToken,
// then vote moves, then an ownership change would be interruptible: a crash
// midway leaves an identity holding a name but owning nothing, and replay
// would faithfully reproduce that torn state.
func (s *Store) MergeUsers(fromUID, intoUID string) error {
	if fromUID == "" || intoUID == "" {
		return fmt.Errorf("both uids are required")
	}
	if fromUID == intoUID {
		return ErrMergeSelf
	}
	d := UserMergeData{FromUID: fromUID, IntoUID: intoUID,
		MergedAt: time.Now().UTC().Format(time.RFC3339)}
	return s.commit(EvUserMerge, &d,
		func() error {
			from, ok := s.users[fromUID]
			if !ok {
				return fmt.Errorf("unknown user %q", fromUID)
			}
			into, ok := s.users[intoUID]
			if !ok {
				return fmt.Errorf("unknown user %q", intoUID)
			}
			// Decided inside validate, under writeMu, together with the write.
			// Two concurrent merges into one identity would otherwise each read
			// the pre-merge token set and each believe nothing needs evicting,
			// leaving the survivor above MaxTokensPerUser.
			d.EvictedHashes = s.mergeEvictionsLocked(from, into)
			return nil
		},
		func() {
			mergeUsersLocked(s, s.users[fromUID], s.users[intoUID], d)
		})
}

// NameTaken reports whether any identity already holds this exact name.
//
// ★ ADVISORY ONLY — it is a UI hint, never an allocation. A caller that reads
// this and then creates has re-introduced exactly the check-then-act race
// CreateUserWithSuffix closes; the decision must stay inside that one commit.
func (s *Store) NameTaken(name string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if u.Name == name {
			return true
		}
	}
	return false
}

// CreateProposal adds a proposal container. pid must come from crypto/rand:
// a guessable pid silently defeats R8's "you need the URL to get in", and that
// failure produces no error message anywhere.
func (s *Store) CreateProposal(pid, ownerUID, title, desc string) error {
	if pid == "" || ownerUID == "" {
		return fmt.Errorf("pid and ownerUid are required")
	}
	if n := len([]rune(title)); n == 0 || n > MaxTitleLen {
		return fmt.Errorf("title must be 1..%d characters", MaxTitleLen)
	}
	if len([]rune(desc)) > MaxDescLen {
		return fmt.Errorf("desc exceeds %d characters", MaxDescLen)
	}
	d := ProposalCreateData{PID: pid, OwnerUID: ownerUID, Title: title, Desc: desc,
		CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	return s.commit(EvProposalCreate, d,
		func() error {
			if _, exists := s.proposals[pid]; exists {
				return fmt.Errorf("proposal %q already exists", pid)
			}
			if _, ok := s.users[ownerUID]; !ok {
				return fmt.Errorf("unknown user %q", ownerUID)
			}
			return nil
		},
		func() {
			s.proposals[pid] = &Proposal{
				PID: pid, OwnerUID: ownerUID, Title: title, Desc: desc,
				Options: map[string]*Option{}, Votes: map[string]*Vote{},
				CreatedAt: d.CreatedAt,
			}
		})
}

func (s *Store) UpdateProposal(pid, title, desc string) error {
	if n := len([]rune(title)); n == 0 || n > MaxTitleLen {
		return fmt.Errorf("title must be 1..%d characters", MaxTitleLen)
	}
	if len([]rune(desc)) > MaxDescLen {
		return fmt.Errorf("desc exceeds %d characters", MaxDescLen)
	}
	return s.commit(EvProposalUpdate, ProposalUpdateData{PID: pid, Title: title, Desc: desc},
		func() error { return s.mustProposal(pid) },
		func() {
			p := s.proposals[pid]
			p.Title, p.Desc = title, desc
		})
}

// DeleteProposal removes the container and, with it, every option and vote
// beneath it — atomically, because they are one nested value under one lock
// (risk I). There is no window in which a half-deleted tree is observable.
func (s *Store) DeleteProposal(pid string) error {
	return s.commit(EvProposalDelete, ProposalDeleteData{PID: pid},
		func() error { return s.mustProposal(pid) },
		func() { delete(s.proposals, pid) })
}

// SetDecision marks (oid != "") or releases (oid == "") the decided option for
// a proposal (DD-5, task 6.7).
//
// ★ The freeze itself is NOT enforced here. Store enforces existence; the
// authorisation layer enforces the freeze (auth.Authorize), because that is the
// single boundary every write already passes through. Putting the check in each
// handler instead would make one forgotten call site a permanently open hole
// that no test failure ever announces.
func (s *Store) SetDecision(pid, oid string) error {
	return s.commit(EvProposalDecide, ProposalDecideData{PID: pid, OID: oid},
		func() error {
			if err := s.mustProposal(pid); err != nil {
				return err
			}
			// An empty oid is the release path and needs no option to exist.
			if oid == "" {
				return nil
			}
			return s.mustOption(pid, oid)
		},
		func() { s.proposals[pid].DecidedOptionID = oid })
}

// RepID rotates a proposal's pid (task 6.9, risk B).
//
// ★ The share URL IS the credential (R8: knowing the pid is what grants entry).
// There is no password to change and no session to revoke, so rotating the id
// is the ONLY revocation this design admits. Anything gentler — a "disable
// sharing" flag, an allow-list — either breaks the one access mechanism that
// exists or requires an identity model the product deliberately does not have.
//
// The caller supplies newPID rather than having it generated here: id minting
// lives in the HTTP layer for every other resource, and splitting that across
// two packages is how two different id formats end up in one system.
func (s *Store) RepID(oldPID, newPID string) error {
	// ★ MEASURED 2026-08-05: the `newPID == oldPID` half of this check is
	// REDUNDANT — oldPID is by definition present in the map, so the collision
	// check below already refuses it (control group CG-Z, removing this half
	// alone, stayed green; only removing BOTH lets a no-op through). Kept
	// because it names the intent at the top of the function and gives the
	// caller a clearer message than "pid already exists". Do NOT read it as a
	// live guard — the guard is the collision check.
	if newPID == "" || newPID == oldPID {
		return fmt.Errorf("new pid must be non-empty and different from the old one")
	}
	return s.commit(EvProposalRepid, ProposalRepidData{OldPID: oldPID, NewPID: newPID},
		func() error {
			if err := s.mustProposal(oldPID); err != nil {
				return err
			}
			if _, taken := s.proposals[newPID]; taken {
				return fmt.Errorf("pid %q already exists", newPID)
			}
			return nil
		},
		func() {
			p := s.proposals[oldPID]
			delete(s.proposals, oldPID)
			p.PID = newPID
			s.proposals[newPID] = p
		})
}

// CreateOption adds a candidate date range. Adding is not voting.
func (s *Store) CreateOption(pid, oid, authorUID, start, end, note string) error {
	if oid == "" {
		return fmt.Errorf("oid is required")
	}
	if err := validateRange(start, end); err != nil {
		return err
	}
	if len([]rune(note)) > MaxNoteLen {
		return fmt.Errorf("note exceeds %d characters", MaxNoteLen)
	}
	d := OptionCreateData{PID: pid, OID: oid, AuthorUID: authorUID,
		StartDate: start, EndDate: end, Note: note}
	return s.commit(EvOptionCreate, d,
		func() error {
			if err := s.mustProposal(pid); err != nil {
				return err
			}
			if _, ok := s.users[authorUID]; !ok {
				return fmt.Errorf("unknown user %q", authorUID)
			}
			p := s.proposals[pid]
			if _, exists := p.Options[oid]; exists {
				return fmt.Errorf("option %q already exists", oid)
			}
			// "No duplicate ranges within a proposal" (design.md A3). Checked
			// here rather than before commit() so a concurrent caller cannot
			// slip an identical range in between the check and the write.
			for _, o := range p.Options {
				if o.StartDate == start && o.EndDate == end {
					return fmt.Errorf("an option with range %s..%s already exists", start, end)
				}
			}
			return nil
		},
		func() {
			s.proposals[pid].Options[oid] = &Option{OID: oid, AuthorUID: authorUID,
				StartDate: start, EndDate: end, Note: note}
		})
}

func (s *Store) UpdateOption(pid, oid, start, end, note string) error {
	if err := validateRange(start, end); err != nil {
		return err
	}
	if len([]rune(note)) > MaxNoteLen {
		return fmt.Errorf("note exceeds %d characters", MaxNoteLen)
	}
	return s.commit(EvOptionUpdate,
		OptionUpdateData{PID: pid, OID: oid, StartDate: start, EndDate: end, Note: note},
		func() error {
			if err := s.mustOption(pid, oid); err != nil {
				return err
			}
			for id, o := range s.proposals[pid].Options {
				if id != oid && o.StartDate == start && o.EndDate == end {
					return fmt.Errorf("an option with range %s..%s already exists", start, end)
				}
			}
			return nil
		},
		func() {
			o := s.proposals[pid].Options[oid]
			o.StartDate, o.EndDate, o.Note = start, end, note
		})
}

// DeleteOption removes an option and every vote cast on it, under one lock.
func (s *Store) DeleteOption(pid, oid string) error {
	return s.commit(EvOptionDelete, OptionDeleteData{PID: pid, OID: oid},
		func() error { return s.mustOption(pid, oid) },
		func() {
			p := s.proposals[pid]
			delete(p.Options, oid)
			for k, v := range p.Votes {
				if v.OptionID == oid {
					delete(p.Votes, k)
				}
			}
		})
}

// SetVote records a stance. Assignment to the composite key is idempotent, so
// posting five times leaves exactly one vote (DD-8, TV-1). uid is always
// derived by the caller from the bearer token, never from the request body.
// SetVote records a stance (DD-6, task 6.8). Weight must be one of the three
// stance constants; an out-of-set value is refused rather than clamped.
//
// ★ Rejecting beats clamping. A clamp turns a forged 9.0 into a silent 1.0 —
// the request succeeds, the ranking is wrong by exactly the amount the attacker
// chose, and nothing anywhere reports it.
func (s *Store) SetVote(pid, oid, uid string, weight float64) error {
	if !ValidWeight(weight) {
		return fmt.Errorf("weight must be one of %v/%v, got %v",
			WeightYes, WeightNo, weight)
	}
	d := VoteSetData{PID: pid, OptionID: oid, UID: uid, Weight: weight,
		CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	return s.commit(EvVoteSet, d,
		func() error {
			if err := s.mustOption(pid, oid); err != nil {
				return err
			}
			if _, ok := s.users[uid]; !ok {
				return fmt.Errorf("unknown user %q", uid)
			}
			return nil
		},
		func() {
			s.proposals[pid].Votes[VoteKey(oid, uid)] = &Vote{
				OptionID: oid, UID: uid, Weight: weight, CreatedAt: d.CreatedAt,
			}
		})
}

func (s *Store) DeleteVote(pid, oid, uid string) error {
	return s.commit(EvVoteDelete, VoteDeleteData{PID: pid, OptionID: oid, UID: uid},
		func() error {
			if err := s.mustProposal(pid); err != nil {
				return err
			}
			if s.proposals[pid].Votes[VoteKey(oid, uid)] == nil {
				return fmt.Errorf("no vote on %q by %q", oid, uid)
			}
			return nil
		},
		func() { delete(s.proposals[pid].Votes, VoteKey(oid, uid)) })
}

// mustProposal / mustOption are validate-time existence checks. Callers hold
// at least s.mu.RLock via commit.
func (s *Store) mustProposal(pid string) error {
	if _, ok := s.proposals[pid]; !ok {
		return fmt.Errorf("unknown proposal %q", pid)
	}
	return nil
}

func (s *Store) mustOption(pid, oid string) error {
	if err := s.mustProposal(pid); err != nil {
		return err
	}
	if _, ok := s.proposals[pid].Options[oid]; !ok {
		return fmt.Errorf("unknown option %q", oid)
	}
	return nil
}
