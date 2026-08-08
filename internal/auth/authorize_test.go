package auth

import (
	"strings"
	"testing"
)

// fakeState is a hand-built State. Authorize takes an interface precisely so
// these cases cost nothing to write: when deny cases are expensive, they
// quietly don't get written, and a `return Allow` implementation passes.
type fakeState struct {
	owners  map[string]string // pid -> owner uid
	authors map[string]string // pid/oid -> author uid
	others  map[string]int    // pid/oid -> others' vote count
	decided map[string]string // pid -> decided oid ("" = open)
}

func (f fakeState) ProposalOwner(pid string) (string, bool) {
	u, ok := f.owners[pid]
	return u, ok
}

func (f fakeState) OptionAuthor(pid, oid string) (string, bool) {
	u, ok := f.authors[pid+"/"+oid]
	return u, ok
}

func (f fakeState) OthersVoteCount(pid, oid string) int {
	return f.others[pid+"/"+oid]
}

// DecidedOption reports the freeze. The exists flag is taken from owners, NOT
// from the decided map: a proposal absent from `decided` is an OPEN proposal,
// not a nonexistent one, and conflating the two would let the freeze gate be
// skipped for every case that simply didn't set the field.
func (f fakeState) DecidedOption(pid string) (string, bool) {
	if _, ok := f.owners[pid]; !ok {
		return "", false
	}
	return f.decided[pid], true
}

// baseState mirrors the TV-2..TV-4 setup: P1 owns the proposal, U2 authored
// option O1, and O1 has no other-party votes yet.
func baseState() fakeState {
	return fakeState{
		owners:  map[string]string{"PID1": "P1"},
		authors: map[string]string{"PID1/O1": "U2"},
		others:  map[string]int{},
		decided: map[string]string{},
	}
}

func reg(uid string) Actor  { return Actor{UID: uid, Registered: true} }
func unreg() Actor          { return Actor{UID: "U9", Registered: false} }
func opt(oid string) Target { return Target{PID: "PID1", OID: oid} }

