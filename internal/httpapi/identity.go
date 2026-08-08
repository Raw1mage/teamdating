package httpapi

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"teamdating/internal/auth"
	"teamdating/internal/store"
)

// idEnc mints pid/oid/uid values. base32 with no padding keeps them
// URL-safe without escaping — a pid travels in a shared link (R8).
var idEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// newID returns a 128-bit random identifier.
//
// ★ pid MUST be unguessable: R8's entire access model is "knowing the URL is
// what grants entry", so a sequential or timestamp-derived pid would let anyone
// enumerate proposals by counting — and that failure produces no error message
// anywhere, which is why it has to be right at the source.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return idEnc.EncodeToString(b), nil
}

// cookieName carries the bearer token (DD-16).
const cookieName = "td_token"

// errorBody is the JSON shape of every failure (errors.md: a bare status code
// tells the user only that something failed, which is not actionable).
type errorBody struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Code: code, Msg: msg})
}

// writeDeny renders an authorisation refusal. Always 403 with the code the
// decision carried — never a bare status.
func writeDeny(w http.ResponseWriter, d auth.Decision) {
	status := http.StatusForbidden
	switch d.Code {
	case auth.CodeProposalNotFound, auth.CodeOptionNotFound:
		status = http.StatusNotFound
	}
	writeErr(w, status, d.Code, d.Reason)
}

// decodeJSON reads a request body with a size cap.
//
// ★ The cap is not politeness: without MaxBytesReader a single request can
// stream unbounded data into memory, and M1 has no rate limiting (errors.md
// RATE_LIMIT_ABSENT). DisallowUnknownFields makes a client typo an error
// rather than a silently ignored field — a misspelled "titel" would otherwise
// create a proposal with an empty title and no complaint.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("malformed request body: %w", err)
	}
	return nil
}

// isLoopback reports whether the request came from the local machine (DD-3).
//
// ★ It reads ONLY r.RemoteAddr, which is the peer address of the TCP
// connection and cannot be set by the client. X-Forwarded-For is deliberately
// never consulted: behind a reverse proxy any client can send that header with
// any value, so trusting it would let a remote caller claim to be 127.0.0.1
// and reach the admin route (risk C).
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// A RemoteAddr we cannot parse is not proof of locality. Refusing here
		// keeps "unparseable" from silently becoming "allowed".
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// identity is the authenticated subject of a request.
type identity struct {
	UID        string
	Name       string
	Registered bool
}

func (i identity) actor() auth.Actor {
	return auth.Actor{UID: i.UID, Registered: i.Registered}
}

// identify resolves the bearer cookie to a roster entry.
//
// ★ The uid comes from the token and ONLY from the token — never from the
// request body (DD-8, TV-5). A handler that read a uid field would let anyone
// act as anyone while every test that used the normal frontend still passed.
func identify(st *store.Store, r *http.Request) identity {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return identity{}
	}
	hash := auth.HashToken(c.Value)
	uid, ok := st.UIDByTokenHash(hash)
	if !ok {
		return identity{}
	}
	u, ok := st.User(uid)
	if !ok {
		return identity{}
	}
	// One identity may hold several tokens, one per device (multi-token DD-1).
	// Find the one this request presented; the others are equally valid and
	// belong to the caller's other devices.
	ref, ok := u.FindToken(hash)
	if !ok {
		return identity{}
	}
	// Confirm the match in constant time (DD-16). The map lookup above is on a
	// hash, which is not secret; this is the comparison that must not leak
	// timing information about the stored value.
	if !auth.SameTokenHash(ref.Hash, hash) {
		return identity{}
	}
	// Refresh this device's LastSeenAt so eviction can order by actual use
	// (DD-2). Throttled inside the store: the overwhelming majority of calls
	// take no writer lock and write nothing, so the read path stays a read path
	// (DD-7).
	//
	// The error is deliberately dropped. A failed bookkeeping write must not
	// turn an otherwise valid request into an authentication failure — the
	// worst case is that this token's LRU position is a little stale.
	_, _ = st.TouchTokenIfStale(uid, hash)
	// A name is what makes an identity "registered" — the S3/S4 gate. A token
	// with no name may read; it may not write.
	return identity{UID: uid, Name: u.Name, Registered: strings.TrimSpace(u.Name) != ""}
}

// cookiePath derives the cookie's scope from the service's own base prefix.
//
// ★ WHY THIS IS NOT "/" (BR_20260805_cookie_path_leaks_token_to_sibling_apps)
//
// A cookie's scope is (domain, path), and behind a path-multiplexing reverse
// proxy the domain axis does NOT isolate anything: every unrelated app mounted
// on the same origin under its own prefix (/<sibling-app>, …) shares that
// domain. With Path=/ the browser attaches this service's bearer token to every
// one of them — measured on such a deployment:
//
//	$ curl -b jar http://<host>/<sibling-app>/api/anything
//	> Cookie: td_token=TWSK33YL…
//
// HttpOnly does not help here and is easy to misread as if it did: it stops a
// script on the page from READING the cookie, not the browser from SENDING it.
// So a neighbour's server receives the token directly, and one that merely logs
// request headers writes a 400-day credential to disk.
//
// Scoping to the base prefix makes a sibling's prefix fall outside the cookie's
// path, so the browser never attaches it. Empty base (direct run, no proxy)
// keeps "/", which is correct there because nothing else shares that origin.
func cookiePath(base string) string {
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		return "/"
	}
	return base + "/"
}

func setTokenCookie(w http.ResponseWriter, token, base string, secure bool) {
	// ★ Clear any cookie left over from before the scope was narrowed. A
	// Path=/ cookie and a Path=/teamdating/ cookie are DIFFERENT cookies that
	// coexist, so without this the old wide-scoped one keeps leaking to the
	// neighbours until it expires — 400 days from now, silently, while the fix
	// looks like it landed.
	if p := cookiePath(base); p != "/" {
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: "", Path: "/", MaxAge: -1,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure,
		})
	}
	http.SetCookie(w, &http.Cookie{
		Name:  cookieName,
		Value: token,
		Path:  cookiePath(base),
		// HttpOnly keeps the token out of reach of any script on the page, so
		// an injected script cannot exfiltrate the identity (DD-16). Note it
		// does NOT limit which servers receive the cookie — that is what the
		// Path above is for.
		HttpOnly: true,
		// Lax rather than Strict: the proposal link is followed from a chat
		// app, and Strict would drop the cookie on that first navigation,
		// making a returning user look like a new one.
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   400 * 24 * 60 * 60,
	})
}
