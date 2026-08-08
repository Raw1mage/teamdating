package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"teamdating/internal/auth"
)

// Concurrency vectors for the multi-token read path.
//
// ★ WHY THIS FILE EXISTS AT ALL. The rest of the multi-token suite is
// single-goroutine, and `go test -race` on a single-goroutine suite is GREEN BY
// CONSTRUCTION — the detector reports races between goroutines, so a suite that
// never starts a second one cannot produce a finding no matter how broken the
// code is. That made "-race passes" read as "no races", when it meant "nothing
// was concurrent". A green -race run is evidence only in proportion to the
// concurrency the tests actually create; this file is what supplies it for the
// identify() path, which in production runs on every request in parallel.

// meOnce issues GET /api/me carrying the client's cookies.
//
// ★ Returns errors rather than calling t.Fatalf, because it runs on a
// non-test goroutine and testing.T's failure methods are only legal on the
// goroutine running the test function. The existing client.do helper cannot be
// reused here for exactly that reason.
func meOnce(c *client) (meResp, error) {
	var out meResp
	req, err := http.NewRequest("GET", c.srv.URL+"/api/me", nil)
	if err != nil {
		return out, err
	}
	for _, ck := range c.jar {
		req.AddCookie(ck)
	}
	resp, err := c.srv.Client().Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("GET /api/me: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

// TestIdentifyIsRaceFreeWhileTokensMutate is the concurrency vector for the
// User-copy aliasing defect.
//
// ★ THE DEFECT IT GUARDS. store.User() hands the caller a User by value. While
// User held only value fields that was a complete copy; once Tokens []TokenRef
// existed, the returned struct SHARED the store's backing array. identify()
// then walks that array (FindToken) with no lock held, while any token
// mutation — eviction inside AddToken, or RevokeToken — rewrites the same
// array in place under the writer lock (removeToken compacts via
// u.Tokens[:0]). Two goroutines, same memory, one of them writing.
//
// ★ WHY THE READER'S OWN TOKEN IS NEVER THE ONE REMOVED. The writer adds a
// throwaway token and immediately revokes it, so the identity never exceeds
// two tokens and eviction never fires on the reader's. That is deliberate: if
// the reader's token could be evicted mid-run, a failing arrival counter would
// be ambiguous between "the read path broke" and "the token was legitimately
// gone". RevokeToken reaches the SAME removeToken write that the eviction path
// does, so nothing about the race is weakened by keeping the reader alive.
//
// ★ THE ARRIVAL COUNTER IS LOAD-BEARING. A reader whose requests all failed
// early — wrong cookie, 404 route, server not up — would also produce a clean
// -race run, and "no race found" would then mean "the racing line never
// executed". identify() returns an EMPTY identity from every early exit, so
// only a response carrying the right uid AND registered=true proves control
// reached FindToken, which is the racing read. The test therefore fails if
// arrivals is too low, independently of whether the detector fired.
func TestIdentifyIsRaceFreeWhileTokensMutate(t *testing.T) {
	e := newEnv(t)
	reader := e.newClient("小明")

	const churns = 120
	// minArrivals is a floor, not an expectation: the reader loops freely for
	// the writer's whole run and normally lands in the hundreds. It exists to
	// fail loudly if the reader was silently doing nothing.
	const minArrivals = 20

	var arrivals, mismatches atomic.Int64
	var mu sync.Mutex
	var firstReadErr, firstWriteErr error
	recordRead := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstReadErr == nil {
			firstReadErr = err
		}
	}
	recordWrite := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstWriteErr == nil {
			firstWriteErr = err
		}
	}

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Reader: the production HTTP read path, identify() included.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			me, err := meOnce(reader)
			if err != nil {
				recordRead(err)
				continue
			}
			if me.UID == reader.UID && me.Registered {
				arrivals.Add(1)
				continue
			}
			// A wrong-but-successful answer is worse than an error: it means
			// the read path resolved to something else while the token was
			// still valid.
			mismatches.Add(1)
		}
	}()

	// Writer: churns tokens on the SAME identity, hitting removeToken.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < churns; i++ {
			h := auth.HashToken(fmt.Sprintf("churn-%d", i))
			if err := e.st.AddToken(reader.UID, h); err != nil {
				recordWrite(err)
				return
			}
			if err := e.st.RevokeToken(reader.UID, h); err != nil {
				recordWrite(err)
				return
			}
		}
	}()

	wg.Wait()

	if firstWriteErr != nil {
		t.Fatalf("writer failed: %v — the mutation side never ran, so a clean "+
			"race report would prove nothing", firstWriteErr)
	}
	if got := arrivals.Load(); got < minArrivals {
		t.Fatalf("only %d authenticated reads reached FindToken (want >= %d); "+
			"first reader error: %v — the racing read never executed, so this "+
			"test cannot have detected anything",
			got, minArrivals, firstReadErr)
	}
	if got := mismatches.Load(); got != 0 {
		t.Errorf("%d reads resolved to the wrong identity while tokens were "+
			"being mutated", got)
	}
	t.Logf("arrivals=%d (authenticated reads through FindToken), churns=%d",
		arrivals.Load(), churns)
}
