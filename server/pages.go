package main

import (
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// pageCSP applies to published backlog pages. They are self-contained: inline
// script and style, no network access.
const pageCSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; " +
	"connect-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// chromeCSP applies to the server's own pages (project list, sign-in).
const chromeCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// The plugin renders the page as an Artifact body, without a document
// skeleton; this is the same skeleton the Artifact host wraps it in.
const pageHead = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<style>
:root{color-scheme:light;padding-top:env(safe-area-inset-top,0px);padding-bottom:env(safe-area-inset-bottom,0px)}
body{margin:0;font:14px/1.4 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
img{max-width:100%}
[hidden]{display:none!important}
</style>
</head>
<body>
`

func writePage(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", pageCSP)
	w.Write([]byte(pageHead))
	w.Write(body)
	w.Write([]byte("\n</body>\n</html>\n"))
}

const chromeStyle = `
:root {
  --bg: #f6f7f9; --surface: #ffffff; --fg: #1b2230; --muted: #5d6779; --line: #dde1e8;
  --accent: #2f5bd3; --danger: #b4472f;
  --font-ui: system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
  color-scheme: light;
}
@media (prefers-color-scheme: dark) {
  :root { --bg: #12151b; --surface: #1a1f28; --fg: #e4e8ef; --muted: #98a2b3; --line: #2b323e;
          --accent: #7c9cf5; --danger: #ec8a73; color-scheme: dark; }
}
* { box-sizing: border-box; }
html { padding-top: env(safe-area-inset-top, 0px); padding-bottom: env(safe-area-inset-bottom, 0px); }
body { margin: 0; background: var(--bg); color: var(--fg); font: 15px/1.45 var(--font-ui); }
.wrap { max-width: 760px; margin: 0 auto; padding-block: 20px 40px;
  padding-left: max(16px, env(safe-area-inset-left, 0px)); padding-right: max(16px, env(safe-area-inset-right, 0px)); }
h1 { font-size: 1.3rem; margin: 0 0 4px; letter-spacing: -0.01em; }
.sub { color: var(--muted); font-size: 0.85rem; margin: 0 0 18px; }
.sub a { color: var(--accent); }
.list { display: grid; gap: 8px; }
.proj { display: grid; gap: 6px; padding: 12px 14px; background: var(--surface); border: 1px solid var(--line);
  border-radius: 10px; color: inherit; text-decoration: none; min-height: 44px; }
.proj:hover, .proj:focus-visible { border-color: var(--accent); }
:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
.name { font-weight: 600; font-size: 1.02rem; overflow-wrap: anywhere; }
.counts { display: flex; flex-wrap: wrap; gap: 6px; font-size: 0.8rem; color: var(--muted); }
.count { border: 1px solid var(--line); border-radius: 999px; padding: 0 8px; line-height: 1.7;
  white-space: nowrap; font-variant-numeric: tabular-nums; }
.when { font-size: 0.78rem; color: var(--muted); font-variant-numeric: tabular-nums; }
.empty, .msg { color: var(--muted); padding: 24px 0; }
.msg.err { color: var(--danger); }
.btn { display: inline-flex; align-items: center; min-height: 44px; padding: 0 18px; border-radius: 8px;
  background: var(--accent); color: #fff; text-decoration: none; font-weight: 600; }
`

var listTmpl = template.Must(template.New("list").Funcs(template.FuncMap{
	"since": since,
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>Backlogs</title><style>` + chromeStyle + `</style></head>
<body><div class="wrap">
<h1>Backlogs</h1>
<p class="sub">{{len .Projects}} published{{if .Viewer.Label}} · {{if eq .Viewer.Kind "google"}}signed in as {{.Viewer.Label}}{{else}}view key {{.Viewer.Label}}{{end}} · <a href="/auth/logout">Sign out</a>{{end}}</p>
{{if .Projects}}<div class="list">
{{range .Projects}}<a class="proj" href="/p/{{.Slug}}">
  <span class="name">{{.Project}}</span>
  <span class="counts">{{$c := .Counts}}{{range $st := .States}}{{with index $c $st}}<span class="count">{{$st}} {{.}}</span>{{end}}{{end}}</span>
  <span class="when">{{.Tasks}} tasks · updated {{since .UpdatedAt}}</span>
</a>
{{end}}</div>{{else}}<p class="empty">Nothing published yet. Set the plugin's publish target to "server" and end a turn in a repo with a backlog.</p>{{end}}
</div></body></html>`))

var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>Backlogs</title><style>` + chromeStyle + `</style></head>
<body><div class="wrap">
<h1>Backlogs</h1>
{{if .Message}}<p class="msg err">{{.Message}}</p>{{end}}
{{if .Google}}<p class="sub">Sign in to see the published backlogs.</p>
<p><a class="btn" href="/auth/login?next={{.Next}}">Sign in with Google</a></p>
{{else}}<p class="sub">Open the key link you were given to see the published backlogs.</p>{{end}}
</div></body></html>`))

var messageTmpl = template.Must(template.New("msg").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>{{.Title}}</title><style>` + chromeStyle + `</style></head>
<body><div class="wrap"><h1>{{.Title}}</h1><p class="msg">{{.Text}}</p><p class="sub"><a href="/">All backlogs</a></p></div></body></html>`))

func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour") + " ago"
	default:
		return t.Format("2 Jan 2006")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

func (s *Server) renderList(w http.ResponseWriter, v viewer, projects []ProjectMeta) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", chromeCSP)
	// Bearer viewers are API clients; only cookie sessions get a sign-out link.
	if v.Kind == "bearer" {
		v = viewer{}
	}
	listTmpl.Execute(w, map[string]any{"Projects": projects, "Viewer": v})
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", chromeCSP)
	w.WriteHeader(status)
	loginTmpl.Execute(w, map[string]any{
		"Google":  s.oidc != nil,
		"Message": message,
		"Next":    loginNext(r),
	})
}

// loginNext is where to return after signing in: the page that asked for it,
// or the project list when the request came from the sign-in flow itself.
func loginNext(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/auth/") {
		return "/"
	}
	return safeNext(r.URL.Path)
}

func renderMessage(w http.ResponseWriter, title, text string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	messageTmpl.Execute(w, map[string]string{"Title": title, "Text": text})
}
