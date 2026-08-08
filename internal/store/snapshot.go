package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// snapshotName / logName are the two files that make up the on-disk state.
const (
	snapshotName = "snapshot.json"
	logName      = "events.jsonl"
	tmpSuffix    = ".tmp"
)

// snapshotFile is the serialised form of the whole store (6.3, risk N).
//
// ★ byToken is NOT serialised: it is derivable from Users, and a persisted
// index can disagree with the data it indexes. A field that can be wrong in a
// way nothing checks is worse than one recomputed on every load.
type snapshotFile struct {
	Version int `json:"version"`
	// AppliedLines is how many leading lines of events.jsonl this snapshot
	// already contains. Replay skips exactly that many.
	//
	// ★ Compact always writes 0, because it truncates the log in the same
	// operation. The field exists anyway so that a crash BETWEEN the two
	// renames is safe: the snapshot lands first and claims to cover nothing,
	// so the still-intact old log is replayed in full on top of it. Replaying
	// a complete tail over a state that already contains it is idempotent —
	// which is the entire reason the renames are ordered this way.
	AppliedLines int                  `json:"appliedLines"`
	Users        map[string]*User     `json:"users"`
	Proposals    map[string]*Proposal `json:"proposals"`
}

// snapshotFileV1 is the shape written before multi-token (single TokenHash).
//
// ★ A SEPARATE TYPE, decoded by a SEPARATE FUNCTION (DD-3). The tempting
// alternative — keep one struct carrying both `tokenHash,omitempty` and
// `tokens,omitempty` and let JSON decide — produces IDENTICAL memory for two
// opposite situations: "a v1 file whose tokens we forgot to convert" and "a v2
// file that genuinely has no tokens". The first is every user silently logged
// out; the second is normal. Two decode paths make forgetting v1 a loud
// failure (unsupported version) instead of a quiet empty list.
type snapshotFileV1 struct {
	Version      int                  `json:"version"`
	AppliedLines int                  `json:"appliedLines"`
	Users        map[string]*userV1   `json:"users"`
	Proposals    map[string]*Proposal `json:"proposals"`
}

// userV1 is the pre-multi-token roster entry. Read-only: nothing ever writes
// this shape again.
type userV1 struct {
	UID       string `json:"uid"`
	TokenHash string `json:"tokenHash"`
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
}

// snapshotVersion is 2 since User gained a token slice (DD-3). Version 1 files
// are still readable via the upgrade path in loadSnapshot.
const snapshotVersion = 2

// Compact writes the current state to snapshot.json and truncates the event
// log, so replay cost stops growing with the lifetime of the deployment.
//
// ★ ORDERING IS THE WHOLE CONTRACT. Two renames, and swapping them loses data:
//
//	crash before rename #1  -> old snapshot + old log        (unchanged)
//	crash between #1 and #2 -> new snapshot (claims 0 lines) + old log
//	                           => full log replayed over a complete state,
//	                              which is idempotent because the log is a
//	                              complete history
//	crash after rename #2   -> new snapshot + empty log      (the goal)
//
// Reversed, the window between them holds a TRUNCATED log next to a snapshot
// that predates it — every mutation since the last compaction is gone, and
// nothing anywhere reports it.
//
// A partially-written snapshot is impossible for the same structural reason
// the durability contract does not rely on care: the bytes go to a temp file
// and become visible only through rename(2), which is atomic within a
// filesystem. Writing in place would make "the process died mid-write" and
// "this is the snapshot" produce the same filename.
func (s *Store) Compact() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.compactLocked()
}

// DefaultCompactThreshold is how many appended events trigger a compaction.
//
// ★ Counted, not timed. A timer rewrites an identical snapshot forever on an
// idle deployment: it burns fsyncs and takes the crash window over and over for
// no benefit, while doing nothing at all about the case that actually matters
// (a burst that grows the log fast). The count is proportional to the thing
// being bounded; wall-clock time is not.
const DefaultCompactThreshold = 500

