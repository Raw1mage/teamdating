package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Event type constants. The replayer MUST reject any type not in this set and
// report the line number (tasks 2.3, TV-17): silently skipping an unknown event
// makes "we dropped a record" and "the file never had one" produce the same
// state, so the defect becomes undetectable.
const (
	EvUserCreate     = "user_create"
	EvUserRename     = "user_rename"
	EvProposalCreate = "proposal_create"
	EvProposalUpdate = "proposal_update"
	EvProposalDelete = "proposal_delete"
	EvOptionCreate   = "option_create"
	EvOptionUpdate   = "option_update"
	EvOptionDelete   = "option_delete"
	EvVoteSet        = "vote_set"
	EvVoteDelete     = "vote_delete"
	// EvProposalDecide records BOTH decide and un-decide (DD-5). One event type
	// with an OID that may be empty, rather than two types: the replayer then
	// cannot end up applying "decide" without also understanding its inverse.
	EvProposalDecide = "proposal_decide"
	// EvProposalRepid rotates a proposal's pid (task 6.9, risk B). The share URL
	// IS the credential, so the only revocation available is changing it.
	EvProposalRepid = "proposal_repid"
	// EvUserAddToken adds one bearer token to an existing identity, and records
	// which token (if any) was evicted to make room (multi-token DD-6).
	EvUserAddToken = "user_add_token"
	// EvUserRevokeToken removes one bearer token. The type is defined now so the
	// replayer understands it; no production caller exists yet.
	EvUserRevokeToken = "user_revoke_token"
	// EvUserTouchToken refreshes a token's LastSeenAt. Throttled to at most one
	// per TouchInterval per token (DD-2) — an unthrottled version would turn
	// every authenticated read into an fsync.
	EvUserTouchToken = "user_touch_token"
	// EvUserMerge absorbs one identity wholly into another after a magic-link
	// proved they are the same person (owner-inheritance).
	//
	// ★ ONE EVENT, NOT FOUR. The merge moves tokens, votes, option authorship
	// and proposal ownership. Recording those as four independent events makes
	// a partially-applied merge representable: a crash between them leaves an
	// identity that owns nothing but still holds the name, and replay would
	// reproduce that torn state faithfully. One event is one atomic fact.
	//
	// ★ THERE IS DELIBERATELY NO SEPARATE "transfer owner" EVENT. Ownership
	// moves here because the OWNER did not change — the person was always the
	// same, and what changed is that two records for them became one. A
	// standalone transfer would let ownership move between two identities that
	// were never shown to be one person, which is precisely the authorisation
	// the magic link exists to establish.
	EvUserMerge = "user_merge"
	// EvUserSetEmail records an identity's email address (DD-5, identity plan).
	//
	// ★ ONCE DEFINED, THIS CASE IS PERMANENT. The replayer refuses any type it
	// does not know (ErrUnknownEventType), so the moment one binary writes a
	// user_set_email line, every older binary REFUSES TO START on that log —
	// there is no rollback. That is the deliberate trade this project already
	// made for the other Ev* types: a hard startup failure beats a silent
	// partial replay in which some users mysteriously lack an email.
	//
	// ★ Also why the magic-link verification token is NOT an event (DD-4). It is
	// a 15-minute secret; writing it to an append-only log would leave a
	// permanent record of a credential AND grow replay cost with every login
	// attempt. Only the OUTCOME — this event, plus EvUserAddToken — lands.
	EvUserSetEmail = "user_set_email"
)

// Event is one line of data/events.jsonl.
//
// File order IS the authoritative order (append-only); the timestamp is for
// humans and audit only and is never used to sort during replay.
type Event struct {
	T    string          `json:"t"`
	Type string          `json:"type"`
	D    json.RawMessage `json:"d"`
}

