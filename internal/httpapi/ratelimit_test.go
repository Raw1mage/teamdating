package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"teamdating/internal/store"
)

// ---------------------------------------------------------------------------
// 6.2 — rate limiting (risk F)
// ---------------------------------------------------------------------------

// fakeClock lets the bucket tests advance time without sleeping. Sleeping would
// make these tests slow AND flaky, and a flaky limiter test is worse than none:
// it trains the reader to re-run until green.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func TestBucketRefusesAfterBurstAndRefills(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	l := newLimiter(5, 3, clk.now) // 5/sec, burst 3

	// The burst is spendable.
	for i := 0; i < 3; i++ {
		if !l.allow("k") {
			t.Fatalf("request %d refused inside the burst of 3", i+1)
		}
	}
	// And then exhausted.
	if l.allow("k") {
		t.Fatal("the 4th request passed: the burst is not enforced at all")
	}

	// Refill: 5/sec means 200ms buys exactly one token.
	clk.add(200 * time.Millisecond)
	if !l.allow("k") {
		t.Fatal("no token after 200ms at 5/sec — the bucket never refills, " +
			"which would lock a client out permanently")
	}
	if l.allow("k") {
		t.Fatal("200ms yielded more than one token; the refill rate is wrong")
	}
}

// TestBucketsAreIndependentPerKey is the assertion that would catch the failure
// this design exists to avoid: one shared bucket for everyone.
//
// ★ A limiter keyed on something constant (r.RemoteAddr behind a same-host
// proxy, say) passes every single-client test above and fails only this one.
func TestBucketsAreIndependentPerKey(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	l := newLimiter(5, 2, clk.now)

	for i := 0; i < 2; i++ {
		if !l.allow("alice") {
			t.Fatalf("alice request %d refused inside her burst", i+1)
		}
	}
	if l.allow("alice") {
		t.Fatal("alice's burst is not enforced")
	}
	// Bob must be untouched by alice exhausting hers.
	if !l.allow("bob") {
		t.Fatal("bob was throttled by alice's traffic: the buckets are shared, " +
			"so one abuser takes the service down for everyone")
	}
}

// TestIdleBucketsAreSwept pins the memory bound. Without it the map grows once
// per distinct key forever — an unbounded leak keyed on attacker-supplied
// values, whose only symptom is the process dying much later.
func TestIdleBucketsAreSwept(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	l := newLimiter(5, 5, clk.now)

	for _, k := range []string{"a", "b", "c"} {
		l.allow(k)
	}
	if got := l.size(); got != 3 {
		t.Fatalf("control group failed: %d buckets after 3 distinct keys, want 3", got)
	}

	// Past the idle horizon, one more call triggers the sweep.
	clk.add(10 * time.Minute)
	l.allow("d")
	if got := l.size(); got != 1 {
		t.Errorf("after the idle horizon the map holds %d buckets, want only "+
			"the live one — idle buckets are never reclaimed", got)
	}
}

// TestWritesAreThrottledEndToEnd drives the real handler, because the unit
// tests above prove the bucket works and say nothing about whether it is WIRED.
func TestWritesAreThrottledEndToEnd(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	p := a.createProposal("Trip")

	// ★ Each write must carry DISTINCT dates. Repeating one range returns 409
	// DUPLICATE_OPTION, and a 409 is not a 429 — the throttle assertion below
	// would then be measuring the duplicate rule rather than the limiter, while
	// still looking like a rate-limit test.
	day := func(i int) map[string]string {
		return map[string]string{
			"startDate": fmt.Sprintf("2026-%02d-%02d", 1+i/28, 1+i%28),
			"endDate":   fmt.Sprintf("2026-%02d-%02d", 1+i/28, 1+i%28),
		}
	}

	// Control group first: a burst of writes at a sane pace must all succeed,
	// or a limiter that refused everything would "pass" the throttle assertion.
	for i := 0; i < 3; i++ {
		if code := a.do("POST", "/api/proposals/"+p.PID+"/options", day(i), nil); code != http.StatusOK {
			t.Fatalf("control group failed: ordinary write %d got %d", i+1, code)
		}
	}

	// Now exceed the burst. The limit is 20; 60 attempts must hit it.
	throttled, other := 0, map[int]int{}
	for i := 3; i < 63; i++ {
		code := a.do("POST", "/api/proposals/"+p.PID+"/options", day(i), nil)
		switch code {
		case http.StatusTooManyRequests:
			throttled++
		case http.StatusOK:
		default:
			other[code]++
		}
	}
	if len(other) > 0 {
		t.Fatalf("unexpected statuses %v — the loop is measuring something other "+
			"than the limiter", other)
	}
	if throttled == 0 {
		t.Error("60 rapid writes produced no 429: the limiter is not wired into " +
			"the request path at all")
	}
}