// CompactIfDue compacts when at least threshold events have been appended since
// the last compaction, and reports whether it did.
//
// ★ This is the ONLY production caller of Compact, and it exists because a
// compaction routine nothing invokes is indistinguishable from one that works:
// the tests pass, the code reviews clean, and the log grows without bound
// forever. Wiring is the deliverable, not the function.
func (s *Store) CompactIfDue(threshold int) (bool, error) {
	if threshold <= 0 {
		threshold = DefaultCompactThreshold
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.sinceCompact < threshold {
		return false, nil
	}
	if err := s.compactLocked(); err != nil {
		return false, err
	}
	return true, nil
}

// PendingSinceCompact reports how many events have been appended since the
// last compaction. Exposed so the health endpoint can show it: a compaction
// that silently stopped running looks exactly like one that is not needed yet.
func (s *Store) PendingSinceCompact() int {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.sinceCompact
}

func (s *Store) compactLocked() error {
	if s.dir == "" {
		return fmt.Errorf("store has no data directory")
	}

	s.mu.RLock()
	snap := snapshotFile{
		Version:      snapshotVersion,
		AppliedLines: 0,
		Users:        make(map[string]*User, len(s.users)),
		Proposals:    make(map[string]*Proposal, len(s.proposals)),
	}
	for k, v := range s.users {
		u := *v
		snap.Users[k] = &u
	}
	for k, v := range s.proposals {
		snap.Proposals[k] = deepCopy(v)
	}
	s.mu.RUnlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	raw = append(raw, '\n')

	snapPath := filepath.Join(s.dir, snapshotName)
	logPath := filepath.Join(s.dir, logName)
	snapTmp := snapPath + tmpSuffix
	logTmp := logPath + tmpSuffix

	// Both temp files are fully durable BEFORE either rename. Renaming a file
	// whose contents are still in the page cache makes the name visible while
	// the bytes are not, which is the same failure with a longer fuse.
	if err := writeFileSync(snapTmp, raw); err != nil {
		return fmt.Errorf("write snapshot temp: %w", err)
	}
	if err := writeFileSync(logTmp, nil); err != nil {
		return fmt.Errorf("write log temp: %w", err)
	}

	if s.unsafeCompactOrder {
		// CONTROL GROUP ONLY. Truncate the log first: a crash in the window
		// leaves an empty log next to a stale snapshot.
		if err := os.Rename(logTmp, logPath); err != nil {
			return fmt.Errorf("rename log: %w", err)
		}
		s.fireCompactHook()
		if err := os.Rename(snapTmp, snapPath); err != nil {
			return fmt.Errorf("rename snapshot: %w", err)
		}
	} else {
		if err := os.Rename(snapTmp, snapPath); err != nil {
			return fmt.Errorf("rename snapshot: %w", err)
		}
		s.fireCompactHook()
		if err := os.Rename(logTmp, logPath); err != nil {
			return fmt.Errorf("rename log: %w", err)
		}
	}

	// A rename is durable only after its parent directory is synced; without
	// this a power cut can leave the old directory entry.
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("sync data dir: %w", err)
	}

	// The old handle now points at an unlinked inode: appends to it would go
	// nowhere visible and would be lost on the next restart — a write reported
	// as successful that no longer exists anywhere.
	if s.log != nil {
		if err := s.log.Close(); err != nil {
			return fmt.Errorf("close old log: %w", err)
		}
		s.log = nil
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("reopen event log: %w", err)
	}
	s.log = f
	s.sinceCompact = 0
	return nil
}

// writeFileSync writes data to path and fsyncs it before returning.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := f.Write(data); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// loadSnapshot reads dir/snapshot.json into the store and reports how many
// leading log lines it already covers. A missing snapshot is not an error.
//
// ★ A CORRUPT snapshot IS an error, deliberately. Falling back to "replay the
// log from scratch" would look like a graceful recovery and would silently
// serve a state missing everything the log no longer holds (天條 11).
func (s *Store) loadSnapshot(dir string) (int, error) {
	path := filepath.Join(dir, snapshotName)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read snapshot %s: %w", path, err)
	}

	// Read the version alone first, so the choice of decoder is made from the
	// declared version rather than from which fields happened to be present.
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return 0, fmt.Errorf("%s: malformed snapshot: %w", path, err)
	}
	if probe.Version == 1 {
		return s.loadSnapshotV1(path, raw)
	}

	var snap snapshotFile
	if err := json.Unmarshal(raw, &snap); err != nil {
		return 0, fmt.Errorf("%s: malformed snapshot: %w", path, err)
	}
	if snap.Version != snapshotVersion {
		return 0, fmt.Errorf("%s: unsupported snapshot version %d (want %d)",
			path, snap.Version, snapshotVersion)
	}
	if snap.AppliedLines < 0 {
		return 0, fmt.Errorf("%s: negative appliedLines %d", path, snap.AppliedLines)
	}

	for uid, u := range snap.Users {
		if u == nil {
			return 0, fmt.Errorf("%s: null user %q", path, uid)
		}
		cp := *u
		cp.Tokens = append([]TokenRef(nil), u.Tokens...)
		s.users[uid] = &cp
		// Walk the whole slice. The single-assignment version of this line was
		// the index-rebuild point under the old model; with a slice, indexing
		// only the first entry would leave every additional device unable to
		// authenticate after a restart, with no error at load time.
		for _, t := range cp.Tokens {
			s.byToken[t.Hash] = uid
		}
	}
	for pid, p := range snap.Proposals {
		if p == nil {
			return 0, fmt.Errorf("%s: null proposal %q", path, pid)
		}
		// A snapshot written before any option existed serialises an empty
		// map as null; leaving it nil would panic on the first write.
		if p.Options == nil {
			p.Options = map[string]*Option{}
		}
		if p.Votes == nil {
			p.Votes = map[string]*Vote{}
		}
		s.proposals[pid] = deepCopy(p)
	}
	return snap.AppliedLines, nil
}

