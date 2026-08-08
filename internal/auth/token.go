// Package auth holds the identity mechanism (DD-16) and the authorisation
// decision function (DD-19). Zero third-party dependencies (DD-13).
//
// The two halves are deliberately separate files: token.go answers "who is
// this?" and needs crypto; authorize.go answers "may they?" and is a pure
// function with no crypto, no I/O and no clock. Mixing them would make the
// authorisation rules untestable without minting real tokens.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"
)

// TokenBytes is 32 bytes = 256 bits of entropy (DD-16).
const TokenBytes = 32

// tokenEnc is base32 without padding: the alphabet is A-Z2-7, so a token can be
// read aloud and retyped as a recovery code (DD-2). Base64 would be shorter but
// its alphabet contains +/ and mixed case, which is hostile to transcription.
var tokenEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewToken mints a bearer token and returns the plaintext alongside its
// hex-encoded SHA-256.
//
// ★ The plaintext is returned ONCE and never stored (DD-16). The caller puts it
// in an HttpOnly cookie and shows it to the user as a recovery code; only the
// hash reaches the store or the event log.
func NewToken() (plaintext, hash string, err error) {
	b := make([]byte, TokenBytes)
	// crypto/rand.Read is documented to return an error only when the system
	// source fails, in which case continuing would mint a guessable token —
	// a silent, total defeat of R8. Fail loudly instead (天條 11).
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate token: %w", err)
	}
	plaintext = tokenEnc.EncodeToString(b)
	return plaintext, HashToken(plaintext), nil
}

// HashToken returns the hex-encoded SHA-256 of a token. The hash is the store's
// index key; it is not secret, which is why a plain map lookup on it is safe.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// SameTokenHash reports whether two token hashes match, in constant time.
//
// ★ DD-16 forbids comparing tokens with ==. Go's string equality short-circuits
// at the first differing byte, so the time it takes leaks how long a shared
// prefix is, and an attacker can extend a guess one byte at a time. The map
// lookup that precedes this is on a hash and is not itself a secret comparison;
// this call is what makes the confirmation step constant-time.
func SameTokenHash(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// recoveryGroup is the transcription chunk size for RecoveryCode.
const recoveryGroup = 4

// RecoveryCode renders a token as dash-separated groups for the user to write
// down (DD-2). It is a display transform, not a different credential — the
// token IS the identity, so the recovery mechanism costs nothing extra.
func RecoveryCode(plaintext string) string {
	var b strings.Builder
	for i, r := range plaintext {
		if i > 0 && i%recoveryGroup == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ParseRecoveryCode normalises user-typed input back into a token.
//
// ★ It accepts the shapes a human actually produces — lowercase, spaces,
// missing or extra dashes — because rejecting them would push people toward
// "it doesn't work" rather than a corrected retry. It does NOT accept
// arbitrary length: a wrong-length input is a typo, and returning it silently
// would produce a lookup miss that is indistinguishable from an unknown token.
func ParseRecoveryCode(input string) (string, error) {
	var b strings.Builder
	for _, r := range input {
		switch {
		case r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r':
			continue
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		default:
			b.WriteRune(r)
		}
	}
	got := b.String()
	want := tokenEnc.EncodedLen(TokenBytes)
	if len(got) != want {
		return "", fmt.Errorf("recovery code must be %d characters after removing separators, got %d", want, len(got))
	}
	// Decode to reject characters outside the base32 alphabet. Without this, a
	// transcription slip (O for 0, I for 1) would reach the store as a lookup
	// miss reported as "unknown token" — the user would be told their code is
	// wrong rather than that they mistyped it.
	if _, err := tokenEnc.DecodeString(got); err != nil {
		return "", fmt.Errorf("recovery code contains characters that are not part of a token: %w", err)
	}
	return got, nil
}
