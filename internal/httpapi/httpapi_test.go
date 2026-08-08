package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	teamdating "teamdating"
	"teamdating/internal/store"
)

// teamdatingWeb reaches the real embedded assets, not a test double.
//
// ★ Using the production embed is the whole point: a fake FS here would
// compare a stand-in against disk and pass while the shipped binary carried
// stale bytes.
func teamdatingWeb() (fs.FS, error) { return teamdating.Web() }

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type client struct {
	t     *testing.T
	srv   *httptest.Server
	jar   []*http.Cookie
	UID   string
	Name  string
	Recov string
}

type env struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
	dir string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv := httptest.NewServer(New(st, emptyFS{}, "test", "", ""))
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, st: st, dir: dir}
}

// emptyFS stands in for the embedded assets; these tests exercise /api only.
type emptyFS struct{}

func (emptyFS) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

// newClient registers a participant and keeps its cookie, i.e. a browser.
func (e *env) newClient(name string) *client {
	e.t.Helper()
	c := &client{t: e.t, srv: e.srv}
	var out meResp
	code := c.do("POST", "/api/register", map[string]any{"name": name}, &out)
	if code != http.StatusOK {
		e.t.Fatalf("register %s: status %d", name, code)
	}
	c.UID, c.Name, c.Recov = out.UID, out.Name, out.Recovery
	return c
}

// anonClient is a visitor with no identity at all.
func (e *env) anonClient() *client { return &client{t: e.t, srv: e.srv} }

