package auth

import (
	"os"
	"strings"
	"testing"
)

// readSource reads one of this package's own files for the wiring guards.
//
// ★ Deliberately reads from disk rather than asserting on a variable. The
// thing being guarded IS the source text — a behavioural test cannot tell
// `==` from ConstantTimeCompare, since both return the right answer.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if len(b) == 0 {
		// An empty read would make every "forbidden pattern absent" check
		// below pass vacuously — absent and failing sharing one output.
		t.Fatalf("%s is empty; the guards below would pass vacuously", name)
	}
	return string(b)
}

// stripGoComments removes // and /* */ comments so prose explaining WHY a
// pattern is forbidden does not itself register as a violation.
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

// TestTokenUniqueness covers tasks 3.1's "1000 tokens, no duplicates".
func TestTokenUniqueness(t *testing.T) {
	const n = 1000
	seenToken := make(map[string]bool, n)
	seenHash := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		tok, hash, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seenToken[tok] {
			t.Fatalf("duplicate token at iteration %d", i)
		}
		if seenHash[hash] {
			t.Fatalf("duplicate hash at iteration %d — collision or a constant hash", i)
		}
		seenToken[tok] = true
		seenHash[hash] = true
	}
	if len(seenToken) != n || len(seenHash) != n {
		t.Fatalf("expected %d distinct tokens/hashes, got %d/%d", n, len(seenToken), len(seenHash))
	}
}

// TestTokenEntropy guards against the degenerate implementations that would
// still pass the uniqueness test above — a counter, or a timestamp.
//
// ★ Uniqueness alone is a weak property: "user-1", "user-2" is perfectly
// unique and completely guessable, and a guessable token silently defeats R8
// with no error anywhere. So assert the SIZE of the secret, not just that the
// values differ.
func TestTokenEntropy(t *testing.T) {
	tok, _, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	// 32 bytes in base32-no-padding = ceil(32*8/5) = 52 characters.
	if want := tokenEnc.EncodedLen(TokenBytes); len(tok) != want {
		t.Errorf("token length = %d, want %d", len(tok), want)
	}
	raw, err := tokenEnc.DecodeString(tok)
	if err != nil {
		t.Fatalf("token is not valid base32: %v", err)
	}
	if len(raw) != TokenBytes {
		t.Errorf("token carries %d bytes of entropy, want %d", len(raw), TokenBytes)
	}

	// A constant or low-entropy generator would produce a byte histogram
	// concentrated on a few values. 100 tokens * 32 bytes = 3200 draws; seeing
	// fewer than 128 distinct byte values would be far outside chance.
	distinct := map[byte]bool{}
	for i := 0; i < 100; i++ {
		_, _, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		tok, _, _ := NewToken()
		b, _ := tokenEnc.DecodeString(tok)
		for _, x := range b {
			distinct[x] = true
		}
	}
	if len(distinct) < 128 {
		t.Errorf("only %d distinct byte values across 3200 draws — the source "+
			"does not look random", len(distinct))
	}
}

// TestHashTokenIsStableAndOneWay checks the index property the store relies on.
func TestHashTokenIsStableAndOneWay(t *testing.T) {
	tok, hash, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if again := HashToken(tok); again != hash {
		t.Errorf("HashToken is not deterministic: %q then %q", hash, again)
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64 hex chars for SHA-256", len(hash))
	}
	// ★ The plaintext must not be recoverable from, or embedded in, the hash.
	// Storing "hash" that contains the token would defeat DD-16's "明碼不落庫"
	// while every other test still passed.
	if strings.Contains(hash, tok) {
		t.Error("the hash contains the token plaintext")
	}
	if hash == tok {
		t.Error("the hash IS the plaintext — the token would be stored in clear")
	}
}

// TestSameTokenHash exercises both verdicts of the constant-time comparison.
func TestSameTokenHash(t *testing.T) {
	a := HashToken("alpha")
	b := HashToken("beta")
	if !SameTokenHash(a, a) {
		t.Error("identical hashes compared unequal")
	}
	if SameTokenHash(a, b) {
		t.Error("different hashes compared equal")
	}
	// Differing lengths must be false, not a panic: ConstantTimeCompare
	// returns 0 for unequal lengths, and a truncated hash reaching this call
	// is exactly the kind of input an attacker supplies.
	if SameTokenHash(a, a[:10]) {
		t.Error("a truncated hash compared equal to the full one")
	}
	if SameTokenHash("", "") == false {
		t.Error("two empty strings should compare equal")
	}
}

