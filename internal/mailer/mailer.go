// Package mailer sends the magic-link verification mail (DD-1, DD-10).
// Zero third-party dependencies: net/smtp from the standard library (DD-13).
package mailer

import (
	cryptorand "crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// Defaults for the local LAN MTA relay (DD-10, 使用者裁示 2026-08-06).
//
// ★ NO USER / PASS EXISTS ANYWHERE IN THIS PACKAGE, AND THAT IS THE DESIGN,
// NOT AN OMISSION. The relay accepts LAN-local submission and performs the
// real delivery itself; the SMTP conversation in Send presents no auth. An
// earlier draft carried TEAMDATING_SMTP_USER / _PASS fields — they were
// removed because credentials that are never checked are worse than absent
// ones: the next reader sets them, observes mail being delivered, and
// concludes authentication is in force when nothing has ever verified them.
const (
	DefaultHost = "localhost:25"
	DefaultFrom = "noreply@localhost"

	EnvHost = "TEAMDATING_SMTP_HOST"
	EnvFrom = "TEAMDATING_SMTP_FROM"
)

// Sender delivers one message. Everything above this interface is testable
// without a network.
//
// ★ THE SEAM EXISTS SO FAILURE IS REACHABLE IN A TEST, WHICH IS THE HALF THAT
// MATTERS. The fail-loud contract (SMTP_SEND_FAILED must refuse the binding —
// TV-10) cannot be demonstrated against a real relay: a test that only ever
// observes successful delivery proves the success path and says nothing about
// the branch that is supposed to protect the user. A fake that can be told to
// fail is what gives TV-10 any discriminating power at all.
//
// ★ AND IT IS THE REASON FOR THE WIRING GUARD IN
// internal/httpapi/email_test.go (TestMailerIsWiredIntoNew). It lives with the
// wiring it guards, not with this package: the line it protects is the one in
// httpapi.New that constructs the REAL sender, so a guard placed here would be
// reading a file that cannot contain the defect. Injecting a
// dependency makes the pure logic testable and simultaneously makes the
// production wiring — the one line that constructs the REAL sender — the
// blind spot: every test passes against the fake while the live path is never
// executed by anything. So the guard reads this file's source and asserts
// SendMail is actually reached.
type Sender interface {
	// Send delivers a message. It MUST return a non-nil error when delivery
	// did not happen; returning nil on a failure would let the caller bind a
	// device to an identity whose owner never received the link.
	Send(to, subject, body string) error
}

// SMTPSender is the production Sender: a direct submission to the LAN relay.
type SMTPSender struct {
	// Host is "host:port". Empty falls back to DefaultHost.
	Host string
	// From is the envelope sender. Empty falls back to DefaultFrom.
	From string
}

// NewSMTPSender reads the two environment variables (DD-10).
//
// ★ Falling back to a DEFAULT here is deliberate and is not the "silent
// fallback" the project forbids: there is no ambiguity to hide — the local
// relay address is a deployment constant, and an unset variable means "use the
// documented default", not "we could not determine the value". What is NOT
// defaulted anywhere is delivery success; see Send.
func NewSMTPSender() *SMTPSender {
	return &SMTPSender{
		Host: envOr(EnvHost, DefaultHost),
		From: envOr(EnvFrom, DefaultFrom),
	}
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// Send submits the message to the relay.
//
// ★ NO AUTH IS PRESENTED, BY DESIGN (DD-10). The relay accepts LAN submission;
// there is no credential to present.
//
// ★ THE CONVERSATION IS DRIVEN BY HAND INSTEAD OF smtp.SendMail, AND THE
// REASON IS NOT STYLE. SendMail upgrades to STARTTLS whenever the server
// advertises it and then verifies the certificate against the dial address,
// with no way to influence either decision. A LAN relay typically advertises
// STARTTLS while presenting a certificate issued for its public mail names —
// not for the bare internal hostname the submission is dialled by — so
// verification against the dial address fails with
//
//	x509: certificate is valid for <public names>, not <internal hostname>
//
// and no mail is delivered at all. Driving the same standard library client
// directly is what makes the TLS config reachable.
//
// ★ InsecureSkipVerify IS A DECIDED TRADE-OFF (使用者裁示 2026-08-07), NOT AN
// OVERSIGHT, and it is narrow: the peer is the single relay named by
// TEAMDATING_SMTP_HOST, expected to sit on a trusted internal segment, whose
// certificate may not carry that bare hostname. The alternative — a
// certificate reissued to cover the internal name — is an infrastructure
// change outside this service. What is deliberately NOT done is dropping TLS:
// the session is still encrypted, only the name is unchecked. Do not widen
// this to a configurable "skip verify anywhere" flag; the moment the host
// becomes an arbitrary address this stops being a LAN decision.
//
// ★ Every error is returned, never logged-and-swallowed. A swallowed SMTP
// error makes "the mail went out" and "the mail was silently dropped" produce
// the same outcome for the caller, which is the exact shape the verification
// gate cannot tolerate: the user would be told to check an inbox that will
// never receive anything, and — worse in the version that swallows — the
// binding would proceed. That contract is why each step below is checked
// individually rather than deferred.
func (s *SMTPSender) Send(to, subject, body string) error {
	host, from := s.Host, s.From
	if strings.TrimSpace(host) == "" {
		host = DefaultHost
	}
	if strings.TrimSpace(from) == "" {
		from = DefaultFrom
	}
	msg, err := BuildMessage(from, to, subject, body)
	if err != nil {
		return err
	}

	c, err := smtp.Dial(host)
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", host, err)
	}
	// ★ Close (not Quit) on every failure path. Quit is the orderly goodbye
	// and is sent explicitly at the end; this only guarantees the socket is
	// released when we leave early, and its own error is intentionally
	// ignored — reporting a teardown failure would mask the real cause.
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		// ★ ServerName is set even though InsecureSkipVerify makes it
		// irrelevant to the verification result: it still goes out as SNI, so
		// a relay serving several names hands back the intended certificate
		// rather than a default one, and a future reader who removes the skip
		// finds the field already correct instead of silently unset.
		serverName := host
		if h, _, splitErr := net.SplitHostPort(host); splitErr == nil {
			serverName = h
		}
		if err := c.StartTLS(&tls.Config{
			ServerName:         serverName,
			InsecureSkipVerify: true, //nolint:gosec // LAN relay, name mismatch is expected (see ★ above)
		}); err != nil {
			return fmt.Errorf("smtp starttls %s: %w", host, err)
		}
	}

	if err := c.Mail(from); err != nil {
		return fmt.Errorf("smtp MAIL FROM %s via %s: %w", from, host, err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("smtp RCPT TO via %s: %w", host, err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA via %s: %w", host, err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp write body via %s: %w", host, err)
	}
	// ★ THE CLOSE OF THE DATA WRITER IS WHERE THE RELAY ACCEPTS OR REJECTS THE
	// MESSAGE — it is not cleanup. Deferring it, or ignoring its error, turns
	// a refused message into a successful-looking send: the bytes were written
	// and nothing else would ever report otherwise.
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close data via %s: %w", host, err)
	}
	if err := c.Quit(); err != nil {
		return fmt.Errorf("smtp quit %s: %w", host, err)
	}
	return nil
}

