package auth

import "fmt"

// Decision is the verdict of Authorize. It carries the machine-readable code
// that the HTTP layer turns into a JSON body (errors.md: "每個 403 都必須帶
// Code 與人話理由" — a bare 403 tells the user only that something failed).
type Decision struct {
	Allowed bool
	// Code is one of the CODE constants below; empty when Allowed.
	Code string
	// Reason is the human-readable half, in the user's language.
	Reason string
}

// Allow is the sole affirmative verdict.
var Allow = Decision{Allowed: true}

// Deny codes, mirroring errors.md's authorisation table.
const (
	CodeIdentityUnregistered = "IDENTITY_UNREGISTERED"
	CodeNotProposalOwner     = "NOT_PROPOSAL_OWNER"
	CodeOptionLocked         = "OPTION_LOCKED"
	CodeVoteKeyForgery       = "VOTE_KEY_FORGERY"
	CodeProposalNotFound     = "PROPOSAL_NOT_FOUND"
	CodeOptionNotFound       = "OPTION_NOT_FOUND"
	CodeUnknownAction        = "UNKNOWN_ACTION"
	CodeProposalDecided      = "PROPOSAL_DECIDED"
)

func deny(code, reason string) Decision {
	return Decision{Allowed: false, Code: code, Reason: reason}
}

// Action enumerates every authorised operation.
//
// ★ A closed enum, not a string: an unrecognised action hits the default branch
// and is DENIED. With a bare string, a typo at a call site would fall through
// whatever branch happened to match — or none — and the endpoint would end up
// unprotected while every test still passed.
type Action int

const (
	ActionUnknown Action = iota
	ActionProposalUpdate
	ActionProposalDelete
	ActionOptionCreate
	ActionOptionUpdate
	ActionOptionDelete
	ActionVoteSet
	ActionVoteDelete
	ActionProposalDecide
	ActionProposalRepid
)

func (a Action) String() string {
	switch a {
	case ActionProposalUpdate:
		return "proposal.update"
	case ActionProposalDelete:
		return "proposal.delete"
	case ActionOptionCreate:
		return "option.create"
	case ActionOptionUpdate:
		return "option.update"
	case ActionOptionDelete:
		return "option.delete"
	case ActionVoteSet:
		return "vote.set"
	case ActionVoteDelete:
		return "vote.delete"
	case ActionProposalDecide:
		return "proposal.decide"
	case ActionProposalRepid:
		return "proposal.repid"
	}
	return "unknown"
}

// Actor is the authenticated subject.
//
// ★ UID is ALWAYS derived from the bearer token by the HTTP layer, never read
// from a request body (DD-8, TV-5). Registered is the S3/S4 identity gate:
// a visitor with a token but no name may read, never write.
type Actor struct {
	UID        string
	Registered bool
}

// Target names the resource being acted upon. OID is empty for proposal-level
// actions; UID is the vote's subject for vote actions.
type Target struct {
	PID string
	OID string
	UID string
}

// State is the read-only slice of the store that Authorize needs.
//
// ★ An interface, not *store.Store, for two reasons that both matter:
//
//  1. It makes the no-side-effects property (DD-19) STRUCTURAL rather than a
//     promise in a comment — there is no method here that could write, so a
//     rule that tried to mutate would not compile.
//  2. It keeps the rules testable without a real store, so deny cases are
//     exactly as cheap to write as allow cases. That cost symmetry is the whole
//     point of DD-19: when deny cases are expensive, they quietly don't get
//     written, and a `return Allow` implementation passes the suite.
//
// The methods are deliberately narrow: each returns one scalar plus an
// existence flag, so the type system cannot express "read the whole proposal
// and decide". The existence flag also keeps "no such proposal" distinct from
// "owned by the empty string" — collapsing those would let a request for a
// nonexistent pid be authorised against a zero value.
type State interface {
	ProposalOwner(pid string) (string, bool)
	OptionAuthor(pid, oid string) (string, bool)
	OthersVoteCount(pid, oid string) int
	// DecidedOption returns the frozen option id ("" when open) plus the
	// proposal-exists flag.
	//
	// ★ MEASURED, and the honest version: inside Authorize the exists flag is
	// currently REDUNDANT — the ProposalOwner lookup above already refuses an
	// unknown pid before the freeze gate is reached, so dropping the flag here
	// changes no observable behaviour (control group CG-D, 2026-08-05, stayed
	// green). It is kept because an accessor that returns only "" for both
	// "open" and "no such proposal" is a defect waiting for its second caller:
	// the next consumer of this interface has no way to tell them apart, and
	// the collapse is silent. Do NOT read this flag as a live guard — the guard
	// is the ProposalOwner check, and it is the ORDER that carries it.
	DecidedOption(pid string) (string, bool)
}