// Payload shapes for each event type.
type (
	UserCreateData     struct{ UID, TokenHash, Name, CreatedAt string }
	UserRenameData     struct{ UID, Name string }
	ProposalCreateData struct {
		PID, OwnerUID, Title, Desc, CreatedAt string
	}
	ProposalUpdateData struct{ PID, Title, Desc string }
	ProposalDeleteData struct{ PID string }
	OptionCreateData   struct {
		PID, OID, AuthorUID, StartDate, EndDate, Note string
	}
	OptionUpdateData struct {
		PID, OID, StartDate, EndDate, Note string
	}
	OptionDeleteData struct{ PID, OID string }
	VoteSetData      struct {
		PID, OptionID, UID, CreatedAt string
		Weight                        float64
	}
	VoteDeleteData struct{ PID, OptionID, UID string }
	// ProposalDecideData carries the decided option id. An EMPTY OID means
	// "un-decide" — the same shape both ways, so a replay cannot silently drop
	// the release half and leave a proposal frozen forever.
	ProposalDecideData struct{ PID, OID string }
	// ProposalRepidData carries both ends. Recording only the new pid would make
	// replay unable to find what to move, and recording only the old one would
	// make the new pid unreproducible — a random value cannot be re-derived.
	ProposalRepidData struct{ OldPID, NewPID string }
	// UserAddTokenData records the DECISION, not the inputs (DD-6).
	//
	// ★ EvictedHash is why this struct has four fields instead of three. Replay
	// must not RECOMPUTE which token to evict: the computation reads LastSeenAt,
	// which is a throttled approximation, so a recomputation can legitimately
	// pick a different token than the live decision did — and the replayed state
	// then diverges from the state that was actually served, with nothing
	// anywhere reporting it. An empty EvictedHash means nothing was evicted.
	UserAddTokenData struct {
		UID, Hash, AddedAt string
		EvictedHash        string
	}
	UserRevokeTokenData struct{ UID, Hash string }
	UserTouchTokenData  struct{ UID, Hash, SeenAt string }
	// UserSetEmailData carries the address AS TYPED, not folded (DD-3).
	//
	// ★ Storing the normalised form would make the log — the only durable
	// record — disagree with what the user entered, and the address is shown
	// back to them in the gear modal. Comparison folds at the decision point
	// (store.SameEmail); persistence keeps the original.
	UserSetEmailData struct{ UID, Email string }
	// UserMergeData absorbs FromUID wholly into IntoUID (owner-inheritance).
	//
	// ★ THE EVICTED HASHES ARE RECORDED, NOT RECOMPUTED, for exactly the
	// reason UserAddTokenData records EvictedHash: eviction reads LastSeenAt,
	// which is a throttled approximation (TouchInterval), so a replay that
	// re-derived the choice could legitimately evict a different device than
	// the live merge did — and the replayed state would then diverge from the
	// state actually served, silently.
	//
	// ★ THE VOTE DECISIONS ARE NOT RECORDED, AND THAT ASYMMETRY IS DELIBERATE.
	// Dedup compares Vote.CreatedAt, which is an exact stored value rather than
	// an approximation, so replaying the same two rows always yields the same
	// winner. Recording it anyway would add a second copy of a derivable fact
	// that could disagree with the rows it describes.
	UserMergeData struct {
		FromUID, IntoUID, MergedAt string
		EvictedHashes             []string
	}
)

