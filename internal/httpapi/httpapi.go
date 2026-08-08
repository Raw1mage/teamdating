// Package httpapi wires the HTTP surface: identity (DD-16), authorisation
// (DD-19) and the REST endpoints of tasks 3 and 4.
package httpapi

import (
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync/atomic"

	"teamdating/internal/hub"
	"teamdating/internal/mailer"
	"teamdating/internal/store"
)

// api holds the handler dependencies.
type api struct {
	store *store.Store
	hub   *hub.Hub
	// base is this service's URL path prefix, and it exists here for ONE
	// reason: the auth cookie's Path must match it. See cookiePath.
	base string
	// publicURL is the operator-declared canonical origin ("https://host"), or
	// empty when nobody declared one. It exists because the ONLY other way to
	// learn the external scheme behind a TLS-terminating proxy is a forwarded
	// header, which this codebase refuses to trust anywhere (see verifyLink).
	//
	// ★ Scheme+host ONLY, no path and no trailing slash — normalised once in
	// newWithSender so a caller cannot pass a shape that silently produces a
	// double slash or a swallowed prefix in a mailed link.
	publicURL string
	// candidateProbes counts how many times the register endpoint answered an
	// UNAUTHENTICATED caller with a same-name candidate list.
	//
	// ★ THIS COUNTER EXISTS BECAUSE THE BRANCH IT COUNTS IS THE ONLY WAY TO
	// QUESTION THIS SERVICE WITHOUT LEAVING A TRACE. Every other anonymous
	// write path ends in a store mutation, so the event log records that it
	// happened; the multi-candidate branch only reads, and a read that changes
	// nothing writes nothing. Without this counter, "nobody has ever probed a
	// name" and "someone has been probing one name all week" produce byte-
	// identical evidence — which is the same absence/failure conflation R8
	// avoids structurally elsewhere by having no listing route at all.
	//
	// ★ Deliberately NOT an event in the replay log. That log is state to be
	// replayed, and an unauthenticated read path that appends to it would (a)
	// let anyone grow the startup cost without bound and (b) add one more
	// event type that old binaries cannot parse (DD-5 is not rollback-safe as
	// it is). Observability must not be bought with durability.
	candidateProbes atomic.Uint64

	// pending holds the outstanding magic links (DD-4). In MEMORY, never in
	// the event log: a verification token is a 15-minute secret, and the log is
	// replayed forever.
	pending *pendingTable

	// mailer sends the verification mail (DD-10).
	//
	// ★ AN INTERFACE FIELD, so a test can inject a failing sender — without
	// that, the fail-loud branch (TV-10: SMTP_SEND_FAILED must refuse the
	// binding) is never executed by anything, and an untested refusal is
	// indistinguishable from one that silently proceeds. The production value
	// is wired in New; TestMailerIsWiredIntoNew reads the source to prove that
	// line exists, because an injected-only test leaves exactly that line
	// unexecuted.
	mailer mailer.Sender
}

// New builds the router. Go 1.22+ pattern routing is used directly — no router
// package (DD-13).
//
// assetSource is reported by /api/health so the empty-directory run (V2) can
// tell "served from the embedded copy" from "served from disk". Those two cases
// are indistinguishable in a browser, which is exactly why the check needs the
// server to say which one it used rather than inferring it from a page loading.
func New(st *store.Store, assets fs.FS, assetSource, base, publicURL string) http.Handler {
	// The REAL sender. This is the line TestMailerIsWiredIntoNew asserts on:
	// every behavioural test runs against a fake, so nothing else in the suite
	// would ever execute it — a service that mails nothing would otherwise pass
	// the whole suite green.
	h, _ := newWithSender(st, assets, assetSource, base, publicURL, mailer.NewSMTPSender())
	return h
}