// Authorize is the single security boundary (design.md A6). Pure: no I/O, no
// clock, no mutation.
//
// The frontend hiding a button is convenience, never protection — every test
// that matters here bypasses the UI with curl (TV-3, TV-5).
func Authorize(actor Actor, action Action, target Target, state State) Decision {
	// Identity gate first (S3/S4). Every action below is a write, so an
	// unregistered actor is refused before any rule runs.
	if !actor.Registered || actor.UID == "" {
		return deny(CodeIdentityUnregistered,
			"請先登記姓名才能進行修改（未登記的訪客可以瀏覽，但不能寫入）")
	}

	owner, ok := state.ProposalOwner(target.PID)
	if !ok {
		// Refusing an unknown pid here keeps every rule below able to assume
		// the proposal exists.
		return deny(CodeProposalNotFound, "找不到此提案")
	}

	// ★ The decide/release action is evaluated BEFORE the freeze gate below.
	// Order is load-bearing: if the gate ran first, a decided proposal could
	// never be released — the only action able to lift the freeze would itself
	// be refused by it, and the proposal would be permanently frozen with no
	// error message explaining why.
	// ★ repid 與 decide 一樣在凍結閘之前裁決。一個已定案的提案，其分享網址
	// 同樣可能外洩——若凍結閘擋住 repid，撤銷手段就會在最需要它的狀態下失效。
	if action == ActionProposalDecide || action == ActionProposalRepid {
		if actor.UID != owner {
			return deny(CodeNotProposalOwner, "只有提案人可以標記定案或更換分享網址")
		}
		return Allow
	}

	// ★ Freeze gate (DD-5, task 6.7). It lives HERE, at the single boundary
	// every write already passes through, and not in each handler: a handler
	// that forgot the check would stay writable forever while every existing
	// test still passed, because nothing observes the absence of a check.
	//
	// Reads are unaffected — the gate is only reached by the write actions
	// enumerated in this function.
	if decided, ok := state.DecidedOption(target.PID); ok && decided != "" {
		switch action {
		case ActionVoteSet, ActionVoteDelete,
			ActionOptionCreate, ActionOptionUpdate, ActionOptionDelete:
			// Option edits are frozen too, not just votes. Allowing them would
			// let the decided option be deleted or its dates rewritten out from
			// under a decision everyone has already acted on — the decision
			// would still read as valid while pointing at something else.
			return deny(CodeProposalDecided,
				"這個提案已經定案，投票與可選項都已凍結；提案人可先解除定案再修改")
		}
	}

	switch action {
	case ActionProposalUpdate, ActionProposalDelete:
		// R5 branch 1 at the proposal level: only the proposer.
		if actor.UID != owner {
			return deny(CodeNotProposalOwner, "只有提案人可以修改或刪除這個提案")
		}
		return Allow

	case ActionOptionCreate:
		// Anyone registered may add a candidate range; adding is not voting.
		return Allow

	case ActionOptionUpdate, ActionOptionDelete:
		return authorizeOptionEdit(actor, action, target, state, owner)

	case ActionVoteSet, ActionVoteDelete:
		// ★ No-proxy-voting (DD-8, TV-5). The vote key embeds a uid; if the
		// requested uid is not the token's own, someone hand-built the request.
		// The HTTP layer already derives uid from the token, so this is the
		// second of two independent barriers, not the only one.
		if target.UID != actor.UID {
			return deny(CodeVoteKeyForgery,
				"不能代替其他人投票或取消投票")
		}
		if _, ok := state.OptionAuthor(target.PID, target.OID); !ok {
			return deny(CodeOptionNotFound, "找不到這個可選項")
		}
		return Allow
	}

	// ★ Default DENY. An action constant added later but not routed above is
	// refused rather than silently permitted — the failure mode of a missing
	// case must be "blocked", never "wide open".
	return deny(CodeUnknownAction, fmt.Sprintf("不支援的操作：%s", action))
}

// authorizeOptionEdit implements R5's three mutually exclusive branches.
//
// ★ ORDER IS LOAD-BEARING (TV-4). Branch 1 must be evaluated BEFORE branch 2.
// Writing it as "author AND no others' votes" first would wrongly refuse the
// proposer on an option that others have voted on — and that defect is
// invisible in any test where the proposer happens to also be the author.
func authorizeOptionEdit(actor Actor, action Action, target Target, state State, owner string) Decision {
	// Branch 1: the proposer always wins.
	if actor.UID == owner {
		return Allow
	}

	author, ok := state.OptionAuthor(target.PID, target.OID)
	if !ok {
		return deny(CodeOptionNotFound, "找不到這個可選項")
	}

	// Branch 3 (partial): not the proposer and not the author.
	if actor.UID != author {
		return deny(CodeNotProposalOwner,
			"只有新增這個可選項的人或提案人可以修改它")
	}

	// Branch 2: the author, provided nobody ELSE has voted on it.
	//
	// ★ OthersVoteCount excludes the author's own vote (使用者裁示: 參加者自選
	// 不算). Using the total instead would make TV-2 and TV-3 indistinguishable
	// — the author voting for their own option would lock them out of editing
	// it, and both vectors would return the same verdict for opposite reasons.
	if n := state.OthersVoteCount(target.PID, target.OID); n > 0 {
		return deny(CodeOptionLocked,
			fmt.Sprintf("已有 %d 人投票給這個可選項，無法再修改；如需變更請提案人代為處理", n))
	}
	return Allow
}