// loadSnapshotV1 reads a pre-multi-token snapshot and upgrades it in memory.
//
// ★ A SEPARATE FUNCTION, not a branch inside the v2 decoder (DD-3). Each v1
// user carries exactly one token, which becomes a one-element slice whose
// AddedAt and LastSeenAt both take createdAt — the only timestamp the old
// format has. Inventing "now" instead would make every pre-existing token look
// freshly used, so the first eviction after an upgrade would pick by hash
// tie-break rather than by actual staleness.
//
// The file is NOT rewritten here. The next compaction writes v2 naturally; a
// rewrite at load time would mean an upgrade destroys the only copy of the old
// state before anyone has confirmed the new binary works.
func (s *Store) loadSnapshotV1(path string, raw []byte) (int, error) {
	var snap snapshotFileV1
	if err := json.Unmarshal(raw, &snap); err != nil {
		return 0, fmt.Errorf("%s: malformed snapshot: %w", path, err)
	}
	if snap.AppliedLines < 0 {
		return 0, fmt.Errorf("%s: negative appliedLines %d", path, snap.AppliedLines)
	}

	for uid, u := range snap.Users {
		if u == nil {
			return 0, fmt.Errorf("%s: null user %q", path, uid)
		}
		cp := &User{UID: u.UID, Name: u.Name, CreatedAt: u.CreatedAt}
		if cp.UID == "" {
			cp.UID = uid
		}
		// An empty TokenHash is a roster entry with no key at all. Materialising
		// a TokenRef{Hash:""} for it would put "" into byToken, where a request
		// carrying no cookie could match it.
		if u.TokenHash != "" {
			cp.Tokens = []TokenRef{{
				Hash: u.TokenHash, AddedAt: u.CreatedAt, LastSeenAt: u.CreatedAt,
			}}
			s.byToken[u.TokenHash] = uid
		}
		s.users[uid] = cp
	}
	for pid, p := range snap.Proposals {
		if p == nil {
			return 0, fmt.Errorf("%s: null proposal %q", path, pid)
		}
		if p.Options == nil {
			p.Options = map[string]*Option{}
		}
		if p.Votes == nil {
			p.Votes = map[string]*Vote{}
		}
		s.proposals[pid] = deepCopy(p)
	}
	return snap.AppliedLines, nil
}

// loadSnapshotV2Only decodes WITHOUT the v1 upgrade path. It exists so TV-8
// can execute the broken implementation against real bytes.
//
// ★ A committed control group, not a mutation to be undone. The alternative —
// temporarily deleting the v1 branch above, observing red, and putting it back
// — leaves correctness depending on someone remembering to restore, and this
// repo has already paid twice for that window (a stale-bytes run, and a
// `git checkout --` that wiped an unstaged fix). Nothing here has to be put
// back, so an interrupted session leaves a clean tree.
//
// Production never calls it: loadSnapshot dispatches on the declared version.
func (s *Store) loadSnapshotV2Only(raw []byte, path string) (int, error) {
	var snap snapshotFile
	if err := json.Unmarshal(raw, &snap); err != nil {
		return 0, fmt.Errorf("%s: malformed snapshot: %w", path, err)
	}
	if snap.Version != snapshotVersion {
		return 0, fmt.Errorf("%s: unsupported snapshot version %d (want %d)",
			path, snap.Version, snapshotVersion)
	}
	for uid, u := range snap.Users {
		if u == nil {
			return 0, fmt.Errorf("%s: null user %q", path, uid)
		}
		cp := *u
		s.users[uid] = &cp
		for _, t := range cp.Tokens {
			s.byToken[t.Hash] = uid
		}
	}
	return snap.AppliedLines, nil
}

// SetUnsafeCompactOrderForControlGroup flips the two renames into the
// KNOWN-BROKEN order. Only tests call this.
func (s *Store) SetUnsafeCompactOrderForControlGroup(b bool) { s.unsafeCompactOrder = b }

// SetCompactHookForControlGroup installs a function to run BETWEEN the two
// renames. Test harnesses SIGKILL the process there; production never sets it.
func (s *Store) SetCompactHookForControlGroup(f func()) { s.compactHook = f }

func (s *Store) fireCompactHook() {
	if s.compactHook != nil {
		s.compactHook()
	}
}