// newWithSender is New with the mail transport supplied.
//
// ★ UNEXPORTED, and the ONLY reason it exists is that TV-10's failure branch
// is unreachable otherwise: a test cannot make a real relay refuse on demand,
// so "SMTP_SEND_FAILED must refuse the binding" could never be executed and an
// implementation that bound the device anyway would look identical.
//
// ★ It does NOT default a nil sender to the real one. A nil-means-production
// fallback would make "the test forgot to inject" silently reach out to the
// network from a unit test — absent and configured must not share an outcome.
// ★ It also returns the *api so a test can reach the pending table's clock.
// TV-3 ("an expired link is refused") is otherwise UNEXECUTABLE without
// sleeping fifteen real minutes — and an expiry check that no test ever runs
// is indistinguishable from no expiry check at all.
func newWithSender(st *store.Store, assets fs.FS, assetSource, base, publicURL string, sender mailer.Sender) (http.Handler, *api) {
	h := hub.New()
	st.SetObserver(func(seq uint64, ev store.Event) {
		h.Observe(seq, ev.Type, ev.D)
	})
	a := &api{
		store: st, hub: h, base: base,
		publicURL: strings.TrimSuffix(strings.TrimSpace(publicURL), "/"),
		pending:   newPendingTable(),
		mailer:    sender,
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		ts := st.TokenStats()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":        true,
			"assets":    assetSource,
			"users":     st.UserCount(),
			"proposals": st.ProposalCount(),
			// pendingCompact is reported because a compaction that quietly
			// stopped running looks exactly like one that is not due yet
			// (6.3). A number that only ever grows is the signal.
			"pendingCompact": st.PendingSinceCompact(),
			// Token-population figures (observability.md). The multi-token
			// model has no metrics system to fall back on — this endpoint and
			// the event log ARE the observability.
			//
			// ★ tokensTotal / users is "average devices per person"; a ratio
			// near the cap means the cap is too tight. usersAtTokenLimit is
			// the only empirical evidence about whether the cap is set
			// correctly at all: permanently 0 means it has never engaged.
			"tokensTotal":       ts.Total,
			"usersAtTokenLimit": ts.AtLimit,
			// ★ Eviction is a user-visible harm — some device was logged out
			// with no explanation possible, because its token no longer
			// exists. A rising number here is the only warning before the
			// complaints arrive. Process-local by design; the lifetime total
			// is recoverable from the log's EvictedHash fields.
			"evictionsTotal": st.Evictions(),
			// ★ The register endpoint's one traceless branch, made countable.
			// A number that climbs while registrations do not is somebody
			// enumerating a name, and this is the only place that shows it.
			// Process-local: it is a probe rate signal, not an audit trail.
			"candidateProbesTotal": a.candidateProbes.Load(),
			// ★ THE LIVE OUTSTANDING-MAGIC-LINK COUNT IS DELIBERATELY ABSENT
			// FROM THIS PUBLIC RESPONSE, and its absence is the fix rather than
			// an oversight. verify-email answers a matching address and a
			// non-matching one identically, on purpose, so the endpoint cannot
			// be asked "is this address on file". But a MATCH mints a pending
			// entry and a MISS does not — so publishing the table size here
			// handed that same answer back through a side channel: read health,
			// submit a guess, read health again, and the delta IS the oracle the
			// uniform response denied. Any figure that moves with a SINGLE
			// pending entry rebuilds it, so no such figure is exported.
			//
		// ★ A HIGH-WATER MARK IS NOT AN ADEQUATE SUBSTITUTE EITHER, WHICH IS
		// WHY THERE IS NO REPLACEMENT FIGURE HERE AT ALL. A monotonic mark
		// resists the attack only once the table has already been busy: on an
		// idle service the first correct guess takes the mark from 0 to 1 and
		// is just as legible as the live count was. "Harder to exploit" is not
		// the property needed — the response must carry no per-guess signal at
		// all, and the only figure that reliably carries none is no figure.
		//
		// The observability need — telling "the sweep works" from "the sweep
		// never runs" — is met by the operator log instead (pendingTable.put
		// records each new peak), which an anonymous caller cannot read. Moving
		// the signal to a channel that requires host access is the whole fix:
		// the operator keeps the evidence, the prober gets nothing.
		})
	})

	// --- Identity (3.1-3.4) ---
	mux.HandleFunc("GET /api/me", a.handleMe)
	mux.HandleFunc("POST /api/register", a.handleRegister)

	// --- Email verification (identity plan A3, DD-1) ---
	mux.HandleFunc("POST /api/verify-email", a.handleVerifyEmail)
	// Both methods reach the same handler: a mail client can only GET, and the
	// frontend prefers POST. The single-use, short-lived, unguessable token is
	// what makes the GET defensible — see handleVerifyConfirm.
	mux.HandleFunc("GET /api/verify-email/confirm", a.handleVerifyConfirm)
	mux.HandleFunc("POST /api/verify-email/confirm", a.handleVerifyConfirm)
	// --- Gear modal identity management (3.6, DD-6) ---
	mux.HandleFunc("POST /api/identity", a.handleIdentity)

	// --- Proposals (4.1) ---
	mux.HandleFunc("POST /api/proposals", a.handleCreateProposal)
	mux.HandleFunc("GET /api/proposals/{pid}", a.handleGetProposal)
	mux.HandleFunc("PATCH /api/proposals/{pid}", a.handleUpdateProposal)
	mux.HandleFunc("DELETE /api/proposals/{pid}", a.handleDeleteProposal)

	// --- Subject-scoped query (4.2, DD-20) ---
	//
	// ★ Registered BEFORE the {pid} pattern would be ambiguous, so note the
	// shape: "/api/my/proposals" is a distinct path, not a pid named "my".
	// There is deliberately NO "GET /api/proposals" — that route's absence is
	// what makes R8 structural (risk B), and TestNoEnumerationRoute asserts it.
	mux.HandleFunc("GET /api/my/proposals", a.handleMyProposals)

	// --- Options and votes (4.3) ---
	mux.HandleFunc("POST /api/proposals/{pid}/options", a.handleCreateOption)
	mux.HandleFunc("PATCH /api/proposals/{pid}/options/{oid}", a.handleUpdateOption)
	mux.HandleFunc("DELETE /api/proposals/{pid}/options/{oid}", a.handleDeleteOption)
	mux.HandleFunc("POST /api/proposals/{pid}/decision", a.handleDecide)
	mux.HandleFunc("POST /api/proposals/{pid}/repid", a.handleRepid)
	mux.HandleFunc("POST /api/proposals/{pid}/votes", a.handleSetVote)
	mux.HandleFunc("DELETE /api/proposals/{pid}/votes/{oid}", a.handleDeleteVote)

	// --- Realtime (4.4-4.6) ---
	mux.HandleFunc("GET /api/proposals/{pid}/stream", a.handleStream)
	mux.HandleFunc("POST /api/proposals/{pid}/cursor", a.handleCursor)

	// --- Admin: DELIBERATELY ABSENT from this mux (BR_20260805) ---
	//
	// ★ The admin enumeration used to be registered here behind requireLoopback.
	// That guard was measured to protect NOTHING in the real deployment: a
	// reverse proxy on the same host forwards every request from 127.0.0.1, so
	// r.RemoteAddr is loopback for the entire internet and
	//   curl http://<lan-ip>:1080/teamdating/api/admin/users
	// returned 200 with the full roster.
	//
	// The original reasoning ("RemoteAddr is the kernel's view and cannot be
	// spoofed by the peer") is correct in itself — deployment inverted its
	// premise, and nothing in the process could observe that. So the fix cannot
	// be a better predicate on the request: any predicate here is evaluated
	// after the proxy has already erased the caller's identity.
	//
	// A route that is not registered cannot be reached by a mistake in a
	// predicate. That is the same reason R8 has no "GET /api/proposals": the
	// ABSENCE is the guarantee. Admin now lives on its own listener — see
	// NewAdmin — on a port that is not in the gateway's route table.

	// Static assets last: "/" is the catch-all pattern, so every API route
	// registered above still wins.
	mux.Handle("GET /", http.FileServerFS(assets))

	// Rate limiting wraps the WHOLE mux rather than individual handlers
	// (tasks 6.2). Same reasoning as requireLoopback once had: a per-handler
	// call is one forgotten line away from an unlimited write route, and the
	// omission produces no error — the route simply works without a limit.
	// Wrapping means a new mutating route is covered the moment it is added.
	//
	// Numbers: 5 writes/sec sustained with a burst of 20 per identity. A human
	// clicking through the UI never approaches this (a vote is one write); a
	// script does immediately. Registration is far tighter — see limitWrites.
	writes := newLimiter(5, 20, nil)
	registers := newLimiter(0.5, 10, nil)
	// Cursor fan-out is charged separately (see isTransientFanout). 30/sec with
	// a burst of 60 comfortably fits the front end's 60ms throttle (~16/sec)
	// while still refusing an unthrottled script.
	cursors := newLimiter(30, 60, nil)
	return limitWrites(writes, registers, cursors, compactAfterWrite(st, store.DefaultCompactThreshold, mux)), a
}