// TestReadsAreNeverThrottled: throttling reads would break the SSE reconnect
// storm after a restart, turning a brief outage into a long one.
func TestReadsAreNeverThrottled(t *testing.T) {
	e := newEnv(t)
	a := e.newClient("Alice")
	p := a.createProposal("Trip")

	for i := 0; i < 100; i++ {
		if code := a.do("GET", "/api/proposals/"+p.PID, nil, nil); code != http.StatusOK {
			t.Fatalf("read %d got %d — reads must not be rate limited", i+1, code)
		}
	}
}

// TestThrottleResponseIsActionable. A bare 429 with no code and no Retry-After
// is indistinguishable from an outage, and the usual reaction to an outage is
// to retry harder — the opposite of what a throttle needs.
func TestThrottleResponseIsActionable(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	// A limiter with zero capacity: every write is refused, so the shape of the
	// refusal is what is under test here, not the arithmetic.
	empty := newLimiter(1, 0, nil)
	h := limitWrites(empty, empty, empty, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the handler ran despite an empty bucket")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/proposals", nil))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("no Retry-After header: the client cannot tell a throttle from an outage")
	}
	if body := rec.Body.String(); !strings.Contains(body, "RATE_LIMITED") {
		t.Errorf("refusal carries no RATE_LIMITED code: %s", body)
	}
}

// TestRateLimitKeyIsNotTheRemoteAddr is a wiring guard on the source.
//
// ★ Behavioural tests cannot catch this: a RemoteAddr-keyed limiter passes
// every test in this file when run under httptest, because httptest gives each
// request a real and varying peer address. It fails only in production behind
// the gateway, where RemoteAddr is 127.0.0.1 for the entire internet and the
// whole world shares one bucket. That is precisely the shape of defect that
// BR_20260805_reverse_proxy_defeats_requireloopback_admin_exposed already cost
// us once, so it gets a source-level assertion rather than a behavioural one.
func TestRateLimitKeyIsNotTheRemoteAddr(t *testing.T) {
	src, err := os.ReadFile("ratelimit.go")
	if err != nil {
		t.Fatalf("read ratelimit.go: %v", err)
	}
	code := stripGoComments(string(src))

	// Control group: prove the matcher can see a planted use.
	if !strings.Contains("x := r.RemoteAddr", "RemoteAddr") {
		t.Fatal("control group failed: the matcher cannot see a planted RemoteAddr read")
	}

	if strings.Contains(code, "RemoteAddr") {
		t.Error("ratelimit.go reads RemoteAddr; behind the same-host gateway that " +
			"is 127.0.0.1 for every caller, so the whole internet would share one bucket")
	}
	for _, h := range []string{"X-Forwarded-For", "X-Real-IP"} {
		if strings.Contains(code, h) {
			t.Errorf("ratelimit.go reads %s, which any client can forge", h)
		}
	}
}

// ---------------------------------------------------------------------------
// Cursor traffic vs the write budget
// ---------------------------------------------------------------------------