// do issues a request carrying this client's cookies and returns the status.
func (c *client) do(method, path string, body any, out any) int {
	c.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.srv.URL+path, rdr)
	if err != nil {
		c.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for _, ck := range c.jar {
		req.AddCookie(ck)
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if cs := resp.Cookies(); len(cs) > 0 {
		c.jar = cs
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			c.t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

// errCode extracts the machine-readable code from a failure response.
func (c *client) errCode(method, path string, body any) (int, string) {
	c.t.Helper()
	var eb errorBody
	code := c.do(method, path, body, &eb)
	return code, eb.Code
}

// ensureEmail gives this client an address so it can pass the DD-7 proposal
// gate.
//
// ★ WHY THE HELPER AND NOT A FIXTURE CHANGE. Creating a proposal now requires
// the owner to hold an email (DD-7), so every pre-plan test that merely wanted
// "somebody with a proposal" would otherwise be asserting against a 403 it
// never meant to test. Setting the address here keeps those tests aimed at
// what they were written to check.
//
// ★ IT IS DELIBERATELY NOT CALLED FROM newClient. An identity that HOLDS an
// email takes a different branch of the same-name split (it answers
// `emailRequired` instead of returning the candidate list), so giving every
// test client an address would silently retarget the candidate tests onto a
// branch they are not about. Tests that need an email-less owner therefore use
// legacyProposal below.
// ★ IT MUST NOT OVERWRITE AN ADDRESS THE TEST ALREADY CHOSE. The merge split
// keys on email EQUALITY, so a fixture that clobbered a deliberately-set
// address would silently retarget those tests onto the "different person"
// branch — which is what an unconditional version of this helper did, turning
// the TV-2 merge vector red for a reason that had nothing to do with merging.
func (c *client) ensureEmail() {
	c.t.Helper()
	var cur meResp
	if code := c.do("GET", "/api/me", nil, &cur); code != http.StatusOK {
		c.t.Fatalf("fixture: read identity of %s: status %d", c.Name, code)
	}
	if strings.TrimSpace(cur.Email) != "" {
		return
	}
	var out meResp
	addr := "u" + strings.ToLower(c.UID) + "@fixture.example"
	if code := c.do("POST", "/api/identity",
		map[string]any{"email": addr}, &out); code != http.StatusOK {
		c.t.Fatalf("fixture: set email for %s: status %d", c.Name, code)
	}
	if out.Email != addr {
		c.t.Fatalf("fixture: identity reports email %q, want %q", out.Email, addr)
	}
}

// legacyProposal seeds a proposal for an owner who deliberately holds NO email.
//
// ★ IT WRITES THROUGH THE STORE ON PURPOSE, and that is the honest way to build
// this fixture rather than a way around the gate. The DD-7 gate correctly
// refuses an email-less owner, so a pre-plan identity holding a proposal cannot
// be produced through the API at all — and giving the owner an address to get
// past it would destroy the very condition these tests are about: an identity
// WITH an email takes the emailRequired branch of the same-name split, so the
// candidate list under test would never be returned.
func (e *env) legacyProposal(c *client, pid, title string) proposalView {
	e.t.Helper()
	if err := e.st.CreateProposal(pid, c.UID, title, ""); err != nil {
		e.t.Fatalf("seed legacy proposal %s: %v", pid, err)
	}
	return proposalView{PID: pid, OwnerUID: c.UID, Title: title}
}

func (c *client) createProposal(title string) proposalView {
	c.t.Helper()
	c.ensureEmail()
	var pv proposalView
	if code := c.do("POST", "/api/proposals",
		map[string]any{"title": title, "desc": ""}, &pv); code != http.StatusOK {
		c.t.Fatalf("create proposal: status %d", code)
	}
	return pv
}

func (c *client) addOption(pid, start, end string) proposalView {
	c.t.Helper()
	var pv proposalView
	if code := c.do("POST", "/api/proposals/"+pid+"/options",
		map[string]any{"startDate": start, "endDate": end, "note": ""}, &pv); code != http.StatusOK {
		c.t.Fatalf("add option: status %d", code)
	}
	return pv
}

func findOption(pv proposalView, start string) (optionView, bool) {
	for _, o := range pv.Options {
		if o.StartDate == start {
			return o, true
		}
	}
	return optionView{}, false
}

// ---------------------------------------------------------------------------
// V3 — no enumeration (R8, DD-20)
// ---------------------------------------------------------------------------

// TestMyProposalsIsSubjectScoped is V3 with BOTH arms.
//
// ★ The control arm is what gives this test meaning. Asserting only "A cannot
// see B's proposal" would also pass for an implementation that always returns
// an empty array — the failing case and the correct case would share one
// output. The second arm proves the endpoint returns something when it should.
func TestMyProposalsIsSubjectScoped(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	b := e.newClient("Bob")

	pa := a.createProposal("Alice trip")
	pb := b.createProposal("Bob trip")

	type listResp struct {
		Proposals []proposalView `json:"proposals"`
	}

	var listA listResp
	if code := a.do("GET", "/api/my/proposals", nil, &listA); code != http.StatusOK {
		t.Fatalf("A list: status %d", code)
	}
	if !containsPID(listA.Proposals, pa.PID) {
		t.Errorf("A's own proposal %s missing from A's list — the endpoint "+
			"returns nothing, so the exclusion below proves nothing", pa.PID)
	}
	if containsPID(listA.Proposals, pb.PID) {
		t.Errorf("A can see B's proposal %s — subject scoping is broken", pb.PID)
	}

	// Control arm: B must see PB. Without this, an always-empty implementation
	// passes every assertion above.
	var listB listResp
	if code := b.do("GET", "/api/my/proposals", nil, &listB); code != http.StatusOK {
		t.Fatalf("B list: status %d", code)
	}
	if !containsPID(listB.Proposals, pb.PID) {
		t.Errorf("control group failed: B cannot see B's own proposal %s", pb.PID)
	}
	if containsPID(listB.Proposals, pa.PID) {
		t.Errorf("B can see A's proposal %s", pa.PID)
	}

	// Knowing the pid still grants entry — that IS the access model (R8).
	var direct proposalView
	if code := a.do("GET", "/api/proposals/"+pb.PID, nil, &direct); code != http.StatusOK {
		t.Errorf("direct fetch by pid returned %d; R8 grants entry to whoever "+
			"holds the URL", code)
	}
}

func containsPID(list []proposalView, pid string) bool {
	for _, p := range list {
		if p.PID == pid {
			return true
		}
	}
	return false
}

// TestVotingPutsProposalInMyList covers the other half of DD-20: participation,
// not just ownership, makes a proposal "mine".
func TestVotingPutsProposalInMyList(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	b := e.newClient("Bob")

	pv := a.createProposal("Trip")
	pv = a.addOption(pv.PID, "2026-09-01", "2026-09-03")
	o := pv.Options[0]

	type listResp struct {
		Proposals []proposalView `json:"proposals"`
	}
	var before listResp
	b.do("GET", "/api/my/proposals", nil, &before)
	if containsPID(before.Proposals, pv.PID) {
		t.Fatal("control group failed: B sees the proposal before voting, so " +
			"seeing it afterwards would prove nothing")
	}

	if code := b.do("POST", "/api/proposals/"+pv.PID+"/votes",
		map[string]any{"optionId": o.OID}, nil); code != http.StatusOK {
		t.Fatalf("B vote: status %d", code)
	}

	var after listResp
	b.do("GET", "/api/my/proposals", nil, &after)
	if !containsPID(after.Proposals, pv.PID) {
		t.Error("after voting, the proposal is still absent from B's list")
	}
}

// TestNoEnumerationRoute asserts the ABSENCE of a public listing route.
//
// ★ R8 is structural: it holds because no such route exists, not because a
// route filters. So the assertion is about routing, and the control group is
// mandatory — a 404 from a broken server would otherwise look like proof.
func TestNoEnumerationRoute(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	a.createProposal("Trip")

	// Control group: a route that DOES exist must answer, proving the server
	// is up and that 404 below means "no such route" rather than "server dead".
	if code := a.do("GET", "/api/my/proposals", nil, nil); code != http.StatusOK {
		t.Fatalf("control group failed: /api/my/proposals returned %d, so a "+
			"404 elsewhere would not distinguish absence from breakage", code)
	}

	for _, path := range []string{"/api/proposals", "/api/proposals/"} {
		code := a.do("GET", path, nil, nil)
		if code == http.StatusOK {
			t.Errorf("GET %s returned 200 — a public enumeration route exists, "+
				"which defeats R8 structurally", path)
		}
	}
}

// TestAdminEnumerationIsAbsentFromPublicMux is the regression guard for
// BR_20260805.
//
// ★ This test previously asserted the OPPOSITE: that a loopback request to
// /api/admin/proposals on the public mux returns 200. It passed for months and
// protected nothing, because httptest dials the handler directly — so
// RemoteAddr was a real peer address. In production a same-host reverse proxy
// forwards everything from 127.0.0.1, which made requireLoopback true for the
// entire internet and served the roster to the LAN.
//
// The lesson is about what a test can see: the guard's logic was correct and
// fully covered, while its PREMISE ("the peer address identifies the caller")
// was false in the only environment that mattered. No amount of testing the
// predicate could reveal that. So the assertion now pins the structural fact
// instead — the route is not registered here at all, which no proxy can undo.
func TestAdminEnumerationIsAbsentFromPublicMux(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	a.createProposal("Trip")

	for _, path := range []string{"/api/admin/proposals", "/api/admin/users"} {
		// A genuine loopback caller — the strongest case there is — must still
		// get 404. Anything else means the route is reachable, and a proxy in
		// front would make "reachable from loopback" mean "reachable from
		// anywhere".
		if code := a.do("GET", path, nil, nil); code != http.StatusNotFound {
			t.Errorf("%s is registered on the public mux (got %d, want 404); "+
				"behind a same-host proxy that is an open enumeration route", path, code)
		}
	}

	// Control group: the mux is alive and this client works. Without it, a
	// handler that 404'd everything would pass the assertions above.
	if code := a.do("GET", "/api/me", nil, nil); code != http.StatusOK {
		t.Fatalf("control group failed: /api/me got %d, so the 404s above prove nothing", code)
	}
}

// TestAdminHandlerStillServesTheEnumeration proves the fix removed the ROUTE
// from the public mux without removing the CAPABILITY — otherwise "admin is
// secure" and "admin is broken" would look identical from the tests above.
func TestAdminHandlerStillServesTheEnumeration(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	admin := NewAdmin(st)

	for _, path := range []string{"/api/admin/proposals", "/api/admin/users"} {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "127.0.0.1:54321"
		rec := httptest.NewRecorder()
		admin.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s on the admin listener got %d, want 200 — the capability "+
				"was removed rather than relocated", path, rec.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// V6 — admin route rejects forged loopback (3.7, DD-3)
// ---------------------------------------------------------------------------

// TestAdminRejectsForgedForwardedFor is V6.
//
// ★ The discriminating case is the forged one. Testing only from localhost
// passes for an implementation with no check at all — tasks.md says exactly
// this: "只從 localhost 測會全過".
func TestAdminRejectsForgedForwardedFor(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	// Against the ADMIN handler now: the public mux no longer carries these
	// routes at all (BR_20260805). requireLoopback survives there as defence in
	// depth for an operator who binds the admin listener wider than loopback —
	// it is no longer the thing standing between the roster and the internet.
	h := NewAdmin(st)

	// Control group: a genuine loopback peer must be ALLOWED. Without it, a
	// handler that refused everything would pass the refusal assertions below.
	local := httptest.NewRequest("GET", "/api/admin/proposals", nil)
	local.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, local)
	if rec.Code != http.StatusOK {
		t.Fatalf("control group failed: genuine loopback got %d, so refusals "+
			"below would not prove the guard works", rec.Code)
	}

	// The forgery attempts. Each claims locality in a header while the actual
	// peer address is remote.
	forgeries := []struct {
		name    string
		remote  string
		headers map[string]string
	}{
		{"X-Forwarded-For", "203.0.113.7:1234",
			map[string]string{"X-Forwarded-For": "127.0.0.1"}},
		{"X-Forwarded-For chain", "203.0.113.7:1234",
			map[string]string{"X-Forwarded-For": "127.0.0.1, 10.0.0.1"}},
		{"X-Real-IP", "203.0.113.7:1234",
			map[string]string{"X-Real-IP": "127.0.0.1"}},
		{"Forwarded", "203.0.113.7:1234",
			map[string]string{"Forwarded": "for=127.0.0.1"}},
		{"no header, remote peer", "203.0.113.7:1234", nil},
		{"IPv6 remote", "[2001:db8::1]:443",
			map[string]string{"X-Forwarded-For": "::1"}},
		{"private LAN peer", "192.168.1.50:9999",
			map[string]string{"X-Forwarded-For": "127.0.0.1"}},
	}
	for _, f := range forgeries {
		t.Run(f.name, func(t *testing.T) {
			for _, path := range []string{"/api/admin/proposals", "/api/admin/users"} {
				req := httptest.NewRequest("GET", path, nil)
				req.RemoteAddr = f.remote
				for k, v := range f.headers {
					req.Header.Set(k, v)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Errorf("%s: got %d, want 403 — a spoofed header reached "+
						"the admin route", path, rec.Code)
				}
				if !strings.Contains(rec.Body.String(), "ADMIN_NOT_LOOPBACK") {
					t.Errorf("%s: refusal carries no ADMIN_NOT_LOOPBACK code: %s",
						path, rec.Body.String())
				}
			}
		})
	}

	// IPv6 loopback must be accepted: refusing ::1 would break the admin route
	// on a dual-stack host in a way that looks like a permissions bug.
	v6 := httptest.NewRequest("GET", "/api/admin/proposals", nil)
	v6.RemoteAddr = "[::1]:54321"
	rec6 := httptest.NewRecorder()
	h.ServeHTTP(rec6, v6)
	if rec6.Code != http.StatusOK {
		t.Errorf("IPv6 loopback ::1 was refused with %d", rec6.Code)
	}
}

// TestNoForwardedHeaderIsRead is a wiring guard on the source.
//
// ★ Behavioural tests can only probe the headers they think to send. This
// asserts the code never consults a forwarded-for header at all, which covers
// the variants nobody enumerated.
func TestNoForwardedHeaderIsRead(t *testing.T) {
	src, err := os.ReadFile("identity.go")
	if err != nil {
		t.Fatalf("read identity.go: %v", err)
	}
	code := stripGoComments(string(src))

	// Control group: prove the matcher fires on planted source.
	planted := `host := r.Header.Get("X-Forwarded-For")`
	if !strings.Contains(planted, "X-Forwarded-For") {
		t.Fatal("control group failed: the matcher cannot see a planted header read")
	}

	for _, h := range []string{"X-Forwarded-For", "X-Real-IP", "Forwarded"} {
		if strings.Contains(code, h) {
			t.Errorf("identity.go reads %s; DD-3 requires loopback to be "+
				"determined from RemoteAddr alone (risk C)", h)
		}
	}
	if !strings.Contains(code, "RemoteAddr") {
		t.Error("identity.go never reads RemoteAddr — the loopback check has " +
			"no basis at all")
	}
}

func stripGoComments(src string) string {
	var out strings.Builder
	i := 0
	for i < len(src) {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = len(src)
			} else {
				i += 2 + end + 2
			}
		default:
			out.WriteByte(src[i])
			i++
		}
	}
	return out.String()
}

// ---------------------------------------------------------------------------
// V2 — no proxy voting (TV-5) and the identity gate (3.3)
// ---------------------------------------------------------------------------

// TestNoProxyVoting is V2 with both arms.
func TestNoProxyVoting(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	b := e.newClient("Bob")

	pv := a.createProposal("Trip")
	pv = a.addOption(pv.PID, "2026-09-01", "2026-09-03")
	oid := pv.Options[0].OID

	// Control arm: A voting as A must succeed. If both arms returned 200 the
	// uid is not being taken from the token at all.
	if code := a.do("POST", "/api/proposals/"+pv.PID+"/votes",
		map[string]any{"optionId": oid}, nil); code != http.StatusOK {
		t.Fatalf("control group failed: A voting for itself got %d", code)
	}

	// The forgery attempt: a hand-built body naming B as the voter. The wire
	// format has no uid field, so DisallowUnknownFields rejects it — and even
	// if it were accepted, the server derives uid from the token.
	status, code := a.errCode("POST", "/api/proposals/"+pv.PID+"/votes",
		map[string]any{"optionId": oid, "uid": b.UID})
	if status == http.StatusOK {
		t.Error("a request naming another uid was accepted")
	}
	t.Logf("proxy-vote attempt: status=%d code=%s", status, code)

	// Whatever the rejection path, B must have no vote.
	var check proposalView
	a.do("GET", "/api/proposals/"+pv.PID, nil, &check)
	o, _ := findOption(check, "2026-09-01")
	for _, v := range o.Voters {
		if v.UID == b.UID {
			t.Error("B has a vote that B never cast")
		}
	}
	if o.VoteCount != 1 {
		t.Errorf("vote count = %d, want 1 (only A's own vote)", o.VoteCount)
	}
}

// TestIdentityGateBlocksUnregisteredWrites is 3.3 — the curl-past-the-frontend
// case, which tasks.md calls this item's only source of discriminating power.
func TestIdentityGateBlocksUnregisteredWrites(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("Owner")
	anon := e.anonClient()

	pv := owner.createProposal("Trip")
	pv = owner.addOption(pv.PID, "2026-09-01", "2026-09-03")
	oid := pv.Options[0].OID

	writes := []struct {
		name, method, path string
		body               any
	}{
		{"create proposal", "POST", "/api/proposals", map[string]any{"title": "X", "desc": ""}},
		{"vote", "POST", "/api/proposals/" + pv.PID + "/votes", map[string]any{"optionId": oid}},
		{"add option", "POST", "/api/proposals/" + pv.PID + "/options",
			map[string]any{"startDate": "2026-10-01", "endDate": "2026-10-02", "note": ""}},
		{"edit proposal", "PATCH", "/api/proposals/" + pv.PID, map[string]any{"title": "Y", "desc": ""}},
		{"delete proposal", "DELETE", "/api/proposals/" + pv.PID, nil},
		{"delete option", "DELETE", "/api/proposals/" + pv.PID + "/options/" + oid, nil},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			status, code := anon.errCode(w.method, w.path, w.body)
			if status != http.StatusForbidden {
				t.Errorf("got %d, want 403 — an unregistered visitor performed a write", status)
			}
			if code == "" {
				t.Error("refusal carries no machine-readable code (errors.md rule 1)")
			}
		})
	}

	// Control arm: reading is allowed without an identity. If reads were also
	// refused, the refusals above would prove nothing about the identity gate
	// — they would just mean the server rejects anonymous traffic wholesale.
	if code := anon.do("GET", "/api/proposals/"+pv.PID, nil, nil); code != http.StatusOK {
		t.Errorf("control group failed: anonymous read got %d, want 200 — R8 "+
			"grants entry to whoever holds the URL", code)
	}
}