// compactAfterWrite runs a due compaction after a mutating request completes
// (6.3).
//
// ★ AFTER, never during: Store.Compact takes writeMu, and commit already
// holds it — calling from inside a mutator would deadlock on a non-reentrant
// lock. Once the handler has returned, the lock is free.
//
// ★ Wrapping the whole mux, for the same reason limitWrites does: a
// per-handler call is one forgotten line away from a route whose writes never
// count toward compaction, and that omission produces no error anywhere — the
// log simply grows forever while every test stays green.
//
// ★ A compaction failure must NOT fail the request. The write is already
// durable in the log (DD-14); compaction only shortens replay. Turning a
// housekeeping failure into a user-visible 500 would discard a write that
// actually succeeded.
// ★ threshold is a parameter so a test can drive this REAL middleware instead
// of a stand-in. It is NOT a general injection seam: New passes the production
// constant, and TestCompactionIsWiredIntoNew reads the source to prove it —
// because a test that only ever exercises an injected value leaves the one
// production line that matters unexecuted by anything.
func compactAfterWrite(st *store.Store, threshold int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return
		}
		if _, err := st.CompactIfDue(threshold); err != nil {
			log.Printf("compaction failed (data is still durable in the event log): %v", err)
		}
	})
}

// NewAdmin builds the admin-only handler, meant for a SEPARATE listener bound
// to loopback and never published through a gateway.
//
// ★ requireLoopback is still applied, but understand what it is worth here:
// it is defence in depth, NOT the guarantee. If this listener is ever exposed
// through a same-host proxy it will fail exactly as it failed on the public
// mux. The guarantee is the bind address plus the absence of a route entry;
// this wrapper only catches the case where the operator binds it wider than
// loopback by accident.
func NewAdmin(st *store.Store) http.Handler {
	a := &api{store: st, hub: hub.New()}
	mux := http.NewServeMux()
	mux.Handle("GET /api/admin/proposals", requireLoopback(http.HandlerFunc(a.handleAdminProposals)))
	mux.Handle("GET /api/admin/users", requireLoopback(http.HandlerFunc(a.handleAdminUsers)))
	return mux
}