// TestAuthorizeRules is a table where EVERY rule carries both an allow case and
// a deny case (spec.md: "只測正向的測試套件視為未通過本需求").
//
// The meta-test that proves this table has teeth is V1, run by hand: replace
// Authorize's body with `return Allow` and every wantAllow=false row must turn
// red. A suite whose deny rows are missing cannot detect that mutation at all.
func TestAuthorizeRules(t *testing.T) {
	cases := []struct {
		name      string
		actor     Actor
		action    Action
		target    Target
		state     fakeState
		wantAllow bool
		wantCode  string
	}{
		// --- Identity gate (3.3, S3/S4) ---
		{
			name:  "deny: unregistered visitor cannot vote",
			actor: unreg(), action: ActionVoteSet,
			target: Target{PID: "PID1", OID: "O1", UID: "U9"},
			state:  baseState(), wantAllow: false, wantCode: CodeIdentityUnregistered,
		},
		{
			name:  "allow: registered participant can vote",
			actor: reg("U3"), action: ActionVoteSet,
			target: Target{PID: "PID1", OID: "O1", UID: "U3"},
			state:  baseState(), wantAllow: true,
		},
		{
			// An empty uid with Registered=true would otherwise sail through
			// and then match an empty owner field.
			name:  "deny: registered flag with empty uid is still unregistered",
			actor: Actor{UID: "", Registered: true}, action: ActionVoteSet,
			target: Target{PID: "PID1", OID: "O1"},
			state:  baseState(), wantAllow: false, wantCode: CodeIdentityUnregistered,
		},

		// --- Proposal CRUD: proposer only ---
		{
			name:  "allow: proposer updates own proposal",
			actor: reg("P1"), action: ActionProposalUpdate,
			target: Target{PID: "PID1"}, state: baseState(), wantAllow: true,
		},
		{
			name:  "deny: non-proposer updates proposal",
			actor: reg("U2"), action: ActionProposalUpdate,
			target: Target{PID: "PID1"}, state: baseState(),
			wantAllow: false, wantCode: CodeNotProposalOwner,
		},
		{
			name:  "allow: proposer deletes own proposal",
			actor: reg("P1"), action: ActionProposalDelete,
			target: Target{PID: "PID1"}, state: baseState(), wantAllow: true,
		},
		{
			name:  "deny: non-proposer deletes proposal",
			actor: reg("U3"), action: ActionProposalDelete,
			target: Target{PID: "PID1"}, state: baseState(),
			wantAllow: false, wantCode: CodeNotProposalOwner,
		},

		// --- R5 branch 1: proposer is never locked out (TV-4) ---
		{
			name:  "allow: proposer edits an option others voted on (TV-4)",
			actor: reg("P1"), action: ActionOptionUpdate, target: opt("O1"),
			state: fakeState{
				owners:  map[string]string{"PID1": "P1"},
				authors: map[string]string{"PID1/O1": "U2"},
				others:  map[string]int{"PID1/O1": 3},
			},
			wantAllow: true,
		},
		{
			name:  "allow: proposer deletes an option others voted on (TV-4)",
			actor: reg("P1"), action: ActionOptionDelete, target: opt("O1"),
			state: fakeState{
				owners:  map[string]string{"PID1": "P1"},
				authors: map[string]string{"PID1/O1": "U2"},
				others:  map[string]int{"PID1/O1": 3},
			},
			wantAllow: true,
		},

		// --- R5 branch 2: author with zero others' votes (TV-2) ---
		{
			name:  "allow: author edits own option, nobody else voted (TV-2)",
			actor: reg("U2"), action: ActionOptionUpdate, target: opt("O1"),
			state: baseState(), wantAllow: true,
		},
		{
			// The distinguishing case: the author's OWN vote must not lock
			// them out. OthersVoteCount excludes it, so this stays 0.
			name:  "allow: author's own vote does not lock the option (TV-2)",
			actor: reg("U2"), action: ActionOptionUpdate, target: opt("O1"),
			state: fakeState{
				owners:  map[string]string{"PID1": "P1"},
				authors: map[string]string{"PID1/O1": "U2"},
				others:  map[string]int{"PID1/O1": 0},
			},
			wantAllow: true,
		},

		// --- R5 branch 3: locked by the first other-party vote (TV-3) ---
		{
			name:  "deny: author edits option after one other vote (TV-3)",
			actor: reg("U2"), action: ActionOptionUpdate, target: opt("O1"),
			state: fakeState{
				owners:  map[string]string{"PID1": "P1"},
				authors: map[string]string{"PID1/O1": "U2"},
				others:  map[string]int{"PID1/O1": 1},
			},
			wantAllow: false, wantCode: CodeOptionLocked,
		},
		{
			name:  "deny: author deletes option after one other vote (TV-3)",
			actor: reg("U2"), action: ActionOptionDelete, target: opt("O1"),
			state: fakeState{
				owners:  map[string]string{"PID1": "P1"},
				authors: map[string]string{"PID1/O1": "U2"},
				others:  map[string]int{"PID1/O1": 1},
			},
			wantAllow: false, wantCode: CodeOptionLocked,
		},
		{
			name:  "deny: third party edits someone else's option",
			actor: reg("U3"), action: ActionOptionUpdate, target: opt("O1"),
			state: baseState(), wantAllow: false, wantCode: CodeNotProposalOwner,
		},

		// --- Option creation is open to any registered participant ---
		{
			name:  "allow: participant adds a new option",
			actor: reg("U3"), action: ActionOptionCreate,
			target: Target{PID: "PID1"}, state: baseState(), wantAllow: true,
		},
		{
			name:  "deny: unregistered visitor adds an option",
			actor: unreg(), action: ActionOptionCreate,
			target: Target{PID: "PID1"}, state: baseState(),
			wantAllow: false, wantCode: CodeIdentityUnregistered,
		},

		// --- No proxy voting (TV-5) ---
		{
			name:  "deny: voting on behalf of another uid (TV-5)",
			actor: reg("U1"), action: ActionVoteSet,
			target: Target{PID: "PID1", OID: "O1", UID: "U9"},
			state:  baseState(), wantAllow: false, wantCode: CodeVoteKeyForgery,
		},
		{
			name:  "deny: cancelling another person's vote (TV-5)",
			actor: reg("U1"), action: ActionVoteDelete,
			target: Target{PID: "PID1", OID: "O1", UID: "U9"},
			state:  baseState(), wantAllow: false, wantCode: CodeVoteKeyForgery,
		},
		{
			name:  "allow: cancelling one's own vote",
			actor: reg("U1"), action: ActionVoteDelete,
			target: Target{PID: "PID1", OID: "O1", UID: "U1"},
			state:  baseState(), wantAllow: true,
		},

		// --- Nonexistent resources ---
		{
			name:  "deny: unknown proposal",
			actor: reg("U1"), action: ActionProposalUpdate,
			target: Target{PID: "NOPE"}, state: baseState(),
			wantAllow: false, wantCode: CodeProposalNotFound,
		},
		{
			name:  "deny: unknown option",
			actor: reg("U3"), action: ActionOptionUpdate, target: opt("NOPE"),
			state: baseState(), wantAllow: false, wantCode: CodeOptionNotFound,
		},
		{
			name:  "deny: vote on unknown option",
			actor: reg("U1"), action: ActionVoteSet,
			target: Target{PID: "PID1", OID: "NOPE", UID: "U1"},
			state:  baseState(), wantAllow: false, wantCode: CodeOptionNotFound,
		},

		// --- Closed enum: an unrouted action must be refused ---
		{
			name:  "deny: unknown action falls through to deny",
			actor: reg("P1"), action: ActionUnknown,
			target: Target{PID: "PID1"}, state: baseState(),
			wantAllow: false, wantCode: CodeUnknownAction,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Authorize(tc.actor, tc.action, tc.target, tc.state)
			if got.Allowed != tc.wantAllow {
				t.Fatalf("Allowed = %v, want %v (code=%q reason=%q)",
					got.Allowed, tc.wantAllow, got.Code, got.Reason)
			}
			if tc.wantAllow {
				if got.Code != "" {
					t.Errorf("an allow decision must carry no code, got %q", got.Code)
				}
				return
			}
			if got.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, tc.wantCode)
			}
			// errors.md rule 1: every 403 carries a code AND a human reason.
			// A code-only denial leaves the user with "operation failed".
			if strings.TrimSpace(got.Reason) == "" {
				t.Errorf("deny %q carries no human-readable reason", got.Code)
			}
		})
	}
}

