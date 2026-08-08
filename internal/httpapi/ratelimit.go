package httpapi

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"teamdating/internal/auth"
)

// Rate limiting (tasks 6.2, risk F). Hand-written token bucket —
// golang.org/x/time/rate is third-party and DD-13 forbids it.
//
// ─────────────────────────────────────────────────────────────────────────────
// ★ WHY THIS IS NOT KEYED ON THE CLIENT IP, even though tasks.md asked for it
//
// tasks 6.2 says "單一 IP 短時間大量建提案 → 應被限流". That cannot be built in
// this deployment, and the reason is worth writing down because the failure is
// invisible: opencode-gateway proxies /teamdating from the same host and adds
// NO forwarding header. Measured by intercepting the forwarded bytes with
// `nc -l` (BR_20260805_gateway_does_not_forward_client_ip):
//
//	GET /teamdating/api/health HTTP/1.1
//	Host: example.internal:1080
//	User-Agent: curl/8.5.0
//	Accept: */*
//
// No X-Forwarded-For, no X-Real-IP, no Forwarded — and RemoteAddr is 127.0.0.1
// for every caller on earth. A per-IP limiter here would compile, pass its
// tests, and put the entire internet in ONE bucket: either everyone is
// throttled by one abuser, or the limit is set so high it throttles nobody.
// Both failures are silent. So the key has to be something that actually
// varies per client.
//
// ─────────────────────────────────────────────────────────────────────────────
// ★ WHAT IT IS KEYED ON, AND THE HONEST LIMIT OF THAT
//
// Two buckets, because the two abuse shapes are different:
//
//  1. IDENTIFIED writes (everything except register) — keyed on the token
//     hash. Every client holds a bearer token, so this genuinely separates
//     clients. An abuser can mint more tokens, but that costs a register call,
//     which is bucket 2.
//
//  2. REGISTER — there is no key at all before the token exists. A global
//     bucket is the only honest option, and it is stated as such rather than
//     dressed up: it caps how fast identities can be minted SYSTEM-WIDE.
//     Under attack this does degrade service for new legitimate users. That is
//     a real cost, not an oversight — the alternative (no limit) means the
//     roster and the event log grow without bound, which is worse and
//     permanent. Fixing it properly needs the client IP, i.e. it is blocked on
//     the gateway BR.
//
// Reads are not limited: they touch no disk and mint no state.
type limiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	rate     float64 // tokens per second
	burst    float64
	lastSwep time.Time
	now      func() time.Time // injected for tests; time.Now in production
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(perSecond, burst float64, now func() time.Time) *limiter {
	if now == nil {
		now = time.Now
	}
	return &limiter{
		buckets:  make(map[string]*bucket),
		rate:     perSecond,
		burst:    burst,
		lastSwep: now(),
		now:      now,
	}
}

// allow reports whether the caller may proceed, consuming one token if so.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	// Refill for elapsed time, capped at burst.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	l.sweepLocked(now)

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweepLocked drops buckets that have been full (i.e. idle) long enough that
// forgetting them changes no decision.
//
// ★ Without this the map is an unbounded memory leak keyed on attacker-supplied
// values — and a leak has no symptom until the process dies, at which point the
// cause looks like anything but a rate limiter.
func (l *limiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSwep) < time.Minute {
		return
	}
	l.lastSwep = now
	// A bucket refills to full in burst/rate seconds; after that it is
	// indistinguishable from a fresh one.
	idle := time.Duration(l.burst/l.rate*float64(time.Second)) + time.Minute
	for k, b := range l.buckets {
		if now.Sub(b.last) > idle {
			delete(l.buckets, k)
		}
	}
}

// size reports the live bucket count. Test-only observability: the sweep is
// otherwise unobservable, and an unobservable cleanup is one that can silently
// stop working.
func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// rateLimitKey derives the bucket key for a request.
//
// ★ Deliberately NOT r.RemoteAddr — see the file header. The token hash is
// used rather than the raw token so the limiter never holds credentials in a
// map that outlives the request.
func rateLimitKey(r *http.Request) (key string, identified bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return auth.HashToken(c.Value), true
}

// isTransientFanout reports whether a mutating request only fans out to the
// hub instead of changing durable state.
//
// ★ WHY THIS EXISTS (BR_20260805_cursor_traffic_drains_the_user_write_budget)
//
// The limiter used to decide by METHOD alone, and POST /cursor is a mutating
// METHOD that is not a mutating ACTION: handleCursor touches no disk and mints
// no state, it only calls hub.Cursor. Meanwhile the front end emits one every
// 60ms while the mouse moves (web/app.js:378) — about 16/sec against a write
// budget of 5/sec burst 20. So roughly a second of mouse movement drained the
// budget the user's VOTE then needed, and the vote came back 429. It was
// intermittent, because it depended on whether they had moved the mouse first,
// which is the hardest kind of bug for a user to report.
//
// ★ WHITELIST, NOT BLACKLIST. This returns true only for routes known to be
// transient. A newly added mutating route is therefore charged to the write
// budget by DEFAULT — it is protected by omission rather than exposed by it.
// The inverse (a blacklist of "expensive" routes) would silently exempt every
// route somebody forgot to list, and forgetting produces no error.
func isTransientFanout(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cursor")
}

// limitWrites throttles state-changing requests. Safe methods pass through
// untouched.
func limitWrites(writes, registers, cursors *limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		key, identified := rateLimitKey(r)

		// Transient fan-out gets its own budget, so presence chatter can never
		// consume what the user's actual intent needs. It is still LIMITED —
		// exempting it would trade one silent failure for another, since an
		// unthrottled cursor route is a free fan-out amplifier.
		if identified && isTransientFanout(r) {
			if !cursors.allow(key) {
				w.Header().Set("Retry-After", "1")
				writeErr(w, http.StatusTooManyRequests, "RATE_LIMITED",
					"游標更新太頻繁，請稍候再試")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		lim, bucketKey := writes, key
		if !identified {
			// No token yet: the only such write is register. There is nothing
			// client-specific to key on, so this is a system-wide cap.
			lim, bucketKey = registers, "anonymous"
		}

		if !lim.allow(bucketKey) {
			// Retry-After is not decoration: without it a client cannot tell a
			// throttle from an outage, and the usual reaction to an outage is
			// to retry harder — which is precisely the wrong response.
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, "RATE_LIMITED",
				"操作太頻繁，請稍候再試")
			return
		}
		next.ServeHTTP(w, r)
	})
}
