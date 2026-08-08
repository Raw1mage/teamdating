package httpapi

import (
	"html/template"
	"log"
	"net/http"
	"strings"
)

// The magic link is opened by a HUMAN in a browser, from a mail client. That
// path used to answer with the raw API JSON, which is how a recovery code came
// to be rendered as text on a phone screen (and into browser history's page
// title, and into whatever screenshot the user took to ask why it looked
// broken). A page is not decoration here — the representation IS part of the
// security boundary.
//
// ★ WHICH representation is chosen by content negotiation, NOT by method.
// Method-based selection (GET=page, POST=JSON) was rejected: GET is the only
// request a mail client can make, so it is the ONLY path a real user takes —
// moving the binding assertions onto POST would leave the real path asserted
// only for its wording.
//
// ★ ABSENT Accept means JSON. That is the API default and it is explicit, not
// a fallback: a caller that says nothing gets the machine representation, a
// caller that asks for text/html gets the page. Neither answer is a guess made
// on a caller's behalf when the other failed.
func prefersHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// verifyPageData is everything the page may render.
//
// ★ THERE IS NO uid AND NO recovery FIELD, AND THAT ABSENCE IS THE FIX. A page
// that merely "does not currently print" a secret is one edit away from doing
// so; a template whose data cannot carry one has to be re-typed first.
//
// ★ Name IS rendered, on the success branch only. It is the most identifying
// value here and it is deliberate: the whole point of this flow is that the
// device joined a PARTICULAR identity, and a page that says only "done" leaves
// a user who has two same-name accounts unable to tell which one they just
// merged into. The name is also already in the mail they clicked from, so it
// discloses nothing the reader was not shown a moment ago — unlike the uid
// (an internal handle they cannot act on) and the recovery code (a password).
type verifyPageData struct {
	OK      bool
	Code    string
	Heading string
	Message string
	Name    string
	AppURL  string
}

// verifyPageTmpl is parsed once, at init, and template.Must is deliberate: a
// broken template must kill the process at startup rather than surface as a
// blank page on the one request a user cannot repeat (the link is single-use).
var verifyPageTmpl = template.Must(template.New("verify").Parse(verifyPageHTML))

const verifyPageHTML = `<!DOCTYPE html>
<html lang="zh-Hant">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Heading}}｜行程統合</title>
<style>
:root { color-scheme: light dark; }
body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
  font-family: system-ui, -apple-system, "Noto Sans TC", sans-serif;
  background:#f4f5f7; color:#1c1e21; padding:24px; box-sizing:border-box; }
.card { background:#fff; border-radius:16px; padding:32px 28px; max-width:26rem; width:100%;
  box-shadow:0 8px 32px rgba(0,0,0,.08); text-align:center; }
.mark { font-size:44px; line-height:1; margin-bottom:12px; }
h1 { font-size:20px; margin:0 0 12px; }
p { margin:0 0 20px; line-height:1.7; color:#4b4f56; font-size:15px; }
.who { margin-bottom:8px; }
a.btn { display:inline-block; padding:12px 24px; border-radius:10px; text-decoration:none;
  background:#2f6fed; color:#fff; font-size:15px; }
.fail .mark { color:#c0392b; }
@media (prefers-color-scheme: dark) {
  body { background:#17181a; color:#e8e9ea; }
  .card { background:#212225; box-shadow:none; }
  p { color:#a9adb3; }
}
</style>
</head>
<body data-code="{{.Code}}" class="{{if not .OK}}fail{{end}}">
<div class="card">
  <div class="mark">{{if .OK}}&#10003;{{else}}&#9888;{{end}}</div>
  <h1>{{.Heading}}</h1>
  {{if .Name}}<p class="who">已經接上的身分：<strong>{{.Name}}</strong></p>{{end}}
  <p>{{.Message}}</p>
  <a class="btn" href="{{.AppURL}}">回到行程統合</a>
</div>
</body>
</html>
`

// renderVerifyPage writes the browser-facing answer.
//
// ★ The status code is kept as-is. A page is a different representation of the
// same outcome, not a different outcome — answering 200 with an error page
// would make a refused link indistinguishable from an accepted one to
// everything that reads status (logs, probes, the proxy's access log).
func (a *api) renderVerifyPage(w http.ResponseWriter, status int, d verifyPageData) {
	d.AppURL = appURL(a.base)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A magic-link result must never be cached: the link is single-use, so a
	// cached "認證完成" would be shown to the next person to open the URL while
	// the server refuses it.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := verifyPageTmpl.Execute(w, d); err != nil {
		// The header is already written; there is nothing left to say to the
		// client. Log loudly so a template failure is not silent.
		log.Printf("verify-confirm: rendering the result page failed: %v", err)
	}
}

// appURL is the in-app landing target, relative to this deployment's prefix.
func appURL(base string) string {
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		return "/"
	}
	return base + "/"
}