// TestNoPlainEqualityOnTokens is a wiring guard (tasks 3.1's grep step,
// mechanised so it cannot be forgotten).
//
// ★ Reads the source from disk on purpose. A behavioural test cannot detect
// `a == b` versus ConstantTimeCompare — both return the right answer; only the
// timing differs, and timing is not observable from a unit test. So the
// assertion has to be about the code.
func TestNoPlainEqualityOnTokens(t *testing.T) {
	// Control group: run the REAL scanner over planted source and require a
	// hit, then over known-clean source and require none.
	//
	// ★ The control must exercise the same function that does the real work.
	// An earlier version of this test asserted only that the planted string
	// contained "==" — which is true of any string containing "==" and proves
	// nothing about the scanner. A control group that does not run the thing
	// being controlled is decoration.
	const planted = "func check(a, b string) bool {\n\tif plaintext == stored {\n\t\treturn true\n\t}\n}"
	if hits := findSecretComparisons(planted); len(hits) == 0 {
		t.Fatal("control group failed: the scanner missed a planted plaintext " +
			"comparison, so a clean result below would prove nothing")
	}
	// The false-positive half: error checks and length checks must NOT trip it.
	// Without this, a scanner that flags every line would also "pass" the
	// control above while making the real check meaningless.
	const clean = "if _, err := tokenEnc.DecodeString(got); err != nil {\n" +
		"if len(got) != want {\n" +
		"return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1\n"
	if hits := findSecretComparisons(clean); len(hits) != 0 {
		t.Fatalf("control group failed: the scanner flags legitimate lines %q — "+
			"a scanner that flags everything cannot distinguish anything", hits)
	}

	src := stripGoComments(readSource(t, "token.go"))
	for _, line := range findSecretComparisons(src) {
		t.Errorf("token.go compares a secret with ==/!=: %q\n"+
			"DD-16 requires crypto/subtle.ConstantTimeCompare", line)
	}

	if !strings.Contains(src, "subtle.ConstantTimeCompare") {
		t.Error("token.go never calls subtle.ConstantTimeCompare (DD-16)")
	}
}

// findSecretComparisons returns lines that compare a secret-bearing identifier
// with == or !=. Comment stripping is the caller's job.
func findSecretComparisons(code string) []string {
	var hits []string
	for _, line := range strings.Split(code, "\n") {
		if !strings.Contains(line, "==") && !strings.Contains(line, "!=") {
			continue
		}
		// Error checks (`err != nil`) and length checks compare no secret.
		// Strings are not nil-comparable in Go, so excluding nil comparisons
		// cannot hide a real plaintext comparison.
		if strings.Contains(line, "nil") || strings.Contains(line, "len(") ||
			strings.Contains(line, "subtle.") {
			continue
		}
		for _, secret := range []string{"plaintext", "token", "hash", "Hash"} {
			if strings.Contains(line, secret) {
				hits = append(hits, strings.TrimSpace(line))
				break
			}
		}
	}
	return hits
}

// TestRecoveryCodeRoundTrip covers DD-2: the token IS the recovery code, so the
// display transform must be losslessly reversible.
func TestRecoveryCodeRoundTrip(t *testing.T) {
	tok, _, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	code := RecoveryCode(tok)
	if !strings.Contains(code, "-") {
		t.Errorf("recovery code has no group separators: %q", code)
	}

	back, err := ParseRecoveryCode(code)
	if err != nil {
		t.Fatalf("ParseRecoveryCode(%q): %v", code, err)
	}
	if back != tok {
		t.Errorf("round trip changed the token:\n got %q\nwant %q", back, tok)
	}

	// The shapes a human actually produces. Each must resolve to the same
	// token — a recovery path that only accepts one exact rendering sends the
	// user back to "it doesn't work" with no way forward.
	variants := map[string]string{
		"lowercase":      strings.ToLower(code),
		"no separators":  tok,
		"spaces":         strings.ReplaceAll(code, "-", " "),
		"trailing space": code + "  ",
		"newline":        code + "\n",
	}
	for name, v := range variants {
		got, err := ParseRecoveryCode(v)
		if err != nil {
			t.Errorf("%s: ParseRecoveryCode(%q): %v", name, v, err)
			continue
		}
		if got != tok {
			t.Errorf("%s: got %q, want %q", name, got, tok)
		}
	}
}

// TestParseRecoveryCodeRejectsBadInput is the deny half.
//
// ★ Without it, `func ParseRecoveryCode(s string) (string, error) { return s, nil }`
// passes every round-trip case above. The rejection cases are what give the
// round-trip test its meaning.
func TestParseRecoveryCodeRejectsBadInput(t *testing.T) {
	tok, _, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	bad := map[string]string{
		"empty":          "",
		"too short":      tok[:10],
		"too long":       tok + "AAAA",
		"one char short": tok[:len(tok)-1],
		// 0, 1, 8 and 9 are not in the base32 alphabet — the classic
		// transcription slips for O, I, B and g.
		"digit zero":  "0" + tok[1:],
		"digit one":   "1" + tok[1:],
		"punctuation": "!" + tok[1:],
	}
	for name, in := range bad {
		if got, err := ParseRecoveryCode(in); err == nil {
			t.Errorf("%s: ParseRecoveryCode(%q) accepted it and returned %q; "+
				"a malformed code must be reported as malformed, not passed "+
				"through to become an indistinguishable lookup miss", name, in, got)
		}
	}
}