// ---------------------------------------------------------------------------
// R5 lock semantics over HTTP (3.6, TV-2/TV-3/TV-4)
// ---------------------------------------------------------------------------

func TestOptionLockOverHTTP(t *testing.T) {
	e := newEnv(t)
	proposer := e.newClient("Proposer")
	author := e.newClient("Author")
	third := e.newClient("Third")

	pv := proposer.createProposal("Trip")
	pv = author.addOption(pv.PID, "2026-09-01", "2026-09-03")
	o, ok := findOption(pv, "2026-09-01")
	if !ok {
		t.Fatal("option missing after creation")
	}
	edit := map[string]any{"startDate": "2026-09-02", "endDate": "2026-09-04", "note": ""}
	path := "/api/proposals/" + pv.PID + "/options/" + o.OID

	// TV-2: the author's own vote must not lock them out.
	if code := author.do("POST", "/api/proposals/"+pv.PID+"/votes",
		map[string]any{"optionId": o.OID}, nil); code != http.StatusOK {
		t.Fatalf("author self-vote: status %d", code)
	}
	if code := author.do("PATCH", path, edit, nil); code != http.StatusOK {
		t.Errorf("TV-2: author was locked out by their OWN vote (status %d) — "+
			"OthersVoteCount must exclude the author", code)
	}

	// TV-3: the first other-party vote locks the author out.
	if code := third.do("POST", "/api/proposals/"+pv.PID+"/votes",
		map[string]any{"optionId": o.OID}, nil); code != http.StatusOK {
		t.Fatalf("third-party vote: status %d", code)
	}
	edit2 := map[string]any{"startDate": "2026-09-05", "endDate": "2026-09-06", "note": ""}
	status, code := author.errCode("PATCH", path, edit2)
	if status != http.StatusForbidden {
		t.Errorf("TV-3: author edit after another vote got %d, want 403", status)
	}
	if code != "OPTION_LOCKED" {
		t.Errorf("TV-3: code = %q, want OPTION_LOCKED", code)
	}
	if status, _ := author.errCode("DELETE", path, nil); status != http.StatusForbidden {
		t.Errorf("TV-3: author delete after another vote got %d, want 403", status)
	}

	// TV-4: the proposer is never locked out, and branch 1 must be evaluated
	// before the author lock.
	if code := proposer.do("DELETE", path, nil, nil); code != http.StatusOK {
		t.Errorf("TV-4: proposer delete got %d, want 200 — branch 1 must "+
			"precede the author lock", code)
	}
}

