package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Persistence-compatibility vectors for the multi-token change.
//
// ★ These run BEFORE any new-behaviour test in the task order, and the reason
// is the failure shape they guard: a compatibility defect here produces a
// service that starts cleanly, logs nothing, and has quietly logged every
// existing user out. It turns no new test red. Proving old data still loads has
// to happen first, or a wall of green from the new features hides it.

// writeFile is a fatal-on-error scratch writer.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

const v1SnapshotJSON = `{"version":1,"appliedLines":0,` +
	`"users":{"U1":{"uid":"U1","tokenHash":"hA","name":"小明",` +
	`"createdAt":"2026-01-01T00:00:00Z"}},"proposals":{}}`

// TestV1SnapshotLoads is TV-7: a snapshot written by the single-token build
// must load with its token still able to authenticate.
func TestV1SnapshotLoads(t *testing.T) {
	dir := tempDir(t)
	writeFile(t, dir, snapshotName, v1SnapshotJSON+"\n")

	s := newStore(t, dir)

	uid, ok := s.UIDByTokenHash("hA")
	if !ok || uid != "U1" {
		t.Fatalf("hA resolved to (%q,%v), want (\"U1\",true) — "+
			"the pre-existing token no longer authenticates", uid, ok)
	}
	u, ok := s.User("U1")
	if !ok {
		t.Fatal("U1 missing from roster after v1 load")
	}
	if len(u.Tokens) != 1 {
		t.Fatalf("token count = %d, want 1", len(u.Tokens))
	}
	// The v1 format has exactly one timestamp, so both stamps take it.
	// Substituting time.Now() here would make every upgraded token look
	// freshly used, and the first eviction after an upgrade would then order
	// by the hash tie-break instead of by real staleness.
	if got := u.Tokens[0].AddedAt; got != "2026-01-01T00:00:00Z" {
		t.Errorf("AddedAt = %q, want the v1 createdAt", got)
	}
	if got := u.Tokens[0].LastSeenAt; got != "2026-01-01T00:00:00Z" {
		t.Errorf("LastSeenAt = %q, want the v1 createdAt", got)
	}
}