// Store is the authoritative in-memory state plus its write-ahead log.
//
// ★ Write-ahead contract (DD-14, risk N): every mutation appends its event to
// the log and fsyncs it BEFORE touching memory. The reverse order also "works"
// and is completely indistinguishable under a clean shutdown — it only shows up
// as "the user saw success but the record vanished after restart" on kill -9.
// That is exactly why the order is pinned in the design instead of being left
// to the implementer (TV-12).
type Store struct {
	// writeMu serialises whole mutations (validate + append + apply). It is
	// NOT redundant with mu: mu makes each individual map access safe, but a
	// check-then-act sequence needs the two steps to be atomic with respect to
	// other writers, and holding mu across the fsync would block every reader
	// for the duration of a disk sync.
	writeMu sync.Mutex

	// mu guards the maps below. Readers take RLock; apply takes Lock.
	mu        sync.RWMutex
	users     map[string]*User // uid -> user
	byToken   map[string]string
	proposals map[string]*Proposal

	// log is only ever written under writeMu.
	log *os.File
	// dir is the data directory. Compact needs it to place snapshot.json next
	// to the log: a rename is only atomic WITHIN a filesystem, so the temp file
	// has to be a sibling, not somewhere derived independently.
	dir string
	// unsafeOrder, when true, mutates memory BEFORE appending. It exists ONLY
	// so the control group in TV-12 can be executed against the same code path;
	// production never sets it. Without a broken-order mode to compare against,
	// a passing durability test proves nothing.
	unsafeOrder bool
	// unsafeCompactOrder flips Compact's two renames into the broken order
	// (log truncated before the snapshot lands). Control group only; see 6.3.
	unsafeCompactOrder bool
	// evictions counts LRU evictions since this process started. Guarded by mu
	// (written inside apply, read by the accessor).
	//
	// ★ Process-local and reset by a restart, on purpose. An eviction is a
	// user-visible harm — a device silently logged out — so the question worth
	// answering is "how often is this happening lately". The lifetime total is
	// already in the log (every user_add_token records the hash it evicted), so
	// persisting a second copy would only add a field that can disagree with it.
	evictions int

	// sinceCompact counts events appended since the last compaction. Written
	// under writeMu, like the log itself.
	//
	// ★ A compaction driven purely by a timer rewrites an identical snapshot
	// forever on an idle deployment — burning fsyncs and, worse, taking the
	// crash window repeatedly for no benefit. Counting is what makes the
	// trigger proportional to the thing it is supposed to bound.
	sinceCompact int
	// compactHook runs BETWEEN Compact's two renames, the only window where the
	// ordering is observable. Same rationale as crashHook below.
	compactHook func()

	// crashHook runs BETWEEN the two steps of a commit, and is the only way to
	// give the durability test any discriminating power at all.
	//
	// Killing the process after a mutator RETURNS cannot distinguish the two
	// orders: by then both steps have run either way. The difference lives
	// strictly inside the window between them, so the crash has to be injected
	// there. Measured: without this hook both orders survive a kill -9
	// identically — which is precisely the "then the test is measuring nothing"
	// outcome TV-12 predicts.
	crashHook func()

	// seq counts committed mutations, starting at 1. It is assigned under
	// writeMu, so the numbering IS the commit order (DD-21).
	//
	// ★ seq exists for the SSE snapshot handshake, not for bookkeeping. A
	// subscriber that takes a snapshot and THEN subscribes permanently loses
	// every event landing in between — and loses them silently, leaving the
	// client on a stale view with no error anywhere. Numbering lets the client
	// subscribe first, snapshot at seq=N, and replay only seq>N.
	seq uint64

	// observer, if set, is called under writeMu immediately after apply, with
	// the seq just assigned (DD-21). nil in production until the hub is wired
	// in pkg2b, so the whole notification path is currently inert.
	//
	// ★ Called INSIDE writeMu on purpose: notifying after the lock is released
	// lets two mutations enter the store as A,B and be announced as B,A. A
	// diff-rendering client (DD-12) applying them out of order diverges from
	// server state with no error message, visible only to whoever happens to be
	// looking, and repaired by a refresh. Inside the lock, that divergence is
	// structurally impossible.
	//
	// ★ CONTRACT ON THE OBSERVER: it must be non-blocking and do no I/O — a
	// bounded-channel send at most. Blocking here holds writeMu and stalls
	// every writer in the process; one slow client freezing all writes is far
	// worse than that client dropping frames.
	observer func(seq uint64, ev Event)
}

// ErrUnknownEventType is returned by Replay for an unrecognised event type.
var ErrUnknownEventType = errors.New("unknown event type")

