package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/hiddeco/sshsig"
	"golang.org/x/crypto/ssh"
)

const (
	publishTok = "publish-token-0123456789"
	viewTok    = "view-token-0123456789abc"
)

type fixture struct {
	t      *testing.T
	s      *Server
	ts     *httptest.Server
	cfg    *Config
	signer ssh.Signer
	rsa    ssh.Signer
	clock  time.Time
}

func newFixture(t *testing.T, mutate ...func(*Config)) *fixture {
	t.Helper()
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaSigner, _ := ssh.NewSignerFromKey(rsaKey)
	keys := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	keys = strings.TrimSpace(keys) + " laptop\n# a comment\n" + string(ssh.MarshalAuthorizedKey(rsaSigner.PublicKey()))
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := &Config{
		BaseURL:           "http://example.test",
		DataDir:           filepath.Join(dir, "data"),
		PublishKeysFile:   write("keys", keys),
		PublishTokensFile: write("ptok", "ci:"+publishTok+"\n"),
		ViewTokensFile:    write("vtok", "# viewers\nphone:"+viewTok+"\n"),
		AllowedEmails:     map[string]bool{"me@example.com": true},
		SessionSecret:     []byte(strings.Repeat("s", 32)),
	}
	for _, m := range mutate {
		m(cfg)
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, s: s, cfg: cfg, signer: signer, clock: time.Unix(1_800_000_000, 0)}
	s.now = func() time.Time { return f.clock }
	f.ts = httptest.NewServer(s.Routes())
	t.Cleanup(f.ts.Close)
	f.rsa = rsaSigner
	return f
}

func body(project string) []byte {
	b, _ := json.Marshal(map[string]any{
		"project": project,
		"html":    "<title>" + project + " Backlog</title><p>page</p>",
		"data": map[string]any{
			"states":    []string{"To Do", "Done"},
			"generated": "2026-11-18T00:00:00Z",
			"tasks":     []map[string]string{{"state": "To Do"}, {"state": "Done"}, {"state": "Done"}},
		},
	})
	return b
}

func (f *fixture) signedReq(method, slug string, b []byte, signer ssh.Signer, ts int64, signSlug string) *http.Request {
	tsStr := strconv.FormatInt(ts, 10)
	sig, err := sshsig.Sign(bytes.NewReader(signedMessage(method, signSlug, tsStr, b)), signer, sshsig.HashSHA512, sigNamespace)
	if err != nil {
		f.t.Fatal(err)
	}
	req, _ := http.NewRequest(method, f.ts.URL+"/api/projects/"+slug, bytes.NewReader(b))
	req.Header.Set(headerSigTime, tsStr)
	req.Header.Set(headerSig, base64.StdEncoding.EncodeToString(sshsig.Armor(sig)))
	return req
}