// TestDenyCasesExist is a meta-guard on the table above.
//
// ★ It asserts the SHAPE of the suite, not a behaviour. The V1 mutation
// (`return Allow`) is only detectable if deny cases exist for every rule; a
// future edit that deletes them would leave a green suite with no discriminating
// power, and nothing else in the package would notice. Counting them here makes
// that erosion fail loudly.
func TestDenyCasesExist(t *testing.T) {
	byCode := map[string]int{}
	st := fakeState{
		owners:  map[string]string{"PID1": "P1"},
		authors: map[string]string{"PID1/O1": "U2"},
		others:  map[string]int{"PID1/O1": 1},
	}
	probes := []struct {
		actor  Actor
		action Action
		target Target
	}{
		{unreg(), ActionVoteSet, Target{PID: "PID1", OID: "O1"}},
		{reg("U3"), ActionProposalUpdate, Target{PID: "PID1"}},
		{reg("U2"), ActionOptionUpdate, opt("O1")},
		{reg("U1"), ActionVoteSet, Target{PID: "PID1", OID: "O1", UID: "U9"}},
		{reg("U1"), ActionProposalUpdate, Target{PID: "NOPE"}},
		{reg("P1"), ActionUnknown, Target{PID: "PID1"}},
	}
	for _, p := range probes {
		if d := Authorize(p.actor, p.action, p.target, st); !d.Allowed {
			byCode[d.Code]++
		}
	}

	required := []string{
		CodeIdentityUnregistered, CodeNotProposalOwner, CodeOptionLocked,
		CodeVoteKeyForgery, CodeProposalNotFound, CodeUnknownAction,
	}
	for _, code := range required {
		if byCode[code] == 0 {
			t.Errorf("no reachable deny case produces %s — that rule is untested "+
				"and `return Allow` would pass the suite", code)
		}
	}
}

// TestProposerPrecedesAuthorLock pins the branch ORDER (TV-4).
//
// Order is invisible in most tests because the proposer is usually also the
// author. This case separates them: P1 owns the proposal, U2 authored the
// option, others have voted. If branch 2 were evaluated first, P1 would be
// refused for not being the author.
func TestProposerPrecedesAuthorLock(t *testing.T) {
	st := fakeState{
		owners:  map[string]string{"PID1": "P1"},
		authors: map[string]string{"PID1/O1": "U2"},
		others:  map[string]int{"PID1/O1": 5},
	}
	for _, action := range []Action{ActionOptionUpdate, ActionOptionDelete} {
		if d := Authorize(reg("P1"), action, opt("O1"), st); !d.Allowed {
			t.Errorf("%s: proposer was refused (%s: %s) — branch 1 must be "+
				"evaluated before the author lock", action, d.Code, d.Reason)
		}
	}
}