// requireLoopback refuses any request whose TCP peer is not local (DD-3).
//
// ★ X-Forwarded-For is never consulted. Behind a reverse proxy any client can
// send that header with any value, so honouring it would let a remote caller
// claim 127.0.0.1 and walk into the admin route (risk C). RemoteAddr is the
// kernel's view of the connection and cannot be spoofed by the peer.
func requireLoopback(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r) {
			writeErr(w, http.StatusForbidden, "ADMIN_NOT_LOOPBACK",
				"管理路由只接受來自本機的連線")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleAdminProposals is the ONE enumeration in the system, reachable only
// from loopback. Every public listing route is deliberately absent (R8).
func (a *api) handleAdminProposals(w http.ResponseWriter, r *http.Request) {
	all := a.store.AllProposalsAdminOnly()
	out := make([]proposalView, 0, len(all))
	viewer := identify(a.store, r)
	for _, p := range all {
		out = append(out, a.viewProposal(p.PID, viewer))
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposals": out})
}

func (a *api) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	users := a.store.Users()
	out := make([]voterRef, 0, len(users))
	for _, u := range users {
		// ★ TokenHash is deliberately NOT included. It is not the plaintext
		// token, but it IS the lookup key for an identity — leaking it over
		// any route, even a local one, hands over the ability to impersonate.
		out = append(out, voterRef{UID: u.UID, Name: u.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}