func do(t *testing.T, c *http.Client, req *http.Request) (*http.Response, string) {
	t.Helper()
	if c == nil {
		c = noRedirect()
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func noRedirect() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func get(t *testing.T, c *http.Client, u string, hdr ...string) (*http.Response, string) {
	req, _ := http.NewRequest("GET", u, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return do(t, c, req)
}

func TestSSHPublish(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Unix()
	b := body("Demo")

	resp, out := do(t, nil, f.signedReq("PUT", "demo", b, f.signer, now, "demo"))
	if resp.StatusCode != 200 || !strings.Contains(out, "http://example.test/p/demo") {
		t.Fatalf("ed25519 publish: %d %s", resp.StatusCode, out)
	}
	if m, _ := f.s.store.Meta("demo"); m.PublishedBy != "ssh:laptop" || m.Counts["Done"] != 2 || m.Tasks != 3 {
		t.Fatalf("meta: %+v", m)
	}
	resp, out = do(t, nil, f.signedReq("PUT", "demo", b, f.rsa, now, "demo"))
	if resp.StatusCode != 200 {
		t.Fatalf("rsa publish: %d %s", resp.StatusCode, out)
	}

	cases := []struct {
		name string
		req  *http.Request
		want string
	}{
		{"replay", nil, "already used"},
		{"stale", f.signedReq("PUT", "demo", b, f.signer, now-int64(6*60), "demo"), "away from the server clock"},
		{"wrong slug", f.signedReq("PUT", "other", b, f.signer, now+1, "demo"), "does not match"},
	}
	// Replay: send the exact same request twice.
	first := f.signedReq("PUT", "demo", b, f.signer, now+3, "demo")
	raw := first.Header.Clone()
	do(t, nil, first)
	again, _ := http.NewRequest("PUT", f.ts.URL+"/api/projects/demo", bytes.NewReader(b))
	again.Header = raw
	cases[0].req = again

	// Changed body with a signature over the original body.
	tampered := f.signedReq("PUT", "demo", b, f.signer, now+4, "demo")
	tampered.Body = io.NopCloser(bytes.NewReader(body("Evil")))
	tampered.ContentLength = int64(len(body("Evil")))
	cases = append(cases, struct {
		name string
		req  *http.Request
		want string
	}{"changed body", tampered, "does not match"})

	// A key not in authorized_keys.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(otherPriv)
	cases = append(cases, struct {
		name string
		req  *http.Request
		want string
	}{"unlisted key", f.signedReq("PUT", "demo", b, other, now+5, "demo"), "not in the publish authorized_keys"})

	for _, c := range cases {
		resp, out := do(t, nil, c.req)
		if resp.StatusCode != 401 || !strings.Contains(out, c.want) {
			t.Errorf("%s: %d %s", c.name, resp.StatusCode, out)
		}
	}

	// A PUT signature replayed as DELETE fails.
	put := f.signedReq("PUT", "demo", nil, f.signer, now+6, "demo")
	del, _ := http.NewRequest("DELETE", f.ts.URL+"/api/projects/demo", nil)
	del.Header = put.Header.Clone()
	if resp, out := do(t, nil, del); resp.StatusCode != 401 {
		t.Errorf("PUT signature used for DELETE: %d %s", resp.StatusCode, out)
	}
	if resp, out := do(t, nil, f.signedReq("DELETE", "demo", nil, f.signer, now+7, "demo")); resp.StatusCode != 204 {
		t.Errorf("signed delete: %d %s", resp.StatusCode, out)
	}
}

func TestBearerPublishAndViewTokens(t *testing.T) {
	f := newFixture(t)
	put := func(tok string) (*http.Response, string) {
		req, _ := http.NewRequest("PUT", f.ts.URL+"/api/projects/demo", bytes.NewReader(body("Demo")))
		req.Header.Set("Authorization", "Bearer "+tok)
		return do(t, nil, req)
	}
	if resp, out := put(publishTok); resp.StatusCode != 200 {
		t.Fatalf("publish token: %d %s", resp.StatusCode, out)
	}
	if resp, _ := put(viewTok); resp.StatusCode != 401 {
		t.Fatalf("view token must not publish: %d", resp.StatusCode)
	}
	if resp, _ := get(t, nil, f.ts.URL+"/api/projects", "Authorization", "Bearer "+publishTok); resp.StatusCode != 401 {
		t.Fatalf("publish token must not view: %d", resp.StatusCode)
	}
	resp, out := get(t, nil, f.ts.URL+"/api/projects/demo", "Authorization", "Bearer "+viewTok)
	if resp.StatusCode != 200 || !strings.Contains(out, `"tasks": 3`) || !strings.Contains(out, `"generated"`) {
		t.Fatalf("api project: %d %s", resp.StatusCode, out)
	}
	if resp, _ := put("x"); resp.StatusCode != 401 {
		t.Fatal("bad token accepted")
	}
	req, _ := http.NewRequest("PUT", f.ts.URL+"/api/projects/Bad_Slug", bytes.NewReader(body("x")))
	req.Header.Set("Authorization", "Bearer "+publishTok)
	if resp, _ := do(t, nil, req); resp.StatusCode != 400 {
		t.Fatalf("bad slug: %d", resp.StatusCode)
	}
}

func TestKeyLinkSessionAndPages(t *testing.T) {
	f := newFixture(t)
	req, _ := http.NewRequest("PUT", f.ts.URL+"/api/projects/demo", bytes.NewReader(body("Demo")))
	req.Header.Set("Authorization", "Bearer "+publishTok)
	do(t, nil, req)

	c := noRedirect()
	if resp, out := get(t, c, f.ts.URL+"/"); resp.StatusCode != 401 || !strings.Contains(out, "key link") {
		t.Fatalf("anonymous list: %d", resp.StatusCode)
	}
	if resp, _ := get(t, c, f.ts.URL+"/p/demo?key=wrong"); resp.StatusCode != 401 {
		t.Fatalf("bad key: %d", resp.StatusCode)
	}
	resp, _ := get(t, c, f.ts.URL+"/p/demo?key="+viewTok+"&x=1")
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/p/demo?x=1" {
		t.Fatalf("key link: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, out := get(t, c, f.ts.URL+"/p/demo")
	if resp.StatusCode != 200 || !strings.HasPrefix(out, "<!doctype html>") || !strings.Contains(out, "<p>page</p>") {
		t.Fatalf("page: %d %s", resp.StatusCode, out)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'none'") {
		t.Fatalf("csp: %q", csp)
	}
	resp, out = get(t, c, f.ts.URL+"/")
	if resp.StatusCode != 200 || !strings.Contains(out, `href="/p/demo"`) || !strings.Contains(out, "Done 2") {
		t.Fatalf("list: %d %s", resp.StatusCode, out)
	}

	// Removing the token from the file signs the browser out.
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(f.cfg.ViewTokensFile, []byte("other:another-view-token-123\n"), 0o600)
	os.Chtimes(f.cfg.ViewTokensFile, time.Now().Add(time.Second), time.Now().Add(time.Second))
	if resp, _ := get(t, c, f.ts.URL+"/"); resp.StatusCode != 401 {
		t.Fatalf("revoked key still works: %d", resp.StatusCode)
	}
}

func TestSessionCookieTamperAndExpiry(t *testing.T) {
	f := newFixture(t)
	good := f.s.sign(session{Kind: "key", Sub: "phone", Exp: f.clock.Add(time.Hour).Unix()})
	try := func(v string) int {
		resp, _ := get(t, nil, f.ts.URL+"/api/projects", "Cookie", sessionCookie+"="+v)
		return resp.StatusCode
	}
	if code := try(good); code != 200 {
		t.Fatalf("good cookie: %d", code)
	}
	forged := f.s.sign(session{Kind: "google", Sub: "me@example.com", Exp: f.clock.Add(time.Hour).Unix()})
	_, m, _ := strings.Cut(forged, ".")
	evil := base64.RawURLEncoding.EncodeToString([]byte(`{"k":"key","s":"phone","e":9999999999}`)) + "." + m
	if code := try(evil); code != 401 {
		t.Fatalf("tampered cookie: %d", code)
	}
	f.clock = f.clock.Add(2 * time.Hour)
	if code := try(good); code != 401 {
		t.Fatalf("expired cookie: %d", code)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"": "/", "/p/x": "/p/x", "//evil.com": "/", "https://evil.com": "/", "/\\evil.com": "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
	for _, s := range []string{"a", "my-project", "a1"} {
		if !validSlug(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "-a", "a-", "A", "a_b", "../x", strings.Repeat("a", 65)} {
		if validSlug(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

// ---- Google sign-in against a fake OpenID provider ----

type fakeIDP struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	email string
	vfy   bool
	nonce string // overrides the nonce from the auth request when set
	aud   string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	p := &fakeIDP{vfy: true}
	p.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	codes := map[string]string{}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/auth", "token_endpoint": p.srv.URL + "/token",
			"jwks_uri": p.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := randomString()
		codes[code] = q.Get("nonce")
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		nonce := codes[r.Form.Get("code")]
		if p.nonce != "" {
			nonce = p.nonce
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
		claims, _ := json.Marshal(map[string]any{
			"iss": p.srv.URL, "sub": "123", "aud": p.aud, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
			"nonce": nonce, "email": p.email, "email_verified": p.vfy,
		})
		w.Header().Set("Content-Type", "application/json")
		jws, _ := signer.Sign(claims)
		idt, _ := jws.CompactSerialize()
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idt, "expires_in": 3600})
	})
	return p
}

func TestGoogleSignIn(t *testing.T) {
	idp := newFakeIDP(t)
	f := newFixture(t, func(c *Config) {
		c.OIDCIssuer = idp.srv.URL
		c.GoogleClientID = "client-id"
		c.GoogleClientSecret = "secret"
	})
	idp.aud = "client-id"
	f.s.now = time.Now // the ID token uses real time
	f.cfg.BaseURL = f.ts.URL

	login := func() (*http.Client, *http.Response, string) {
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar}
		resp, out := get(t, c, f.ts.URL+"/auth/login?next=/p/demo")
		return c, resp, out
	}

	idp.email = "me@example.com"
	c, resp, out := login()
	// Followed redirects end on /p/demo, which doesn't exist yet: 404 means signed in.
	if resp.StatusCode != 404 || resp.Request.URL.Path != "/p/demo" {
		t.Fatalf("allowed email: %d %s %s", resp.StatusCode, resp.Request.URL, out)
	}
	if resp, _ := get(t, c, f.ts.URL+"/"); resp.StatusCode != 200 {
		t.Fatalf("list after sign-in: %d", resp.StatusCode)
	}

	idp.email = "stranger@example.com"
	if _, resp, out := login(); resp.StatusCode != 403 || !strings.Contains(out, "is not allowed") {
		t.Fatalf("other email: %d %s", resp.StatusCode, out)
	}
	idp.email, idp.vfy = "me@example.com", false
	if _, resp, out := login(); resp.StatusCode != 403 || !strings.Contains(out, "not a verified") {
		t.Fatalf("unverified: %d %s", resp.StatusCode, out)
	}
	idp.vfy, idp.nonce = true, "wrong"
	if _, resp, _ := login(); resp.StatusCode != 403 {
		t.Fatalf("nonce mismatch: %d", resp.StatusCode)
	}
	idp.nonce, idp.aud = "", "someone-else"
	if _, resp, _ := login(); resp.StatusCode != 403 {
		t.Fatalf("wrong audience: %d", resp.StatusCode)
	}
}

func TestListControls(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"Zeta", "alpha"} {
		req, _ := http.NewRequest("PUT", f.ts.URL+"/api/projects/"+strings.ToLower(name), bytes.NewReader(body(name)))
		req.Header.Set("Authorization", "Bearer "+publishTok)
		do(t, nil, req)
	}
	resp, out := get(t, nil, f.ts.URL+"/", "Authorization", "Bearer "+viewTok)
	if resp.StatusCode != 200 || !strings.Contains(out, `data-slug="alpha" data-name="alpha" data-tasks="3" data-updated="`) ||
		!strings.Contains(out, `<div class="controls" id="controls" hidden>`) {
		t.Fatalf("list: %d %s", resp.StatusCode, out)
	}
	// The shared controller is embedded, and the CSP allows exactly the script the page carries.
	if !strings.Contains(listScript, "function listControls(o)") {
		t.Fatal("shared list-controls.js is not embedded")
	}
	sum := sha256.Sum256([]byte(listScript))
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") ||
		!strings.Contains(out, "<script>"+listScript+"</script>") {
		t.Fatalf("script not allowed by csp %q", csp)
	}
}