// New opens (creating if needed) dir/events.jsonl and replays it into a Store.
//
// Failure to create or write the data directory is fatal to the caller: this
// returns an error rather than silently degrading to memory-only operation,
// which would let the user write data that is thrown away at exit.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, logName)

	s := &Store{
		users:     map[string]*User{},
		byToken:   map[string]string{},
		proposals: map[string]*Proposal{},
		dir:       dir,
	}

	// Snapshot first, then the log tail on top of it (6.3). The snapshot says
	// how many leading log lines it already contains; everything after that is
	// replayed. A snapshot written by Compact claims 0, so a crash between
	// Compact's two renames replays the whole surviving log over a complete
	// state — idempotent, and the reason the renames are ordered as they are.
	applied, err := s.loadSnapshot(dir)
	if err != nil {
		return nil, err
	}

	// Replay first, then open for append: replaying through the same handle we
	// later append to would leave the offset in an ambiguous place.
	if err := s.replayFileFrom(path, applied); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open event log %s: %w", path, err)
	}
	// Probe writability now rather than at the first user request, so an
	// unwritable data dir fails at startup with a clear cause (tasks 1.4).
	if _, err := f.Write(nil); err != nil {
		f.Close()
		return nil, fmt.Errorf("event log %s is not writable: %w", path, err)
	}
	s.log = f
	return s, nil
}

// SetUnsafeOrderForControlGroup flips the write order to the KNOWN-BROKEN one.
// Only tests call this, and only to prove the durability test can fail.
func (s *Store) SetUnsafeOrderForControlGroup(b bool) { s.unsafeOrder = b }

// SetCrashHookForControlGroup installs a function to run in the window between
// the log append and the memory mutation. Test harnesses use it to SIGKILL the
// process exactly there; production never sets it.
func (s *Store) SetCrashHookForControlGroup(f func()) { s.crashHook = f }

// SetObserver installs the commit observer (DD-21). Pass nil to detach.
//
// ★ CONTRACT — the observer runs while writeMu is HELD:
//
//   - It MUST NOT block. A bounded-channel send is the intended shape; a send
//     to an unbuffered or full channel stalls every writer in the process, so
//     one slow subscriber would freeze all writes. Dropping frames for a lagging
//     subscriber is the correct trade — let it reconnect and take a fresh
//     snapshot.
//   - It MUST NOT do I/O, and MUST NOT call back into this Store: writeMu is
//     not reentrant, so a mutation from inside the observer deadlocks.
//
// Holding the lock is the point: notifying after release lets two commits land
// as A,B and be announced as B,A, which desynchronises a diff-rendering client
// with no error anywhere (DD-21).
func (s *Store) SetObserver(f func(seq uint64, ev Event)) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.observer = f
}

// Close flushes and closes the event log.
func (s *Store) Close() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.log == nil {
		return nil
	}
	err := s.log.Close()
	s.log = nil
	return err
}

func (s *Store) replayFile(path string) error {
	return s.replayFileFrom(path, 0)
}

func (s *Store) replayFileFrom(path string, skip int) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // first run: an absent log is empty state, not an error
	}
	if err != nil {
		return fmt.Errorf("open event log %s: %w", path, err)
	}
	defer f.Close()
	return s.ReplayFrom(f, path, skip)
}

// Replay rebuilds state from r. line numbers are 1-based and are included in
// every error so an operator can find the offending record (TV-17).
func (s *Store) Replay(r io.Reader, path string) error {
	return s.ReplayFrom(r, path, 0)
}

// ReplayFrom is Replay skipping the first skip lines, which a snapshot already
// contains. Line numbers reported in errors stay absolute (counted from the
// start of the file) so they still match what an operator sees in an editor.
func (s *Store) ReplayFrom(r io.Reader, path string, skip int) error {
	sc := bufio.NewScanner(r)
	// Event payloads are small, but a single pathological line should produce a
	// clear error rather than a truncated silent read.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	line := 0
	for sc.Scan() {
		line++
		if line <= skip {
			continue
		}
		raw := sc.Bytes()
		if len(trimSpace(raw)) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return fmt.Errorf("%s:%d: malformed JSON: %w", path, line, err)
		}
		if err := s.applyEvent(ev); err != nil {
			return fmt.Errorf("%s:%d: %w (type=%q)", path, line, err, ev.Type)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s:%d: read: %w", path, line, err)
	}
	return nil
}

func trimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\r' || b[j-1] == '\n') {
		j--
	}
	return b[i:j]
}

// applyEvent mutates state. Callers hold no lock during replay (single
// goroutine); during live operation apply() is called with s.mu held.
func (s *Store) applyEvent(ev Event) error {
	switch ev.Type {
	case EvUserCreate:
		var d UserCreateData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		// ★ THE CONVERSION LIVES HERE, NOT IN THE PAYLOAD STRUCT (DD-4).
		//
		// UserCreateData still carries a SINGLE TokenHash because every
		// user_create line ever written has that shape. Had the struct been
		// changed to a Tokens slice, those old lines would still decode
		// WITHOUT ERROR — json.Unmarshal fills a missing field with its zero
		// value — yielding Tokens:nil. Replay would report success, the
		// service would start clean, and every existing user would be logged
		// out with not one line of error anywhere. Converting at the point of
		// application keeps the on-disk shape frozen and the defect
		// unreachable.
		s.users[d.UID] = &User{
			UID: d.UID, Name: d.Name, CreatedAt: d.CreatedAt,
			Tokens: []TokenRef{{Hash: d.TokenHash, AddedAt: d.CreatedAt, LastSeenAt: d.CreatedAt}},
		}
		s.byToken[d.TokenHash] = d.UID

	case EvUserAddToken:
		var d UserAddTokenData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		u, ok := s.users[d.UID]
		if !ok {
			return fmt.Errorf("add token to unknown user %q", d.UID)
		}
		// ★ Evict by the hash the EVENT names, never by recomputing the LRU
		// (DD-6). Recomputation reads LastSeenAt, which is throttled and thus
		// approximate, so it can pick a different token than the live decision
		// did — leaving the replayed state quietly different from the state
		// that was actually served.
		if d.EvictedHash != "" {
			u.removeToken(d.EvictedHash)
			delete(s.byToken, d.EvictedHash)
		}
		if _, dup := u.FindToken(d.Hash); !dup {
			u.Tokens = append(u.Tokens, TokenRef{
				Hash: d.Hash, AddedAt: d.AddedAt, LastSeenAt: d.AddedAt,
			})
		}
		s.byToken[d.Hash] = d.UID

	case EvUserRevokeToken:
		var d UserRevokeTokenData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		u, ok := s.users[d.UID]
		if !ok {
			return fmt.Errorf("revoke token of unknown user %q", d.UID)
		}
		u.removeToken(d.Hash)
		delete(s.byToken, d.Hash)

	case EvUserTouchToken:
		var d UserTouchTokenData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		u, ok := s.users[d.UID]
		if !ok {
			return fmt.Errorf("touch token of unknown user %q", d.UID)
		}
		// A touch for a token that is no longer present is not an error: the
		// token may have been evicted by a LATER event, and replay applies
		// events in file order. Failing here would make a perfectly ordinary
		// history unloadable.
		if ref, ok := u.FindToken(d.Hash); ok {
			ref.LastSeenAt = d.SeenAt
		}

	case EvUserSetEmail:
		var d UserSetEmailData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		u, ok := s.users[d.UID]
		if !ok {
			return fmt.Errorf("set email of unknown user %q", d.UID)
		}
		// An EMPTY Email is the clear path, and it is applied like any other
		// value rather than skipped. Treating "" as "no change" would make a
		// deliberate clear silently un-replayable: the address would come back
		// from a restart, and the proposal gate would keep passing someone who
		// had removed it.
		u.Email = d.Email

	case EvUserRename:
		var d UserRenameData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		u, ok := s.users[d.UID]
		if !ok {
			return fmt.Errorf("rename of unknown user %q", d.UID)
		}
		u.Name = d.Name

	case EvProposalCreate:
		var d ProposalCreateData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		s.proposals[d.PID] = &Proposal{
			PID: d.PID, OwnerUID: d.OwnerUID, Title: d.Title, Desc: d.Desc,
			Options:   map[string]*Option{},
			Votes:     map[string]*Vote{},
			CreatedAt: d.CreatedAt,
		}

	case EvProposalUpdate:
		var d ProposalUpdateData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		p, ok := s.proposals[d.PID]
		if !ok {
			return fmt.Errorf("update of unknown proposal %q", d.PID)
		}
		p.Title, p.Desc = d.Title, d.Desc

	case EvProposalDecide:
		var d ProposalDecideData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		p, ok := s.proposals[d.PID]
		if !ok {
			return fmt.Errorf("decide on unknown proposal %q", d.PID)
		}
		p.DecidedOptionID = d.OID

	case EvProposalRepid:
		var d ProposalRepidData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		p, ok := s.proposals[d.OldPID]
		if !ok {
			return fmt.Errorf("repid of unknown proposal %q", d.OldPID)
		}
		if _, taken := s.proposals[d.NewPID]; taken {
			return fmt.Errorf("repid target %q already exists", d.NewPID)
		}
		// Delete THEN insert, and update the embedded PID. Leaving the old key
		// in place would keep the leaked URL working — the revocation would
		// report success and revoke nothing.
		delete(s.proposals, d.OldPID)
		p.PID = d.NewPID
		s.proposals[d.NewPID] = p

	case EvProposalDelete:
		var d ProposalDeleteData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		// Cascade is implicit: options and votes are nested under the proposal
		// (DD-1), so deleting the container cannot leave orphans (risk I).
		delete(s.proposals, d.PID)

	case EvOptionCreate:
		var d OptionCreateData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		p, ok := s.proposals[d.PID]
		if !ok {
			return fmt.Errorf("option under unknown proposal %q", d.PID)
		}
		p.Options[d.OID] = &Option{OID: d.OID, AuthorUID: d.AuthorUID,
			StartDate: d.StartDate, EndDate: d.EndDate, Note: d.Note}

	case EvOptionUpdate:
		var d OptionUpdateData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		p, ok := s.proposals[d.PID]
		if !ok {
			return fmt.Errorf("option under unknown proposal %q", d.PID)
		}
		o, ok := p.Options[d.OID]
		if !ok {
			return fmt.Errorf("update of unknown option %q", d.OID)
		}
		o.StartDate, o.EndDate, o.Note = d.StartDate, d.EndDate, d.Note

	case EvOptionDelete:
		var d OptionDeleteData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		p, ok := s.proposals[d.PID]
		if !ok {
			return fmt.Errorf("option under unknown proposal %q", d.PID)
		}
		delete(p.Options, d.OID)
		for k, v := range p.Votes {
			if v.OptionID == d.OID {
				delete(p.Votes, k)
			}
		}

	case EvVoteSet:
		var d VoteSetData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		p, ok := s.proposals[d.PID]
		if !ok {
			return fmt.Errorf("vote under unknown proposal %q", d.PID)
		}
		// Assignment to the deterministic key is idempotent: voting N times
		// still yields exactly one cell (DD-8, TV-1).
		p.Votes[VoteKey(d.OptionID, d.UID)] = &Vote{
			OptionID: d.OptionID, UID: d.UID, Weight: d.Weight, CreatedAt: d.CreatedAt,
		}

	case EvVoteDelete:
		var d VoteDeleteData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		p, ok := s.proposals[d.PID]
		if !ok {
			return fmt.Errorf("vote under unknown proposal %q", d.PID)
		}
		delete(p.Votes, VoteKey(d.OptionID, d.UID))

	case EvUserMerge:
		var d UserMergeData
		if err := json.Unmarshal(ev.D, &d); err != nil {
			return err
		}
		from, ok := s.users[d.FromUID]
		if !ok {
			return fmt.Errorf("merge from unknown user %q", d.FromUID)
		}
		into, ok := s.users[d.IntoUID]
		if !ok {
			return fmt.Errorf("merge into unknown user %q", d.IntoUID)
		}
		mergeUsersLocked(s, from, into, d)

	default:
		return fmt.Errorf("%w %q", ErrUnknownEventType, ev.Type)
	}
	return nil
}