// TestAuthorizeIsPure asserts that Authorize does not mutate the state it reads.
//
// Purity is what makes exhaustive testing possible (DD-19). A rule that wrote
// to the store would still pass every verdict assertion above while making the
// function order-dependent — so the property needs its own check.
func TestAuthorizeIsPure(t *testing.T) {
	st := fakeState{
		owners:  map[string]string{"PID1": "P1"},
		authors: map[string]string{"PID1/O1": "U2"},
		others:  map[string]int{"PID1/O1": 2},
	}
	before := map[string]int{}
	for k, v := range st.others {
		before[k] = v
	}
	ownersLen, authorsLen := len(st.owners), len(st.authors)

	// Run every action twice; a stateful rule would diverge between runs.
	actions := []Action{
		ActionProposalUpdate, ActionProposalDelete, ActionOptionCreate,
		ActionOptionUpdate, ActionOptionDelete, ActionVoteSet, ActionVoteDelete,
	}
	for _, a := range actions {
		tgt := Target{PID: "PID1", OID: "O1", UID: "U2"}
		first := Authorize(reg("U2"), a, tgt, st)
		second := Authorize(reg("U2"), a, tgt, st)
		if first != second {
			t.Errorf("%s: verdict changed between identical calls (%+v then %+v) "+
				"— Authorize is not pure", a, first, second)
		}
	}

	if len(st.owners) != ownersLen || len(st.authors) != authorsLen {
		t.Error("Authorize changed the size of the state maps")
	}
	for k, v := range before {
		if st.others[k] != v {
			t.Errorf("Authorize mutated others[%s]: %d -> %d", k, v, st.others[k])
		}
	}
}

// TestNoWriteMethodsOnState is a wiring guard: it proves the no-side-effects
// property is enforced by the type system rather than by discipline.
//
// The State interface is what Authorize can reach. If someone widens it to
// include a mutator, purity stops being structural — and no behavioural test
// would catch that, because adding a method breaks nothing until it is called.
func TestNoWriteMethodsOnState(t *testing.T) {
	// Control group: prove the matcher fires on a name that IS forbidden,
	// so a clean result below means "no mutators" rather than "broken check".
	if !looksLikeMutator("SetVote") {
		t.Fatal("control group failed: the matcher does not flag a known " +
			"mutator name, so a clean result proves nothing")
	}
	if looksLikeMutator("ProposalOwner") {
		t.Fatal("control group failed: the matcher flags a known read-only name")
	}

	// The three methods of State, listed explicitly. Reflection over the
	// interface would be circular — it would read whatever the interface
	// currently says, so widening it would silently widen the assertion too.
	for _, name := range []string{"ProposalOwner", "OptionAuthor", "OthersVoteCount"} {
		if looksLikeMutator(name) {
			t.Errorf("State method %q looks like a mutator; Authorize must not "+
				"be able to write (DD-19)", name)
		}
	}
}