// ---------------------------------------------------------------------------
// TV-1 — idempotent voting (DD-8)
// ---------------------------------------------------------------------------

func TestRepeatedVoteIsIdempotent(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	pv := a.createProposal("Trip")
	pv = a.addOption(pv.PID, "2026-09-01", "2026-09-03")
	oid := pv.Options[0].OID

	for i := 0; i < 5; i++ {
		if code := a.do("POST", "/api/proposals/"+pv.PID+"/votes",
			map[string]any{"optionId": oid}, nil); code != http.StatusOK {
			t.Fatalf("vote %d: status %d", i, code)
		}
	}
	var check proposalView
	a.do("GET", "/api/proposals/"+pv.PID, nil, &check)
	o, _ := findOption(check, "2026-09-01")
	if o.VoteCount != 1 {
		t.Errorf("after 5 posts vote count = %d, want 1 (DD-8 composite key)", o.VoteCount)
	}
}

// ---------------------------------------------------------------------------
// V8 — cascade delete survives a restart
// ---------------------------------------------------------------------------

// TestCascadeDeleteSurvivesReplay is V8 with its control group.
func TestCascadeDeleteSurvivesReplay(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	srv := httptest.NewServer(New(st, emptyFS{}, "test", "", ""))

	e := &env{t: t, srv: srv, st: st, dir: dir}
	a := e.newClient("Alice")
	b := e.newClient("Bob")
	pv := a.createProposal("Trip")
	pv = a.addOption(pv.PID, "2026-09-01", "2026-09-03")
	oid := pv.Options[0].OID
	if code := b.do("POST", "/api/proposals/"+pv.PID+"/votes",
		map[string]any{"optionId": oid}, nil); code != http.StatusOK {
		t.Fatalf("vote: status %d", code)
	}
	pid := pv.PID

	// Control group: restart BEFORE deleting and confirm the records are
	// there. Without this, their absence after the delete could just mean
	// replay never restored them in the first place.
	srv.Close()
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	st2, err := store.New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	p2, ok := st2.Proposal(pid)
	if !ok {
		t.Fatal("control group failed: the proposal did not survive replay, so " +
			"its absence after deletion would prove nothing")
	}
	if len(p2.Options) != 1 || len(p2.Votes) != 1 {
		t.Fatalf("control group failed: after replay options=%d votes=%d, want 1/1",
			len(p2.Options), len(p2.Votes))
	}

	srv2 := httptest.NewServer(New(st2, emptyFS{}, "test", "", ""))
	a.srv, b.srv = srv2, srv2
	if code := a.do("DELETE", "/api/proposals/"+pid, nil, nil); code != http.StatusOK {
		t.Fatalf("delete proposal: status %d", code)
	}
	srv2.Close()
	if err := st2.Close(); err != nil {
		t.Fatalf("close 2: %v", err)
	}

	// The real assertion: after replaying a log that contains create + vote +
	// delete, nothing beneath the pid remains.
	st3, err := store.New(dir)
	if err != nil {
		t.Fatalf("reopen 2: %v", err)
	}
	defer st3.Close()
	if _, ok := st3.Proposal(pid); ok {
		t.Error("the deleted proposal came back after replay")
	}
	if n := st3.VoteCount(pid, oid); n != 0 {
		t.Errorf("%d votes survived the cascade delete across a restart", n)
	}
	if _, ok := st3.Option(pid, oid); ok {
		t.Error("an option survived the cascade delete across a restart")
	}
}

// ---------------------------------------------------------------------------
// DD-10 — names are joined, never copied
// ---------------------------------------------------------------------------

