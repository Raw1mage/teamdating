package httpapi

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestFrontendHandlesEveryEventType reads BOTH sources and compares them.
//
// ★ Why a source-reading guard and not a behavioural test: the frontend's SSE
// listener list is a hand-written enumeration of strings. Omitting one produces
// NO error anywhere — the backend still sends the event, the browser's
// EventSource simply has nobody attached to that name, and es.onmessage does
// not fire for NAMED events either. So the screen silently stops updating for
// that one change class. "Handler missing" and "nothing happened" share one
// output, which is exactly the defect shape this repo keeps paying for.
//
// Measured 2026-08-05: proposal_decide was added to the backend and omitted
// here; every Go test stayed green and the browser probe timed out waiting for
// a button that would never appear.
func TestFrontendHandlesEveryEventType(t *testing.T) {
	evSrc, err := os.ReadFile("../store/event.go")
	if err != nil {
		t.Fatalf("read event.go: %v", err)
	}
	appSrc, err := os.ReadFile("../../web/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}

	// Backend: every Ev* constant's string value.
	constRe := regexp.MustCompile(`(?m)^\tEv[A-Za-z]+\s*=\s*"([a-z_]+)"`)
	ms := constRe.FindAllStringSubmatch(string(evSrc), -1)
	if len(ms) < 5 {
		// A regex that silently matches nothing would make this test pass on an
		// empty set — the same absent/failure collapse it exists to catch.
		t.Fatalf("only %d event constants found in event.go; the regex has lost "+
			"its grip on the source and this test is measuring nothing", len(ms))
	}
	var backend []string
	for _, m := range ms {
		backend = append(backend, m[1])
	}

	// Frontend: the SERVER_EVENTS array.
	start := strings.Index(string(appSrc), "const SERVER_EVENTS = [")
	if start < 0 {
		t.Fatal("SERVER_EVENTS not found in app.js — the listener list was renamed " +
			"or removed; this guard cannot see it any more")
	}
	end := strings.Index(string(appSrc)[start:], "]")
	if end < 0 {
		t.Fatal("SERVER_EVENTS array is unterminated")
	}
	block := string(appSrc)[start : start+end]
	have := map[string]bool{}
	for _, q := range regexp.MustCompile(`'([a-z_]+)'`).FindAllStringSubmatch(block, -1) {
		have[q[1]] = true
	}
	if len(have) < 5 {
		t.Fatalf("only %d event names parsed out of SERVER_EVENTS; the parse failed", len(have))
	}

	var missing []string
	for _, ev := range backend {
		if !have[ev] {
			missing = append(missing, ev)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("web/app.js does not listen for %v — the backend broadcasts these and "+
			"the browser silently ignores them (no error, the view just stops updating)", missing)
	}
}

// TestEveryWriteHandlerConsultsAuthorize reads the source and pins the
// universal claim in tasks 3.6: "所有 HTTP handler 一律先呼叫 Authorize".
//
// ★ Behavioural tests cannot see this. Each one probes the handlers it thinks
// to probe; a handler added later with no authorisation call produces no
// failure anywhere — the endpoint is simply open, and every existing test stays
// green. Found by an audit on 2026-08-05: the claim was already false for two
// handlers, and nothing reported it.
//
// ★ Bodies are split on EVERY top-level func, not just handle*. The first
// version split on handle* only, which silently appended a shared helper's body
// to whichever handler happened to precede it — so handleSetVote and
// handleDeleteVote, two identical one-line delegations to vote(), got OPPOSITE
// verdicts. Same construct, different answer ⇒ the criterion was wrong, not the
// code. One level of delegation is now resolved explicitly.
func TestEveryWriteHandlerConsultsAuthorize(t *testing.T) {
	// Handlers that legitimately do not call Authorize, with the reason.
	exempt := map[string]string{
		"handleMe":             "read-only",
		"handleRegister":       "creates the identity being authorised",
		"handleGetProposal":    "read-only (R8: knowing the pid is the credential)",
		"handleMyProposals":    "read-only, subject-scoped by the caller's own uid",
		"handleStream":         "read-only SSE",
		"handleCreateProposal": "no existing resource to authorise against; uses an inline registration gate",
		"handleCursor":         "ephemeral presence, not stored state; uses an inline registration gate",
	}

	var src []byte
	for _, f := range []string{"handlers.go", "realtime.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src = append(src, b...)
		src = append(src, '\n')
	}
	text := string(src)

	// Every top-level func, so a body ends where the next function begins.
	re := regexp.MustCompile(`(?m)^func (?:\(a \*api\) )?(\w+)\(`)
	locs := re.FindAllStringSubmatchIndex(text, -1)
	if len(locs) < 10 {
		// A regex that matched nothing would make this test pass on an empty
		// set — the absent/failure collapse it exists to catch.
		t.Fatalf("only %d funcs found; the regex lost its grip on the source "+
			"and this test is measuring nothing", len(locs))
	}
	body := map[string]string{}
	var order []string
	for i, loc := range locs {
		name := text[loc[2]:loc[3]]
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body[name] = text[loc[1]:end]
		order = append(order, name)
	}

	// delegatesTo names the ONLY accepted forwarding targets, explicitly.
	//
	// ★ MEASURED 2026-08-05. The first version inferred delegation — "this body
	// calls some helper that contains auth.Authorize" — and that inference is
	// WRONG here, because viewProposal calls Authorize to compute the UI's
	// canEdit HINT. Every handler that renders a view therefore looked guarded,
	// and deleting the real authorisation check from handleDeleteOption left
	// this test green. A rendering call and a protecting call are not the same
	// act, and no amount of source scanning can tell them apart — so the list
	// is written by hand instead of derived.
	delegatesTo := map[string]string{
		"handleSetVote":    "vote",
		"handleDeleteVote": "vote",
	}
	guarded := func(name string) bool {
		b, ok := body[name]
		if !ok {
			return false
		}
		if strings.Contains(b, "auth.Authorize") {
			return true
		}
		if callee, ok := delegatesTo[name]; ok {
			return strings.Contains(body[callee], "auth.Authorize")
		}
		return false
	}

	var unguarded []string
	for _, name := range order {
		if !strings.HasPrefix(name, "handle") {
			continue
		}
		if guarded(name) {
			continue
		}
		if _, ok := exempt[name]; ok {
			continue
		}
		unguarded = append(unguarded, name)
	}
	sort.Strings(unguarded)
	if len(unguarded) > 0 {
		t.Fatalf("these handlers never reach auth.Authorize and are not in the "+
			"documented exemption list: %v — either add the call or add an entry "+
			"here WITH a reason", unguarded)
	}

	// Control half: the exemption list must not go stale. A name listed here
	// that no longer exists means the list is drifting away from the code, and
	// a drifting allow-list silently stops guarding.
	for name := range exempt {
		if _, ok := body[name]; !ok {
			t.Errorf("exemption %q names a handler that no longer exists", name)
		}
	}
}

// TestIncrementalRenderCoversEveryServerOwnedNode reads web/app.js and pins the
// claim that the INCREMENTAL render path updates every node the FULL render
// path fills from server data.
//
// ★ Why this cannot be a behavioural Go test: the gap lives entirely in the
// browser. The backend broadcast is correct, the frontend listener is attached
// (TestFrontendHandlesEveryEventType above proves it), applyDelta() really does
// re-fetch the fresh snapshot — and the screen still never changes, because the
// render function it calls does not touch that node. Every Go test stays green;
// so does every backend assertion; so does the source guard above.
//
// ★ MEASURED 2026-08-05, by a three-context browser rehearsal. A renamed a
// proposal: PATCH returned 200, the server-side title WAS the new one, an
// independent EventSource tap on B's stream RECEIVED `event: proposal_update`
// with the new title in the payload — and B's #d-title never changed until a
// reload. Root cause: renderDetailFull() wrote #d-title/#d-desc; patchDetail()
// wrote #d-meta, options and calendar, and nothing else.
//
// The general shape: "the event was RECEIVED" and "the screen was UPDATED" are
// two different facts that share one output — silence. The guard above pins the
// first. This one pins the second.
func TestIncrementalRenderCoversEveryServerOwnedNode(t *testing.T) {
	src, err := os.ReadFile("../../web/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	text := string(src)

	fnBody := func(name string) string {
		start := strings.Index(text, "function "+name+"(")
		if start < 0 {
			t.Fatalf("function %s not found in app.js — it was renamed or removed "+
				"and this guard can no longer see it", name)
		}
		rest := text[start:]
		// Body ends where the next top-level `function ` begins.
		if end := strings.Index(rest[1:], "\nfunction "); end >= 0 {
			return rest[:end+1]
		}
		return rest
	}

	full := fnBody("renderDetailFull")
	patch := fnBody("patchDetail")

	// Every $('id') the FULL path writes with server data.
	idRe := regexp.MustCompile(`\$\('([a-z0-9-]+)'\)`)
	collect := func(body string) map[string]bool {
		out := map[string]bool{}
		for _, m := range idRe.FindAllStringSubmatch(body, -1) {
			out[m[1]] = true
		}
		return out
	}
	fullIDs, patchIDs := collect(full), collect(patch)
	// A regex matching nothing would make this test pass on an empty set — the
	// same absent/failure collapse it exists to catch.
	//
	// ★ NAMED IDS, NOT A COUNT. This was `len(fullIDs) < 3`, and a bare count
	// cannot tell "the parse broke" from "the function legitimately writes
	// fewer nodes now" — it went red the moment gear rendering moved into its
	// own helper, reporting a parse failure that had not happened. Asserting the
	// nodes this function is KNOWN to own proves the parse found the right
	// thing, which a threshold never did.
	for _, id := range []string{"d-title", "d-desc"} {
		if !fullIDs[id] {
			t.Fatalf("renderDetailFull parse yielded %v, missing the known node %q — "+
				"the parse failed and this test is measuring nothing", fullIDs, id)
		}
	}

	// Nodes whose value does NOT come from the live proposal payload, so a stale
	// one is not a sync defect. Each needs a stated reason.
	// gear-wrap (owner-only 提案設定選單) is written by BOTH renderDetailFull and
	// patchDetail, so it needs no exemption — it passes the main check directly.
	// b-repid / b-del-proposal moved INSIDE gear-menu; their visibility is now
	// governed by gear-wrap.hidden, not per-button, so they are no longer written
	// by renderDetailFull at all.
	exempt := map[string]string{}

	var missing []string
	for id := range fullIDs {
		if patchIDs[id] {
			continue
		}
		if _, ok := exempt[id]; ok {
			continue
		}
		missing = append(missing, id)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("renderDetailFull writes %v but patchDetail does not — an incremental "+
			"SSE update will leave these nodes showing stale server data, silently and "+
			"with no error anywhere (this is exactly how the 2026-08-05 rename defect "+
			"escaped). Either patch the node too, or add it to the exemption list WITH "+
			"a reason", missing)
	}

	// Control half: an exemption naming a node the full path no longer writes
	// means the list is drifting, and a drifting allow-list stops guarding.
	for id := range exempt {
		if !fullIDs[id] {
			t.Errorf("exemption %q names a node renderDetailFull no longer writes", id)
		}
	}
}

// TestCalendarPageGuardsAgainstMissingDates reads app.js and asserts the
// standalone calendar page filters out options with absent/malformed dates
// BEFORE they reach range computation.
//
// ★ This is the exact regression the inline calendar suffered: buildCalendar
// uses marks.sort()[0] as the month origin, so a single option with an
// undefined startDate makes new Date('undefinedT00:00:00') = Invalid Date, the
// loop `cur < end` is false forever, and NOT ONE cell is drawn. assignLanes'
// localeCompare throws on undefined outright. Both let "has options" and
// "a date is broken" share the single output "entirely blank". A behavioural
// test cannot see this (it renders fine with clean data); only a source guard
// asserting the filter EXISTS closes it — the same absent/failure collapse this
// file exists to catch.
func TestCalendarPageGuardsAgainstMissingDates(t *testing.T) {
	src, err := os.ReadFile("../../web/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	text := string(src)

	// The filter must exist and must test BOTH endpoints for a real YYYY-MM-DD
	// shape, not merely truthiness — an empty string is falsy but '' <= '' is
	// true, so a truthiness-only guard would still admit a blank date.
	start := strings.Index(text, "const calValidOpts")
	if start < 0 {
		t.Fatal("calValidOpts filter not found in app.js — the calendar page has no " +
			"date guard, so a single option with a missing startDate/endDate will " +
			"blank the entire month grid (the inline-calendar regression), silently")
	}
	end := strings.Index(text[start:], ";")
	if end < 0 {
		t.Fatal("calValidOpts declaration is unterminated")
	}
	filter := text[start : start+end]
	for _, marker := range []string{
		"o.startDate", "o.endDate", `\d{4}-\d{2}-\d{2}`, "o.startDate <= o.endDate",
	} {
		if !strings.Contains(filter, marker) {
			t.Errorf("calValidOpts is missing %q — a partial date guard admits the very "+
				"malformed option that blanks the grid", marker)
		}
	}

	// The calendar page must be reachable AND separable: route parses /cal, the
	// view container exists, and the render+back wiring is present. If any is
	// gone the page silently degrades to the detail view (no error anywhere).
	for _, marker := range []string{
		`/^#\/p\/([^/]+)\/cal$/`, // route parses the calendar hash
		"renderCalendarPage",     // the page renderer exists
		"buildCalendarPage",      // and its builder
		"b-open-calendar",        // entry button from detail page
		"calpage-back",           // back button to detail page
	} {
		if !strings.Contains(text, marker) {
			t.Errorf("calendar page wiring is missing %q", marker)
		}
	}

	// ★ The calendar page must support the FULL three-state cycle, not just
	// yes↔clear. Measured complaint 2026-08-05: the page only wired vote(o, 1),
	// so a tap could reach "可以" or "未選" but NEVER "不行" — one of the three
	// stances was unreachable. cycleStance must route through vote(o, 0).
	// A behavioural test renders identically whether or not the 0 branch exists;
	// only reading the source for the branch closes it.
	cyc := text[strings.Index(text, "function cycleStance"):]
	if !strings.Contains(text, "function cycleStance") {
		t.Fatal("cycleStance not found — the calendar page cannot cycle through the " +
			"three stances; 不行 is unreachable by tapping a segment")
	}
	cyc = cyc[:strings.Index(cyc[1:], "\nfunction ")+1]
	if !strings.Contains(cyc, "vote(o, 0)") {
		t.Error("cycleStance never calls vote(o, 0) — the 不行 stance is unreachable " +
			"from the calendar page, which is exactly the reported defect")
	}
	if !strings.Contains(cyc, "vote(o, 1)") {
		t.Error("cycleStance never calls vote(o, 1) — the 可以 stance is unreachable")
	}

	// ★ The whole segment must be clickable, not a separate blank button. The
	// complaint was that tapping required hitting an empty button; the fix wires
	// onclick directly onto the colour segment (seg.onclick) inside monthBlockPage.
	mbp := text[strings.Index(text, "function monthBlockPage"):]
	mbp = mbp[:strings.Index(mbp[1:], "\nfunction ")+1]
	if !strings.Contains(mbp, "seg.onclick = () => cycleStance(o)") {
		t.Error("monthBlockPage does not wire cycleStance onto the segment itself — " +
			"the whole bar should be tappable, not a blank sub-button")
	}

	htmlSrc, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	for _, marker := range []string{
		`id="view-calendar"`, `id="calpage-scroll"`, `id="calpage-back"`, `id="calpage-empty"`,
	} {
		if !strings.Contains(string(htmlSrc), marker) {
			t.Errorf("calendar page HTML is missing %q", marker)
		}
	}
}

func TestShareInviteSupportsCopyAndManualFallback(t *testing.T) {
	appSrc, err := os.ReadFile("../../web/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	htmlSrc, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	for _, marker := range []string{
		"id=\"b-share\"", "id=\"m-share\"", "id=\"share-url\"", "id=\"b-share-copy\"",
	} {
		if !strings.Contains(string(htmlSrc), marker) {
			t.Errorf("share modal is missing %q", marker)
		}
	}
	for _, marker := range []string{
		"function shareURL()", "${location.origin}${base}/#/p/${pid}",
		"navigator.clipboard && navigator.clipboard.writeText", "已選取連結，請按 Ctrl+C／⌘C 複製",
	} {
		if !strings.Contains(string(appSrc), marker) {
			t.Errorf("share flow is missing %q", marker)
		}
	}
}

func TestNotVotedOnlyShowsRegisteredNames(t *testing.T) {
	appSrc, err := os.ReadFile("../../web/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	htmlSrc, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}

	guard := func(app, html string) []string {
		var missing []string
		for _, marker := range []string{
			"const humanName = u => typeof u?.name === 'string' ? u.name.trim() : '';",
			".map(humanName).filter(Boolean)",
			"$('not-voted-box').hidden = names.length === 0",
			"$('not-voted').textContent = names.join('、')",
		} {
			if !strings.Contains(app, marker) {
				missing = append(missing, marker)
			}
		}
		if !strings.Contains(html, "id=\"not-voted-box\"") {
			missing = append(missing, "id=\"not-voted-box\"")
		}
		return missing
	}

	if missing := guard(string(appSrc), string(htmlSrc)); len(missing) > 0 {
		t.Fatalf("not-voted UI permits unnamed tokens or lacks its empty-state hide: %v", missing)
	}


	// Control: removing the non-empty-name filter must make this guard fail.
	mutant := strings.Replace(string(appSrc), ".map(humanName).filter(Boolean)", ".map(humanName)", 1)
	if missing := guard(mutant, string(htmlSrc)); len(missing) == 0 {
		t.Fatal("not-voted guard accepted a mutant without the non-empty-name filter")
	}
}

// TestNoDuplicateElementIDs asserts every id="..." in index.html is unique.
//
// Why: getElementById returns only the FIRST match, so a duplicated id lets a
// second, stale element sit in the DOM rendering silently — JS binds the first,
// the user sees the second. "Element correctly replaced" and "a leftover copy is
// still on screen" share one output (both parse, both render), so no behavioural
// test fires. Measured 2026-08-06: an R67 edit left an entire duplicate
// detail-head-meta block (b-share/b-repid/b-del-proposal/d-meta/presence all
// twice); every Go test stayed green and a superseded text button shipped to
// live for a full round. This guard makes that structural mistake loud.
func TestNoDuplicateElementIDs(t *testing.T) {
	htmlSrc, err := os.ReadFile("../../web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}

	idRe := regexp.MustCompile(`id="([^"]+)"`)
	dup := func(html string) []string {
		seen := map[string]int{}
		for _, m := range idRe.FindAllStringSubmatch(html, -1) {
			seen[m[1]]++
		}
		var dups []string
		for id, n := range seen {
			if n > 1 {
				dups = append(dups, id)
			}
		}
		sort.Strings(dups)
		return dups
	}

	if dups := dup(string(htmlSrc)); len(dups) > 0 {
		t.Fatalf("index.html has duplicate element ids (getElementById binds only the first, the rest render as stale ghosts): %v", dups)
	}

	// Control: injecting one duplicate id must make this guard fail.
	mutant := strings.Replace(string(htmlSrc), `id="d-title"`, `id="d-title"></h1><h1 id="d-title"`, 1)
	if dups := dup(mutant); len(dups) == 0 {
		t.Fatal("duplicate-id guard accepted a mutant with a doubled id")
	}
}