// TestV1SnapshotControlGroup is TV-8, the control for TV-7.
//
// ★ It asserts the SHAPE OF THE FAILURE, not merely that something fails.
// A v2-only decoder must REFUSE a v1 file. What must never happen is the third
// outcome — loading "successfully" with an empty token list — because that is
// indistinguishable from a user who genuinely has no tokens, and it is the
// exact disaster this plan is built to prevent.
func TestV1SnapshotControlGroup(t *testing.T) {
	dir := tempDir(t)
	writeFile(t, dir, snapshotName, v1SnapshotJSON+"\n")

	// Decode the v1 bytes with the V2 struct — precisely what the
	// "one struct with omitempty on both fields" design would do at runtime.
	s2 := &Store{users: map[string]*User{}, byToken: map[string]string{},
		proposals: map[string]*Proposal{}}
	raw, err := os.ReadFile(filepath.Join(dir, snapshotName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_, err = s2.loadSnapshotV2Only(raw, filepath.Join(dir, snapshotName))

	if err == nil {
		t.Fatalf("CONTROL GROUP FAILED: a v2-only decoder accepted a v1 file. "+
			"users=%d byToken=%d — if this path loads silently, "+
			"TV-7 has no discriminating power", len(s2.users), len(s2.byToken))
	}
	if !strings.Contains(err.Error(), "unsupported snapshot version") {
		t.Errorf("v2-only decoder failed with %q, want an explicit "+
			"unsupported-version error", err)
	}
	// And the decisive half: nothing was loaded, so no half-state is served.
	if len(s2.byToken) != 0 {
		t.Errorf("byToken has %d entries after a refused load; a refused "+
			"snapshot must leave no state behind", len(s2.byToken))
	}
}

const oldEventLogLine = `{"t":"2026-01-01T00:00:00Z","type":"user_create",` +
	`"d":{"UID":"U1","TokenHash":"hA","Name":"小明","CreatedAt":"2026-01-01T00:00:00Z"}}`

// TestOldEventLogReplays is TV-9: a log containing only pre-multi-token
// user_create lines must replay with the token still valid (DD-4).
func TestOldEventLogReplays(t *testing.T) {
	dir := tempDir(t)
	writeFile(t, dir, logName, oldEventLogLine+"\n")

	s := newStore(t, dir)

	uid, ok := s.UIDByTokenHash("hA")
	if !ok || uid != "U1" {
		t.Fatalf("hA resolved to (%q,%v), want (\"U1\",true) — "+
			"replaying an old log logged the user out", uid, ok)
	}
	u, _ := s.User("U1")
	if len(u.Tokens) != 1 || u.Tokens[0].Hash != "hA" {
		t.Fatalf("Tokens = %+v, want exactly one ref for hA", u.Tokens)
	}
	if u.Name != "小明" {
		t.Errorf("Name = %q, want 小明", u.Name)
	}
}

// TestUserCreateDataShapeIsFrozen is TV-10, the control for TV-9.
//
// ★ It is a SOURCE assertion, and that is not laziness — it is the only form
// that works. The defect it guards cannot be caught behaviourally: decoding
// `{"TokenHash":"hA"}` into a struct whose field was renamed to Tokens returns
// NO ERROR (json.Unmarshal ignores unknown fields and zero-fills missing ones).
// Replay would report success, and the only symptom would be every user
// silently logged out. A behavioural test therefore has nothing to observe;
// the shape itself has to be the assertion.
func TestUserCreateDataShapeIsFrozen(t *testing.T) {
	const path = "event.go"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := stripComments(string(b))

	// Control group: prove the matcher can actually find a declaration in this
	// file. Without it, a typo in the needle below yields a clean run that
	// means nothing — "absent" and "broken matcher" share one output.
	if !strings.Contains(src, "UserRenameData") {
		t.Fatal("control group failed: the matcher cannot find a declaration " +
			"known to exist, so a clean result proves nothing")
	}

	const frozen = "UserCreateData     struct{ UID, TokenHash, Name, CreatedAt string }"
	if !strings.Contains(src, frozen) {
		t.Fatalf("UserCreateData no longer has its frozen single-value shape.\n"+
			"want the declaration: %s\n"+
			"★ Old logs carry {\"TokenHash\":\"h\"}. Any other shape decodes "+
			"them WITHOUT ERROR into a zero value, replay reports success, and "+
			"every existing user is logged out with nothing in the log. "+
			"The conversion belongs in applyEvent, not in this struct (DD-4).",
			frozen)
	}
}

// TestUnknownSnapshotVersionIsFatal is TV-11: an unrecognised version is an
// error, never a degraded load.
func TestUnknownSnapshotVersionIsFatal(t *testing.T) {
	dir := tempDir(t)
	writeFile(t, dir, snapshotName,
		`{"version":99,"appliedLines":0,"users":{},"proposals":{}}`+"\n")

	s, err := New(dir)
	if err == nil {
		s.Close()
		t.Fatal("version 99 loaded without error; an unknown version must " +
			"refuse to start rather than serve a partial state")
	}
	if !strings.Contains(err.Error(), "unsupported snapshot version 99") {
		t.Errorf("error = %q, want it to name the unsupported version", err)
	}
}

// TestReplayHonoursRecordedEviction is TV-12: replay evicts the token the
// event NAMES, and does not recompute the choice (DD-6).
//
// ★ The two implementations differ only when the recorded decision and a fresh
// recomputation disagree — which is exactly what throttled LastSeenAt makes
// possible. So the fixture is built so that recomputing would pick a DIFFERENT
// token than the log records: hOld is the one evicted on the log, while hStale
// is the one an LRU recomputation would choose.
func TestReplayHonoursRecordedEviction(t *testing.T) {
	dir := tempDir(t)

	lines := []string{
		`{"t":"2026-01-01T00:00:00Z","type":"user_create","d":` +
			`{"UID":"U1","TokenHash":"hOld","Name":"小明","CreatedAt":"2026-01-01T00:00:00Z"}}`,
	}
	// Fill to the cap with tokens that are all NEWER than hOld…
	for _, h := range []string{"hB", "hC", "hD", "hE"} {
		lines = append(lines, `{"t":"2026-02-01T00:00:00Z","type":"user_add_token","d":`+
			`{"UID":"U1","Hash":"`+h+`","AddedAt":"2026-02-01T00:00:00Z","EvictedHash":""}}`)
	}
	// …then make hStale the least-recently-seen of all of them.
	lines = append(lines, `{"t":"2026-02-01T00:00:00Z","type":"user_add_token","d":`+
		`{"UID":"U1","Hash":"hStale","AddedAt":"2026-02-01T00:00:00Z","EvictedHash":"hB"}}`)
	lines = append(lines, `{"t":"2026-02-02T00:00:00Z","type":"user_touch_token","d":`+
		`{"UID":"U1","Hash":"hStale","SeenAt":"1999-01-01T00:00:00Z"}}`)
	// The recorded decision evicts hOld. A recomputation would evict hStale.
	lines = append(lines, `{"t":"2026-03-01T00:00:00Z","type":"user_add_token","d":`+
		`{"UID":"U1","Hash":"hNew","AddedAt":"2026-03-01T00:00:00Z","EvictedHash":"hOld"}}`)

	writeFile(t, dir, logName, strings.Join(lines, "\n")+"\n")
	s := newStore(t, dir)

	u, ok := s.User("U1")
	if !ok {
		t.Fatal("U1 missing after replay")
	}
	if _, held := u.FindToken("hOld"); held {
		t.Error("hOld is still present; replay ignored the recorded EvictedHash")
	}
	if _, held := u.FindToken("hStale"); !held {
		t.Error("hStale was evicted — replay RECOMPUTED the LRU instead of " +
			"honouring the event. The replayed state now differs from the " +
			"state that was actually served (DD-6)")
	}
	if _, held := u.FindToken("hNew"); !held {
		t.Error("hNew missing after replay")
	}
	if len(u.Tokens) != MaxTokensPerUser {
		t.Errorf("token count = %d, want %d", len(u.Tokens), MaxTokensPerUser)
	}
	// The index must agree with the slice, or an evicted token keeps working.
	if _, ok := s.UIDByTokenHash("hOld"); ok {
		t.Error("byToken still resolves hOld after it was evicted")
	}
}