// TestRenamePropagatesToExistingRecords is 3.2's discriminating case: if the
// name had been copied into the option or vote at write time, the old value
// would still be showing here.
func TestRenamePropagatesToExistingRecords(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("OldName")
	b := e.newClient("Bob")

	pv := a.createProposal("Trip")
	pv = a.addOption(pv.PID, "2026-09-01", "2026-09-03")
	oid := pv.Options[0].OID
	b.do("POST", "/api/proposals/"+pv.PID+"/votes", map[string]any{"optionId": oid}, nil)
	a.do("POST", "/api/proposals/"+pv.PID+"/votes", map[string]any{"optionId": oid}, nil)

	var before proposalView
	a.do("GET", "/api/proposals/"+pv.PID, nil, &before)
	if before.OwnerName != "OldName" {
		t.Fatalf("control group failed: owner name is %q before the rename",
			before.OwnerName)
	}

	var renamed meResp
	if code := a.do("POST", "/api/register", map[string]any{"name": "NewName"}, &renamed); code != http.StatusOK {
		t.Fatalf("rename: status %d", code)
	}
	if renamed.UID != a.UID {
		t.Fatalf("rename minted a NEW identity (%s -> %s); the caller's votes "+
			"would be orphaned", a.UID, renamed.UID)
	}

	var after proposalView
	a.do("GET", "/api/proposals/"+pv.PID, nil, &after)
	if after.OwnerName != "NewName" {
		t.Errorf("owner name = %q after rename, want NewName — the name was "+
			"copied into the record instead of joined (DD-10)", after.OwnerName)
	}
	o, _ := findOption(after, "2026-09-01")
	if o.AuthorName != "NewName" {
		t.Errorf("option author name = %q, want NewName", o.AuthorName)
	}
	for _, v := range o.Voters {
		if v.UID == a.UID && v.Name != "NewName" {
			t.Errorf("voter name = %q, want NewName", v.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// DD-2 — recovery code reattaches an identity
// ---------------------------------------------------------------------------

// TestRecoveryCodeReattachesIdentity is 3.4: a second browser pastes the code
// and finds its own earlier vote.
func TestRecoveryCodeReattachesIdentity(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	if a.Recov == "" {
		t.Fatal("registration returned no recovery code (DD-2)")
	}

	pv := a.createProposal("Trip")
	pv = a.addOption(pv.PID, "2026-09-01", "2026-09-03")
	oid := pv.Options[0].OID
	a.do("POST", "/api/proposals/"+pv.PID+"/votes", map[string]any{"optionId": oid}, nil)

	// A fresh browser: no cookies at all.
	fresh := e.anonClient()
	var me meResp
	if code := fresh.do("GET", "/api/me", nil, &me); code != http.StatusOK || me.Registered {
		t.Fatalf("control group failed: the fresh client already has an "+
			"identity (status=%d registered=%v)", code, me.Registered)
	}

	var back meResp
	if code := fresh.do("POST", "/api/register",
		map[string]any{"recovery": a.Recov}, &back); code != http.StatusOK {
		t.Fatalf("reattach: status %d", code)
	}
	if back.UID != a.UID {
		t.Fatalf("reattach produced uid %s, want %s", back.UID, a.UID)
	}

	// The point of reattaching: the earlier vote is now cancellable by this
	// browser, which is only true if it really is the same identity.
	if code := fresh.do("DELETE", "/api/proposals/"+pv.PID+"/votes/"+oid, nil, nil); code != http.StatusOK {
		t.Errorf("the reattached client could not cancel its own earlier vote: %d", code)
	}
	var check proposalView
	a.do("GET", "/api/proposals/"+pv.PID, nil, &check)
	o, _ := findOption(check, "2026-09-01")
	if o.VoteCount != 0 {
		t.Errorf("vote count = %d after cancellation, want 0", o.VoteCount)
	}
}

// TestRecoveryCodeRejectsBadInput keeps malformed and unknown distinguishable.
func TestRecoveryCodeRejectsBadInput(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	fresh := e.anonClient()

	status, code := fresh.errCode("POST", "/api/register", map[string]any{"recovery": "abc"})
	if status == http.StatusOK {
		t.Error("a malformed recovery code was accepted")
	}
	if code != "RECOVERY_MALFORMED" {
		t.Errorf("malformed code got %q, want RECOVERY_MALFORMED", code)
	}

	// A well-formed code that belongs to nobody must say so DIFFERENTLY —
	// otherwise a user who mistyped cannot tell which mistake they made.
	//
	// ★ The obvious way to build this input is to map 'A' -> 'B' across the whole
	// code. That is WRONG, and it fails in the most misleading direction: a
	// 52-character base32 token contains no 'A' at all about 10% of the time
	// (measured over 100k samples, not estimated), and in that case the mapping
	// is a no-op, `valid` IS Alice's own code, and the server correctly accepts
	// it -- whereupon this test reports "an unknown recovery code was accepted",
	// i.e. a security defect that does not exist.
	//
	// A mutation that did not apply and a mutation the assertion failed to catch
	// share one output, so the substitution has to be forced and then verified.
	original := strings.ReplaceAll(a.Recov, "-", "")
	if original == "" {
		t.Fatal("no recovery code to mutate")
	}
	swapped := byte('A')
	if original[0] == 'A' {
		swapped = 'B'
	}
	valid := string(swapped) + original[1:]
	if valid == original {
		t.Fatal("the mutation was a no-op: this input is Alice's own code, " +
			"so anything it proves about unknown codes is an artefact")
	}
	status2, code2 := fresh.errCode("POST", "/api/register", map[string]any{"recovery": valid})
	if status2 == http.StatusOK {
		t.Error("an unknown recovery code was accepted")
	}
	if code2 == "RECOVERY_MALFORMED" {
		t.Error("an unknown-but-well-formed code was reported as malformed; " +
			"the two cases must stay distinguishable")
	}
	t.Logf("unknown code: status=%d code=%s", status2, code2)
}

// ---------------------------------------------------------------------------
// Embedded assets — byte-for-byte comparison (carried debt 2)
// ---------------------------------------------------------------------------

// TestEmbeddedAssetsMatchDisk compares the embedded copy against web/ on disk.
//
// ★ /api/health reporting assets:"embedded" proves only which SOURCE was used,
// not that the bytes are current. A stale embed serves an old page while every
// existing check stays green.
func TestEmbeddedAssetsMatchDisk(t *testing.T) {
	embedded, err := teamdatingWeb()
	if err != nil {
		t.Fatalf("embedded assets: %v", err)
	}

	var checked int
	err = fs.WalkDir(embedded, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		want, err := os.ReadFile("../../web/" + path)
		if err != nil {
			return fmt.Errorf("read disk copy of %s: %w", path, err)
		}
		got, err := fs.ReadFile(embedded, path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: embedded copy differs from web/ on disk (%d vs %d bytes)",
				path, len(got), len(want))
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded assets: %v", err)
	}

	// Zero files walked would make every assertion above vacuous — absent and
	// passing must not share one output.
	if checked == 0 {
		t.Fatal("no embedded files were compared; the walk found nothing")
	}
	t.Logf("compared %d embedded files against disk", checked)

	// Control group: prove the comparison can actually fail. Flip one byte of
	// a real asset in memory and confirm bytes.Equal rejects it.
	sample, err := fs.ReadFile(embedded, "index.html")
	if err != nil {
		t.Fatalf("read index.html from embed: %v", err)
	}
	mutated := append([]byte(nil), sample...)
	mutated[0] ^= 0xFF
	if bytes.Equal(mutated, sample) {
		t.Fatal("control group failed: a one-byte change compared equal, so " +
			"the comparisons above prove nothing")
	}
}

// --- 6.7 決策凍結 e2e ------------------------------------------------------

// TestDecisionFreezesOverHTTP walks the freeze through the REAL serve path.
//
// ★ The auth-layer tests prove the RULE; this proves the rule is WIRED. Those
// are different claims: a handler that never calls Authorize would leave every
// auth test green while the endpoint stayed wide open (this is exactly the
// shape of the admin defect found on 2026-08-05).
func TestDecisionFreezesOverHTTP(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("Ann")
	other := e.newClient("Bob")

	var p proposalView
	owner.ensureEmail() // DD-7: the proposal gate refuses an email-less owner.
	if code := owner.do("POST", "/api/proposals",
		map[string]any{"title": "行程", "desc": ""}, &p); code != http.StatusOK {
		t.Fatalf("create proposal: %d", code)
	}
	var withOpt proposalView
	if code := owner.do("POST", "/api/proposals/"+p.PID+"/options",
		map[string]any{"startDate": "2026-03-01", "endDate": "2026-03-05"}, &withOpt); code != http.StatusOK {
		t.Fatalf("create option: %d", code)
	}
	if len(withOpt.Options) != 1 {
		t.Fatalf("want 1 option, got %d", len(withOpt.Options))
	}
	oid := withOpt.Options[0].OID

	// Control half FIRST: before any decision, voting must succeed. Without
	// this, a deny produced by some unrelated reason would read as "the freeze
	// works".
	if code := other.do("POST", "/api/proposals/"+p.PID+"/votes",
		map[string]any{"optionId": oid}, nil); code != http.StatusOK {
		t.Fatalf("vote before decision: %d, want 200", code)
	}

	// Freeze.
	var decided proposalView
	if code := owner.do("POST", "/api/proposals/"+p.PID+"/decision",
		map[string]any{"optionId": oid}, &decided); code != http.StatusOK {
		t.Fatalf("decide: %d", code)
	}
	if decided.DecidedOptionID != oid {
		t.Fatalf("view.decidedOptionId = %q, want %q", decided.DecidedOptionID, oid)
	}
	if len(decided.Options) != 1 || !decided.Options[0].Decided {
		t.Fatal("the decided option is not flagged in the view")
	}

	// Every write is now refused — over HTTP, with the real status code.
	frozen := []struct {
		name, method, path string
		body               any
	}{
		{"vote", "POST", "/api/proposals/" + p.PID + "/votes", map[string]any{"optionId": oid}},
		{"unvote", "DELETE", "/api/proposals/" + p.PID + "/votes/" + oid, nil},
		{"add option", "POST", "/api/proposals/" + p.PID + "/options",
			map[string]any{"startDate": "2026-04-01", "endDate": "2026-04-02"}},
		{"edit option", "PATCH", "/api/proposals/" + p.PID + "/options/" + oid,
			map[string]any{"startDate": "2026-05-01", "endDate": "2026-05-02"}},
		{"delete option", "DELETE", "/api/proposals/" + p.PID + "/options/" + oid, nil},
	}
	for _, tc := range frozen {
		if code := other.do(tc.method, tc.path, tc.body, nil); code != http.StatusForbidden {
			t.Fatalf("%s on a decided proposal: %d, want 403", tc.name, code)
		}
	}
	// The OWNER is frozen too. A freeze only the non-owner feels is not a
	// freeze; it is another ownership rule wearing the wrong name.
	if code := owner.do("POST", "/api/proposals/"+p.PID+"/options",
		map[string]any{"startDate": "2026-06-01", "endDate": "2026-06-02"}, nil); code != http.StatusForbidden {
		t.Fatalf("owner added an option to a decided proposal: %d, want 403", code)
	}

	// Release, then the same write must succeed again — proving the freeze is
	// reversible and that the 403s above came FROM it.
	if code := owner.do("POST", "/api/proposals/"+p.PID+"/decision",
		map[string]any{"optionId": ""}, nil); code != http.StatusOK {
		t.Fatalf("release: %d", code)
	}
	if code := other.do("POST", "/api/proposals/"+p.PID+"/votes",
		map[string]any{"optionId": oid}, nil); code != http.StatusOK {
		t.Fatalf("vote after release: %d, want 200", code)
	}
}

// TestDecisionRequiresOwnerOverHTTP: a participant cannot freeze someone
// else's proposal by calling the endpoint directly (the UI hiding the button
// is convenience, never protection).
func TestDecisionRequiresOwnerOverHTTP(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("Ann")
	other := e.newClient("Bob")

	var p proposalView
	owner.ensureEmail() // DD-7: the proposal gate refuses an email-less owner.
	if code := owner.do("POST", "/api/proposals", map[string]any{"title": "行程"}, &p); code != http.StatusOK {
		t.Fatalf("create proposal: %d", code)
	}
	var withOpt proposalView
	owner.do("POST", "/api/proposals/"+p.PID+"/options",
		map[string]any{"startDate": "2026-03-01", "endDate": "2026-03-05"}, &withOpt)
	if len(withOpt.Options) != 1 {
		t.Fatalf("want 1 option, got %d", len(withOpt.Options))
	}
	oid := withOpt.Options[0].OID

	if code := other.do("POST", "/api/proposals/"+p.PID+"/decision",
		map[string]any{"optionId": oid}, nil); code != http.StatusForbidden {
		t.Fatalf("non-owner decided: %d, want 403", code)
	}
	// Control: the owner succeeds on the identical call.
	if code := owner.do("POST", "/api/proposals/"+p.PID+"/decision",
		map[string]any{"optionId": oid}, nil); code != http.StatusOK {
		t.Fatalf("owner refused: %d", code)
	}
}

// --- 6.8 三態表態 e2e ------------------------------------------------------

// TestStanceOmittedWeightMeansYes is the pointer's whole justification.
//
// ★ With a plain float64 in voteReq, a body carrying no "weight" key decodes to
// 0 — which is 不行. Every pre-6.8 client sends exactly that body. So the
// upgrade would have silently converted every existing 投這個 into a rejection,
// with no error and no visible symptom until someone read the tally.
func TestStanceOmittedWeightMeansYes(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("Ann")
	voter := e.newClient("Bob")

	var p proposalView
	owner.ensureEmail() // DD-7: the proposal gate refuses an email-less owner.
	if code := owner.do("POST", "/api/proposals", map[string]any{"title": "T"}, &p); code != http.StatusOK {
		t.Fatalf("create proposal: %d", code)
	}
	var wo proposalView
	owner.do("POST", "/api/proposals/"+p.PID+"/options",
		map[string]any{"startDate": "2026-03-01", "endDate": "2026-03-05"}, &wo)
	oid := wo.Options[0].OID

	// Legacy body: optionId only, no weight key at all.
	var after proposalView
	if code := voter.do("POST", "/api/proposals/"+p.PID+"/votes",
		map[string]any{"optionId": oid}, &after); code != http.StatusOK {
		t.Fatalf("legacy vote: %d", code)
	}
	o := after.Options[0]
	if o.Yes != 1 || o.No != 0 || o.Score != 1 {
		t.Fatalf("legacy vote tallied as yes=%d no=%d score=%v, want yes=1 no=0 score=1 — "+
			"an omitted weight was read as 不行", o.Yes, o.No, o.Score)
	}
	// Control: an EXPLICIT zero must land in No, proving the two are distinguished.
	zero := 0.0
	var afterZero proposalView
	if code := voter.do("POST", "/api/proposals/"+p.PID+"/votes",
		map[string]any{"optionId": oid, "weight": zero}, &afterZero); code != http.StatusOK {
		t.Fatalf("explicit-zero vote: %d", code)
	}
	z := afterZero.Options[0]
	if z.No != 1 || z.Yes != 0 {
		t.Fatalf("explicit 不行 tallied as yes=%d no=%d, want yes=0 no=1 — the handler "+
			"cannot tell an omitted weight from an explicit zero", z.Yes, z.No)
	}
}

// TestStanceThreeStateOverHTTP walks all three stances and the viewer's own
// myWeight through the real serve path.
func TestStanceThreeStateOverHTTP(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("Ann")
	b := e.newClient("Bob")
	c := e.newClient("Cid")

	var p proposalView
	owner.ensureEmail() // DD-7: the proposal gate refuses an email-less owner.
	if code := owner.do("POST", "/api/proposals", map[string]any{"title": "T"}, &p); code != http.StatusOK {
		t.Fatalf("create proposal: %d", code)
	}
	var wo proposalView
	owner.do("POST", "/api/proposals/"+p.PID+"/options",
		map[string]any{"startDate": "2026-03-01", "endDate": "2026-03-05"}, &wo)
	oid := wo.Options[0].OID

	zero := 0.0
	one := 1.0
	owner.do("POST", "/api/proposals/"+p.PID+"/votes", map[string]any{"optionId": oid, "weight": one}, nil)
	b.do("POST", "/api/proposals/"+p.PID+"/votes", map[string]any{"optionId": oid, "weight": one}, nil)
	var final proposalView
	c.do("POST", "/api/proposals/"+p.PID+"/votes", map[string]any{"optionId": oid, "weight": zero}, &final)

	o := final.Options[0]
	if o.Yes != 2 || o.No != 1 {
		t.Fatalf("tally yes=%d no=%d, want 2/1", o.Yes, o.No)
	}
	if o.Score != 2 {
		t.Fatalf("score = %v, want 2", o.Score)
	}
	// myWeight is the viewer's own stance. Cid voted 不行 (0), so it must be a
	// non-nil pointer to 0 — NOT nil, which would mean "has not responded".
	if o.MyWeight == nil {
		t.Fatal("myWeight is nil for a viewer who voted 不行 — rejection reads as silence")
	}
	if *o.MyWeight != 0 {
		t.Fatalf("myWeight = %v, want 0", *o.MyWeight)
	}
	// Control: a viewer who has NOT voted must get nil.
	d := e.newClient("Dee")
	var seen proposalView
	d.do("GET", "/api/proposals/"+p.PID, nil, &seen)
	if seen.Options[0].MyWeight != nil {
		t.Fatalf("myWeight = %v for a viewer who never voted, want nil",
			*seen.Options[0].MyWeight)
	}
}

// TestStanceRejectsForgedWeightOverHTTP: an out-of-set weight is refused at the
// API boundary, not clamped into the ranking.
func TestStanceRejectsForgedWeightOverHTTP(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("Ann")
	var p proposalView
	owner.ensureEmail() // DD-7: the proposal gate refuses an email-less owner.
	if code := owner.do("POST", "/api/proposals", map[string]any{"title": "T"}, &p); code != http.StatusOK {
		t.Fatalf("create proposal: %d", code)
	}
	var wo proposalView
	owner.do("POST", "/api/proposals/"+p.PID+"/options",
		map[string]any{"startDate": "2026-03-01", "endDate": "2026-03-05"}, &wo)
	if len(wo.Options) != 1 {
		t.Fatalf("want 1 option, got %d", len(wo.Options))
	}
	oid := wo.Options[0].OID

	forged := 9.0
	if code := owner.do("POST", "/api/proposals/"+p.PID+"/votes",
		map[string]any{"optionId": oid, "weight": forged}, nil); code != http.StatusBadRequest {
		t.Fatalf("forged weight 9.0 got %d, want 400", code)
	}
	// Control: a legal weight on the identical path succeeds. 0.5 was a legal
	// stance before 勉強 was removed; now it must be rejected too, so use 1.0.
	legal := 1.0
	if code := owner.do("POST", "/api/proposals/"+p.PID+"/votes",
		map[string]any{"optionId": oid, "weight": legal}, nil); code != http.StatusOK {
		t.Fatalf("legal weight 1.0 got %d, want 200", code)
	}
}

// --- 6.9 pid 重新產生 e2e ---------------------------------------------------

// TestRepidRevokesOldURLOverHTTP: after rotation the leaked URL must return a
// hard 404 over the real serve path — not a redirect, not a 200 with a hint.
func TestRepidRevokesOldURLOverHTTP(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("Ann")
	leaked := e.newClient("Eve") // holds the old link

	var p proposalView
	owner.ensureEmail() // DD-7: the proposal gate refuses an email-less owner.
	if code := owner.do("POST", "/api/proposals", map[string]any{"title": "秘密行程"}, &p); code != http.StatusOK {
		t.Fatalf("create proposal: %d", code)
	}
	var wo proposalView
	owner.do("POST", "/api/proposals/"+p.PID+"/options",
		map[string]any{"startDate": "2026-03-01", "endDate": "2026-03-05"}, &wo)
	oid := wo.Options[0].OID

	// Control FIRST: before rotation the leaked link works. Without this, a 404
	// caused by anything else would read as successful revocation.
	if code := leaked.do("GET", "/api/proposals/"+p.PID, nil, nil); code != http.StatusOK {
		t.Fatalf("leaked link before rotation: %d, want 200", code)
	}

	var rotated proposalView
	if code := owner.do("POST", "/api/proposals/"+p.PID+"/repid", nil, &rotated); code != http.StatusOK {
		t.Fatalf("repid: %d", code)
	}
	if rotated.PID == p.PID || rotated.PID == "" {
		t.Fatalf("repid returned pid %q (old was %q) — nothing rotated", rotated.PID, p.PID)
	}

	// The leaked link is dead.
	if code := leaked.do("GET", "/api/proposals/"+p.PID, nil, nil); code != http.StatusNotFound {
		t.Fatalf("leaked link after rotation: %d, want 404 — the revocation reported "+
			"success while the old URL still works", code)
	}
	// Writes through the old pid are dead too, not just reads.
	if code := leaked.do("POST", "/api/proposals/"+p.PID+"/votes",
		map[string]any{"optionId": oid}, nil); code == http.StatusOK {
		t.Fatal("a write through the revoked pid succeeded")
	}
	// The new link carries the full proposal.
	var seen proposalView
	if code := owner.do("GET", "/api/proposals/"+rotated.PID, nil, &seen); code != http.StatusOK {
		t.Fatalf("new link: %d", code)
	}
	if len(seen.Options) != 1 || seen.Options[0].OID != oid {
		t.Fatalf("contents did not travel: %d options", len(seen.Options))
	}
}

// TestRepidRequiresOwnerOverHTTP: a participant cannot rotate someone else's
// proposal (which would lock the real owner out of their own share link).
func TestRepidRequiresOwnerOverHTTP(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("Ann")
	other := e.newClient("Bob")

	var p proposalView
	owner.ensureEmail() // DD-7: the proposal gate refuses an email-less owner.
	if code := owner.do("POST", "/api/proposals", map[string]any{"title": "T"}, &p); code != http.StatusOK {
		t.Fatalf("create proposal: %d", code)
	}

	if code := other.do("POST", "/api/proposals/"+p.PID+"/repid", nil, nil); code != http.StatusForbidden {
		t.Fatalf("non-owner rotated: %d, want 403", code)
	}
	// Control: the owner succeeds on the identical call.
	if code := owner.do("POST", "/api/proposals/"+p.PID+"/repid", nil, nil); code != http.StatusOK {
		t.Fatalf("owner refused: %d", code)
	}
}

// TestRepidWorksWhileDecided: a decided proposal's URL can leak too. If the
// freeze gate swallowed repid, revocation would fail in exactly the state where
// the proposal matters most.
func TestRepidWorksWhileDecided(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("Ann")
	var p proposalView
	owner.ensureEmail() // DD-7: the proposal gate refuses an email-less owner.
	if code := owner.do("POST", "/api/proposals", map[string]any{"title": "T"}, &p); code != http.StatusOK {
		t.Fatalf("create proposal: %d", code)
	}
	var wo proposalView
	owner.do("POST", "/api/proposals/"+p.PID+"/options",
		map[string]any{"startDate": "2026-03-01", "endDate": "2026-03-05"}, &wo)
	if len(wo.Options) != 1 {
		t.Fatalf("want 1 option, got %d", len(wo.Options))
	}
	owner.do("POST", "/api/proposals/"+p.PID+"/decision",
		map[string]any{"optionId": wo.Options[0].OID}, nil)

	var rotated proposalView
	if code := owner.do("POST", "/api/proposals/"+p.PID+"/repid", nil, &rotated); code != http.StatusOK {
		t.Fatalf("repid on a decided proposal: %d, want 200 — the freeze gate ate the "+
			"revocation and the leaked URL cannot be revoked", code)
	}
	// The decision must survive the rotation.
	if rotated.DecidedOptionID != wo.Options[0].OID {
		t.Fatalf("decision lost in rotation: %q", rotated.DecidedOptionID)
	}
}

// --- 3.1 cookie 屬性（查證 2026-08-05 發現的缺口）-----------------------------

// TestSessionCookieAttributes pins HttpOnly and SameSite.
//
// ★ Found by an audit on 2026-08-05: both attributes were implemented and
// NOTHING asserted them. Flipping HttpOnly to false left the entire suite
// green — the token would have become readable by any script on the page, and
// the only signal would have been a security incident.
func TestSessionCookieAttributes(t *testing.T) {
	e := newEnv(t)
	c := &client{t: t, srv: e.srv}
	if code := c.do("POST", "/api/register", map[string]any{"name": "Ann"}, nil); code != http.StatusOK {
		t.Fatalf("register: %d", code)
	}
	if len(c.jar) == 0 {
		t.Fatal("register set no cookie at all")
	}
	var found bool
	for _, ck := range c.jar {
		if ck.Value == "" {
			continue
		}
		found = true
		if !ck.HttpOnly {
			t.Errorf("cookie %q is not HttpOnly — the bearer token is readable by "+
				"any script on the page", ck.Name)
		}
		if ck.SameSite != http.SameSiteLaxMode && ck.SameSite != http.SameSiteStrictMode {
			t.Errorf("cookie %q SameSite = %v, want Lax or Strict — without it the "+
				"token rides along on cross-site requests", ck.Name, ck.SameSite)
		}
		if ck.Path != "/" && ck.Path != "" {
			t.Errorf("cookie %q Path = %q", ck.Name, ck.Path)
		}
	}
	if !found {
		t.Fatal("no non-empty cookie found in the register response")
	}
}

// --- 4.1 PATCH 成功路徑（查證 2026-08-05 發現的缺口）-------------------------

// TestUpdateProposalOverHTTP covers the 200 branch of handleUpdateProposal,
// which had no HTTP-level test at all — only the 403 case was exercised.
func TestUpdateProposalOverHTTP(t *testing.T) {
	e := newEnv(t)
	owner := e.newClient("Ann")
	other := e.newClient("Bob")

	var p proposalView
	owner.ensureEmail() // DD-7: the proposal gate refuses an email-less owner.
	if code := owner.do("POST", "/api/proposals", map[string]any{"title": "舊標題", "desc": "舊說明"}, &p); code != http.StatusOK {
		t.Fatalf("create proposal: %d", code)
	}

	var updated proposalView
	if code := owner.do("PATCH", "/api/proposals/"+p.PID,
		map[string]any{"title": "新標題", "desc": "新說明"}, &updated); code != http.StatusOK {
		t.Fatalf("owner PATCH: %d, want 200", code)
	}
	if updated.Title != "新標題" || updated.Desc != "新說明" {
		t.Fatalf("response still shows title=%q desc=%q", updated.Title, updated.Desc)
	}
	// The change must be visible to ANOTHER client, i.e. it actually landed in
	// the store rather than only being echoed back.
	var seen proposalView
	other.do("GET", "/api/proposals/"+p.PID, nil, &seen)
	if seen.Title != "新標題" {
		t.Fatalf("another client still sees title=%q — the response was echoed, "+
			"not persisted", seen.Title)
	}
	// Control: a non-owner is refused, so the 200 above came from ownership.
	if code := other.do("PATCH", "/api/proposals/"+p.PID,
		map[string]any{"title": "駭入", "desc": ""}, nil); code != http.StatusForbidden {
		t.Fatalf("non-owner PATCH: %d, want 403", code)
	}
}

// ---------------------------------------------------------------------------
// Cookie scope (BR_20260805_cookie_path_leaks_token_to_sibling_apps)
// ---------------------------------------------------------------------------

// TestCookiePathIsScopedToBase is the assertion whose absence let the token
// leak to every neighbouring app for the whole of M1 and M2.
//
// ★ Why nothing else catches it: Path=/ is CORRECT on an origin this service
// owns alone, so every httptest-based test passes and its assertions all hold.
// The defect exists only in the deployment topology — several unrelated apps
// behind one path-multiplexing proxy origin — where the browser then attaches
// this service's bearer token to every sibling prefix. HttpOnly does not help:
// it stops a script from READING the cookie, not the browser from SENDING it.
func TestCookiePathIsScopedToBase(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{"", "/"},                        // direct run: nothing shares the origin
		{"/teamdating", "/teamdating/"},  // behind the gateway
		{"/teamdating/", "/teamdating/"}, // trailing slash must not double up
	}
	for _, c := range cases {
		if got := cookiePath(c.base); got != c.want {
			t.Errorf("cookiePath(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

// TestRegisterSetsScopedCookieAndClearsTheWideOne walks the real HTTP path,
// because cookiePath being right is worth nothing if the handler ignores it.
func TestRegisterSetsScopedCookieAndClearsTheWideOne(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	srv := httptest.NewServer(New(st, emptyFS{}, "test", "/teamdating", ""))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/register", "application/json",
		strings.NewReader(`{"name":"Scoped"}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()

	var scoped, cleared *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name != cookieName {
			continue
		}
		if c.Value == "" && c.MaxAge < 0 {
			cleared = c
		} else if c.Value != "" {
			scoped = c
		}
	}

	if scoped == nil {
		t.Fatal("register issued no token cookie at all")
	}
	if scoped.Path != "/teamdating/" {
		t.Errorf("token cookie Path = %q, want /teamdating/ — a wider path means "+
			"every app sharing this origin receives the bearer token", scoped.Path)
	}
	// ★ Without the clear, a Path=/ cookie issued before this fix keeps being
	// sent to the neighbours for its full 400-day life, while the fix looks
	// like it landed. The two are DIFFERENT cookies and coexist.
	if cleared == nil {
		t.Error("no expiry for the old Path=/ cookie: a token issued before " +
			"this fix keeps leaking to sibling apps until it expires")
	} else if cleared.Path != "/" {
		t.Errorf("the clearing cookie targets Path %q, but the leaky one was at /", cleared.Path)
	}
}

// TestDirectRunKeepsRootCookiePath: the fix must not break the no-proxy case,
// where "/" is the correct and only sensible scope.
func TestDirectRunKeepsRootCookiePath(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()
	srv := httptest.NewServer(New(st, emptyFS{}, "test", "", ""))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/register", "application/json",
		strings.NewReader(`{"name":"Direct"}`))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()

	for _, c := range resp.Cookies() {
		if c.Name == cookieName && c.Value != "" && c.Path != "/" {
			t.Errorf("direct run set Path=%q, want / — narrowing it here would "+
				"log the user out on every path outside the prefix", c.Path)
		}
		if c.Name == cookieName && c.Value == "" {
			t.Error("direct run emitted a clearing cookie: there is no wide " +
				"cookie to clear, and clearing one on every register is churn")
		}
	}
}