// BuildMessage renders RFC 5322 headers plus a UTF-8 body.
//
// ★ HEADER INJECTION IS REFUSED HERE, NOT SANITISED. A CR or LF in an address
// or subject ends the current header and starts another, so a crafted value
// adds a Bcc: and turns this into an open relay for its author. Stripping the
// bytes instead would deliver a mangled message and report success; refusing
// makes the attempt visible to the caller, which is the only way it is ever
// noticed.
//
// ★ The subject is base64 encoded-word wrapped ONLY because it is Chinese:
// a raw UTF-8 Subject: header is technically out of spec and renders as mojibake
// in several clients, which looks like a broken service rather than a bad header.
//
// ★ DATE AND MESSAGE-ID ARE NOT OPTIONAL POLISH — THEIR ABSENCE IS WHY NO MAIL
// EVER ARRIVED IN PRODUCTION, AND THE FAILURE WAS COMPLETELY SILENT. RFC 5322
// makes Date and From the two mandatory fields; Message-ID is a de-facto
// requirement at every large receiver. An earlier version of this function
// emitted neither, and the relay log recorded the exact shape of the defect:
//
//	cleanup[30393]: BFA7A21D281D: message-id=<>
//	smtp[30451]:    to=<…@gmail.com>, status=sent (250 2.0.0 OK …)
//
// The receiver answered 250 and then dropped the message: "accepted" and
// "delivered" produced the same observable outcome, so every layer below —
// SMTP_SEND_FAILED, the pending table, the caller — reported success while the
// user's inbox stayed empty. A same-LAN test could never have caught it either;
// local delivery does not apply these rules, so the send has to be aimed at a
// real external receiver to have any discriminating power at all.
func BuildMessage(from, to, subject, body string) ([]byte, error) {
	for _, f := range []struct{ name, val string }{
		{"from", from}, {"to", to}, {"subject", subject},
	} {
		if strings.ContainsAny(f.val, "\r\n") {
			return nil, fmt.Errorf("mail %s contains a line break (header injection refused)", f.name)
		}
	}
	if strings.TrimSpace(to) == "" {
		return nil, fmt.Errorf("mail recipient is empty")
	}
	mid, err := messageID(from)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + encodeHeader(subject) + "\r\n")
	// ★ RFC1123Z, not RFC1123: the numeric offset (+0800) is what RFC 5322
	// specifies. The named-zone form (CST) is ambiguous across continents and
	// some receivers treat an unparseable Date as no Date at all.
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("Message-ID: " + mid + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return []byte(b.String()), nil
}