// TestCursorDoesNotStarveTheWriteBudget is the assertion that separates
// "presence chatter" from "the user's actual intent".
//
// ★ The failure it catches is invisible in every other test: POST /cursor is a
// mutating METHOD but not a mutating ACTION — it touches no disk and mints no
// state, it only fans out to a hub. The front end emits one every 60ms while
// the mouse moves (web/app.js:378), i.e. ~16/sec, against a write budget of
// 5/sec burst 20. So four seconds of mouse movement drains a budget that the
// VOTE then needs, and the user sees their vote rejected with 429 for no
// reason they can perceive. Worse, it is intermittent: it depends on whether
// they moved the mouse first.
func TestCursorDoesNotStarveTheWriteBudget(t *testing.T) {
	e := newEnv(t)
	alice := e.newClient("Alice")

	p := alice.createProposal("T")

	// Spend far more than the write burst on cursor traffic alone.
	for i := 0; i < 60; i++ {
		code := alice.do("POST", "/api/proposals/"+p.PID+"/cursor",
			map[string]any{"x": 0.5, "y": 0.5}, nil)
		if code == http.StatusTooManyRequests {
			t.Fatalf("cursor %d was throttled by the WRITE limiter: cursor "+
				"traffic is being charged to the write budget", i+1)
		}
	}

	// The write the user actually cares about must still go through.
	code := alice.do("POST", "/api/proposals/"+p.PID+"/options",
		map[string]any{"startDate": "2026-07-01", "endDate": "2026-07-03"}, nil)
	if code == http.StatusTooManyRequests {
		t.Fatal("the option write was rejected with 429 after only cursor " +
			"traffic: moving the mouse consumed the user's write budget")
	}
	if code != http.StatusOK {
		t.Fatalf("option write: unexpected status %d", code)
	}
}

// TestCursorIsStillLimited: giving cursor its own bucket must not become
// giving cursor NO bucket.
//
// ★ Without this assertion the previous test can be "fixed" by exempting the
// route entirely, which trades a visible failure (votes rejected) for an
// invisible one (an unthrottled fan-out amplifier: one client, one socket,
// N subscribers each getting a write per request).
func TestCursorIsStillLimited(t *testing.T) {
	e := newEnv(t)
	alice := e.newClient("Alice")
	p := alice.createProposal("T")

	// The cursor burst is 60. Control group first: a modest run must all pass,
	// or a bucket that refused everything would satisfy the assertion below
	// while being completely broken.
	for i := 0; i < 30; i++ {
		if code := alice.do("POST", "/api/proposals/"+p.PID+"/cursor",
			map[string]any{"x": 0.5, "y": 0.5}, nil); code != http.StatusNoContent {
			t.Fatalf("control group: cursor %d got %d, want 204", i+1, code)
		}
	}

	throttled, other := 0, map[int]int{}
	for i := 0; i < 200; i++ {
		switch code := alice.do("POST", "/api/proposals/"+p.PID+"/cursor",
			map[string]any{"x": 0.5, "y": 0.5}, nil); code {
		case http.StatusTooManyRequests:
			throttled++
		case http.StatusNoContent:
		default:
			other[code]++
		}
	}
	if len(other) > 0 {
		t.Fatalf("unexpected statuses %v — measuring something other than the limiter", other)
	}
	if throttled == 0 {
		t.Error("230 rapid cursor posts produced no 429: the cursor route is " +
			"unlimited, i.e. a free fan-out amplifier")
	}
}

// TestTransientFanoutIsAWhitelist guards the DIRECTION of the classification.
//
// ★ A blacklist ("everything except the expensive routes is transient") passes
// every behavioural test here and silently exempts each newly added mutating
// route, because forgetting to list one produces no error at all. This asserts
// the ordinary write path is NOT classified as transient.
func TestTransientFanoutIsAWhitelist(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"POST", "/api/proposals/ABC/cursor", true},
		{"POST", "/api/proposals", false},
		{"POST", "/api/proposals/ABC/options", false},
		{"POST", "/api/proposals/ABC/decision", false},
		{"POST", "/api/proposals/ABC/repid", false},
		{"POST", "/api/register", false},
		{"DELETE", "/api/proposals/ABC/cursor", false}, // method still matters
	}
	for _, c := range cases {
		got := isTransientFanout(httptest.NewRequest(c.method, c.path, nil))
		if got != c.want {
			t.Errorf("%s %s: isTransientFanout=%v, want %v", c.method, c.path, got, c.want)
		}
	}
}