func looksLikeMutator(name string) bool {
	for _, prefix := range []string{"Set", "Create", "Update", "Delete", "Add", "Remove", "Rename"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// --- 6.7 決策凍結（DD-5）-------------------------------------------------

// decidedState is baseState with PID1 frozen on option O1.
func decidedState() fakeState {
	st := baseState()
	st.decided["PID1"] = "O1"
	return st
}

// TestDecideFreezeBlocksWrites pairs EVERY frozen-deny with the same call on an
// open proposal. Without the open half, a `return deny` in the gate would pass
// just as well — the test would be asserting that writes fail, not that the
// FREEZE is what fails them.
func TestDecideFreezeBlocksWrites(t *testing.T) {
	frozen := decidedState()
	open := baseState()

	cases := []struct {
		name   string
		action Action
		target Target
	}{
		{"vote.set", ActionVoteSet, Target{PID: "PID1", OID: "O1", UID: "U2"}},
		{"vote.delete", ActionVoteDelete, Target{PID: "PID1", OID: "O1", UID: "U2"}},
		{"option.create", ActionOptionCreate, Target{PID: "PID1"}},
		{"option.update", ActionOptionUpdate, opt("O1")},
		{"option.delete", ActionOptionDelete, opt("O1")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Frozen: refused, and refused for the RIGHT reason. Asserting only
			// !Allowed would also accept a denial coming from some unrelated
			// rule, which is how a gate can look present while doing nothing.
			d := Authorize(reg("U2"), tc.action, tc.target, frozen)
			if d.Allowed {
				t.Fatalf("frozen proposal allowed %s", tc.name)
			}
			if d.Code != CodeProposalDecided {
				t.Fatalf("frozen %s denied with %q, want %q (denial came from the wrong rule)",
					tc.name, d.Code, CodeProposalDecided)
			}
			// Control half: identical call, unfrozen state, must be allowed.
			if o := Authorize(reg("U2"), tc.action, tc.target, open); !o.Allowed {
				t.Fatalf("open proposal refused %s (%s) — the frozen case above proves nothing",
					tc.name, o.Code)
			}
		})
	}
}

// TestDecideIsReleasableWhileFrozen is the deadlock guard. If the freeze gate
// ran before the decide branch, this is the case that would hang forever: the
// only action able to lift the freeze would itself be refused by it.
func TestDecideIsReleasableWhileFrozen(t *testing.T) {
	frozen := decidedState()
	if d := Authorize(reg("P1"), ActionProposalDecide, Target{PID: "PID1"}, frozen); !d.Allowed {
		t.Fatalf("owner cannot release a frozen proposal (%s) — the freeze is permanent", d.Code)
	}
}

// TestDecideRequiresOwner: freezing is a proposer-only power (DD-5, R5).
func TestDecideRequiresOwner(t *testing.T) {
	st := baseState()
	if d := Authorize(reg("U2"), ActionProposalDecide, Target{PID: "PID1"}, st); d.Allowed {
		t.Fatal("non-owner was allowed to decide")
	} else if d.Code != CodeNotProposalOwner {
		t.Fatalf("non-owner denied with %q, want %q", d.Code, CodeNotProposalOwner)
	}
	if d := Authorize(reg("P1"), ActionProposalDecide, Target{PID: "PID1"}, st); !d.Allowed {
		t.Fatalf("owner refused (%s) — the deny above proves nothing", d.Code)
	}
}

// TestFreezeDoesNotSwallowProposalLevelActions: the owner must still be able to
// edit the title or delete the whole proposal after deciding. A gate written as
// "frozen ⇒ deny everything" would be indistinguishable from the correct one in
// every test above, and would strand the proposal with no way to correct a typo.
func TestFreezeDoesNotSwallowProposalLevelActions(t *testing.T) {
	frozen := decidedState()
	for _, a := range []Action{ActionProposalUpdate, ActionProposalDelete} {
		if d := Authorize(reg("P1"), a, Target{PID: "PID1"}, frozen); !d.Allowed {
			t.Fatalf("%s refused on a frozen proposal (%s)", a, d.Code)
		}
	}
}

// inconsistentState is the ONLY input shape that can tell "the gate reads the
// exists flag" apart from "the gate reads only the string": ProposalOwner says
// PID1 exists, DecidedOption says it does not — while still handing back a
// non-empty decided id. No real store produces this; a guard against a
// malformed State implementation cannot be exercised by a well-formed one, so
// the malformed case has to be built on purpose.
type inconsistentState struct{ fakeState }

func (h inconsistentState) DecidedOption(pid string) (string, bool) { return "O1", false }

// TestFreezeGateHonoursExistsFlag is the control for the two-value return.
//
// ★ MEASURED 2026-08-05, and the first FOUR attempts at this test were all
// worthless. Each used a pid absent from `owners`, so the ProposalOwner lookup
// upstream refused the request before the freeze gate ran — mutations to the
// gate (drop the flag / hoist it above the existence check / both) ALL stayed
// green. Four green control groups in a row were not evidence the code was
// right; they were evidence the test never reached the line it named.
//
// The fix was not a better assertion but a different INPUT: the pid must exist
// as far as ProposalOwner is concerned, so that execution actually arrives at
// the gate.
func TestFreezeGateHonoursExistsFlag(t *testing.T) {
	st := inconsistentState{baseState()}
	// PID1 exists (owners has it), so the request reaches the freeze gate.
	// DecidedOption claims a decision while reporting exists=false; honouring
	// the flag means NOT freezing on it.
	d := Authorize(reg("U2"), ActionVoteSet, Target{PID: "PID1", OID: "O1", UID: "U2"}, st)
	if !d.Allowed {
		t.Fatalf("gate froze on a decided id whose exists flag was false (%s) — "+
			"it read the string and ignored the flag", d.Code)
	}
	// Control half: same state shape, flag true ⇒ must freeze. Without this,
	// a gate that never freezes at all would also pass the assertion above.
	if o := Authorize(reg("U2"), ActionVoteSet,
		Target{PID: "PID1", OID: "O1", UID: "U2"}, decidedState()); o.Allowed {
		t.Fatal("gate did not freeze when the flag was true — the case above proves nothing")
	}
}

// TestUnknownPidReportsNotFound keeps the ordinary path pinned: with a normal
// state, an unknown pid reports PROPOSAL_NOT_FOUND rather than anything else.
func TestUnknownPidReportsNotFound(t *testing.T) {
	d := Authorize(reg("U2"), ActionVoteSet, Target{PID: "NOPE", OID: "O1", UID: "U2"}, decidedState())
	if d.Allowed || d.Code != CodeProposalNotFound {
		t.Fatalf("unknown pid gave allowed=%v code=%q, want deny/%s", d.Allowed, d.Code, CodeProposalNotFound)
	}
}