// messageID builds a globally unique <local@domain> identifier.
//
// ★ A crypto/rand FAILURE RETURNS AN ERROR RATHER THAN FALLING BACK TO A
// TIMESTAMP OR A COUNTER. A predictable Message-ID is not merely untidy: two
// messages colliding on one id are deduplicated by the receiver, so the second
// magic-link would vanish with every layer still reporting success — the same
// silent-drop shape this header exists to fix. Refusing is the only outcome
// the caller can act on.
//
// ★ The domain half is taken from the sender address so the id is aligned with
// the sending domain; receivers score a Message-ID whose domain is unrelated to
// From as a spam signal. A sender with no "@" falls back to the host name of
// the default sender rather than emitting a bare local part, which is
// syntactically invalid and rejected outright by strict receivers.
func messageID(from string) (string, error) {
	var raw [16]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("message-id: %w", err)
	}
	domain := "localhost"
	if at := strings.LastIndex(from, "@"); at >= 0 && at+1 < len(from) {
		domain = strings.Trim(from[at+1:], "<> ")
	}
	return "<" + hex.EncodeToString(raw[:]) + "." +
		fmt.Sprintf("%d", time.Now().UnixNano()) + "@" + domain + ">", nil
}

// encodeHeader wraps a non-ASCII header value as an RFC 2047 encoded-word.
//
// ★ ASCII is passed through unchanged rather than always encoding: an
// encoded-word around a plain English subject is legal but makes the header
// unreadable in logs and in any tooling that greps them, and readable evidence
// is worth more than uniformity here.
func encodeHeader(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

// VerifySubject / VerifyBody render the magic-link mail (DD-1).
//
// ★ The mail states the 15-minute expiry and the one-time nature IN THE BODY.
// A link that silently stops working looks like a broken service; a link that
// said it would expire looks like a service that works. Same bytes on the
// wire, entirely different user conclusion.
const VerifySubject = "行程統合：認證你的電子郵件"

// VerifyBody builds the message body around the confirmation link.
func VerifyBody(name, link string) string {
	var b strings.Builder
	b.WriteString("你好，" + name + "：\r\n\r\n")
	b.WriteString("有人（應該是你）在行程統合以這個名字登入，並要求把這台裝置接回你的既有身分。\r\n")
	b.WriteString("請點下面的連結完成認證：\r\n\r\n")
	b.WriteString(link + "\r\n\r\n")
	b.WriteString("這個連結 15 分鐘內有效，而且只能使用一次。\r\n")
	b.WriteString("如果不是你本人操作，不用理會這封信：沒有點連結，就不會有任何裝置被接上你的身分。\r\n")
	return b.String()
}
