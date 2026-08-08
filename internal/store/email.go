package store

import (
	"fmt"
	"net/mail"
	"strings"
)

// ValidateEmail rejects an address that is not a single, bare mailbox.
//
// ★ net/mail.ParseAddress, NOT a hand-rolled regex and NOT a bare
// strings.Contains("@") check. The address is about to be handed to
// net/smtp as an envelope recipient, and the failure mode of an under-strict
// check is not a rejected login — it is an SMTP command built from
// user-controlled bytes.
//
// ★ THE THREE EXTRA CHECKS BELOW EXIST BECAUSE ParseAddress ALONE IS NOT
// SUFFICIENT FOR THIS USE, and each one has a shape that would otherwise pass:
//
//	`"A" <a@b.com>`      parses fine — a display name is legal in a header,
//	                     but here it would mean the stored "email" is not the
//	                     address, so a later comparison against a@b.com fails
//	                     and the user is silently routed to the suffix branch.
//	"a@b.com\r\nBcc: x"  ParseAddress refuses this one, but the check is kept
//	                     explicit: CRLF is the SMTP command separator, so a
//	                     value carrying one is header injection by definition,
//	                     and this must not depend on a library detail staying
//	                     the way it is today.
//	"a@localhost"        parses fine and has no dot in the domain. Deliverable
//	                     only on a host that means something by it; as a user's
//	                     verification address it is almost certainly a typo,
//	                     and accepting it produces a magic link that never
//	                     arrives with no error anywhere.
func ValidateEmail(email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return fmt.Errorf("email is empty")
	}
	if len(email) > MaxEmailLen {
		return fmt.Errorf("email exceeds %d characters", MaxEmailLen)
	}
	// Checked BEFORE parsing: a parser is entitled to normalise whitespace,
	// and a check that runs after normalisation can be looking at bytes the
	// attacker no longer needs to have survived.
	if strings.ContainsAny(email, "\r\n") {
		return fmt.Errorf("email must not contain line breaks")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return fmt.Errorf("email format is invalid: %w", err)
	}
	// ParseAddress accepts `Name <a@b.com>` and returns the two parts
	// separately. Requiring the input to equal the bare address is what makes
	// "what we stored" and "what we will compare and mail" the same string.
	if addr.Name != "" || addr.Address != email {
		return fmt.Errorf("email must be a bare address, not a display name form")
	}
	at := strings.LastIndex(addr.Address, "@")
	if at < 0 {
		return fmt.Errorf("email format is invalid: no domain")
	}
	local, domain := addr.Address[:at], addr.Address[at+1:]
	if local == "" || domain == "" {
		return fmt.Errorf("email format is invalid: empty local or domain part")
	}
	if !strings.Contains(domain, ".") {
		return fmt.Errorf("email domain %q has no dot — a bare hostname is almost always a typo", domain)
	}
	return nil
}
