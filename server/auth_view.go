package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "backlog_session"
	oauthCookie   = "backlog_oauth"
	sessionTTL    = 30 * 24 * time.Hour
)

// ---- signed values (session and OAuth state cookies) ----

func (s *Server) sign(v any) string {
	payload, _ := json.Marshal(v)
	mac := hmac.New(sha256.New, s.cfg.SessionSecret)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) unsign(value string, v any) bool {
	p, m, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	sum, err2 := base64.RawURLEncoding.DecodeString(m)
	if err1 != nil || err2 != nil {
		return false
	}
	mac := hmac.New(sha256.New, s.cfg.SessionSecret)
	mac.Write(payload)
	if !hmac.Equal(sum, mac.Sum(nil)) {
		return false
	}
	return json.Unmarshal(payload, v) == nil
}

// session is who a browser is signed in as. Kind "google" carries an email,
// kind "key" carries the name of the view token used. Both are re-checked
// against the current allowlist and token file on every request, so removing
// an email or a token signs that viewer out.
type session struct {
	Kind string `json:"k"`
	Sub  string `json:"s"`
	Exp  int64  `json:"e"`
}

func (s *Server) setCookie(w http.ResponseWriter, name, value, path string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: int(maxAge.Seconds()),
		HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) startSession(w http.ResponseWriter, kind, sub string) {
	s.setCookie(w, sessionCookie, s.sign(session{Kind: kind, Sub: sub, Exp: s.now().Add(sessionTTL).Unix()}), "/", sessionTTL)
}

type viewer struct {
	Kind  string // "google", "key" or "bearer"
	Label string
}

func (s *Server) currentViewer(r *http.Request) (viewer, bool) {
	if tok := bearer(r); tok != "" {
		if name, ok := s.viewTokens.Get().Match(tok); ok {
			return viewer{"bearer", name}, true
		}
		return viewer{}, false
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return viewer{}, false
	}
	var sess session
	if !s.unsign(c.Value, &sess) || s.now().Unix() > sess.Exp {
		return viewer{}, false
	}
	switch sess.Kind {
	case "google":
		if s.oidc != nil && s.cfg.AllowedEmails[strings.ToLower(sess.Sub)] {
			return viewer{"google", sess.Sub}, true
		}
	case "key":
		if s.viewTokens.Get().HasName(sess.Sub) {
			return viewer{"key", sess.Sub}, true
		}
	}
	return viewer{}, false
}

type viewerKey struct{}

func viewerFrom(r *http.Request) viewer {
	v, _ := r.Context().Value(viewerKey{}).(viewer)
	return v
}

// requireViewer lets through signed-in viewers and view-token holders. A
// ?key=<view token> query signs the browser in and redirects to the same URL
// without the key, so the key doesn't stay in the address bar or history.
func (s *Server) requireViewer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Has("key") {
			name, ok := s.viewTokens.Get().Match(q.Get("key"))
			if !ok {
				s.renderLogin(w, r, http.StatusUnauthorized, "That key link is not valid.")
				return
			}
			s.startSession(w, "key", name)
			q.Del("key")
			u := *r.URL
			u.RawQuery = q.Encode()
			http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
			return
		}
		v, ok := s.currentViewer(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "send a view token as a bearer token"})
				return
			}
			s.renderLogin(w, r, http.StatusUnauthorized, "")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), viewerKey{}, v)))
	}
}

// ---- Google sign-in (OpenID Connect) ----

type oidcAuth struct {
	cfg *Config

	mu       sync.Mutex
	provider *oidc.Provider
}

// setup discovers the provider on first use rather than at startup, so the
// server starts even when Google can't be reached.
func (o *oidcAuth) setup(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.provider == nil {
		p, err := oidc.NewProvider(ctx, o.cfg.OIDCIssuer)
		if err != nil {
			return nil, nil, err
		}
		o.provider = p
	}
	oc := &oauth2.Config{
		ClientID:     o.cfg.GoogleClientID,
		ClientSecret: o.cfg.GoogleClientSecret,
		Endpoint:     o.provider.Endpoint(),
		RedirectURL:  o.cfg.BaseURL + "/auth/callback",
		Scopes:       []string{oidc.ScopeOpenID, "email"},
	}
	return oc, o.provider.Verifier(&oidc.Config{ClientID: o.cfg.GoogleClientID}), nil
}

type oauthState struct {
	State string `json:"st"`
	Nonce string `json:"n"`
	Next  string `json:"nx"`
	Exp   int64  `json:"e"`
}

func randomString() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// safeNext keeps post-login redirects on this site.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Host != "" || u.Scheme != "" {
		return "/"
	}
	return next
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		s.renderLogin(w, r, http.StatusNotFound, "Google sign-in is not configured on this server.")
		return
	}
	oc, _, err := s.oidc.setup(r.Context())
	if err != nil {
		s.renderLogin(w, r, http.StatusBadGateway, "Google sign-in is unavailable right now: "+err.Error())
		return
	}
	st := oauthState{State: randomString(), Nonce: randomString(), Next: safeNext(r.URL.Query().Get("next")), Exp: s.now().Add(10 * time.Minute).Unix()}
	s.setCookie(w, oauthCookie, s.sign(st), "/auth/", 10*time.Minute)
	http.Redirect(w, r, oc.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.SetAuthURLParam("prompt", "select_account")), http.StatusFound)
}

func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		http.NotFound(w, r)
		return
	}
	email, next, err := s.completeLogin(r)
	s.setCookie(w, oauthCookie, "", "/auth/", -time.Second)
	if err != nil {
		s.renderLogin(w, r, http.StatusForbidden, err.Error())
		return
	}
	s.startSession(w, "google", email)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) completeLogin(r *http.Request) (email, next string, err error) {
	if e := r.URL.Query().Get("error"); e != "" {
		return "", "", errors.New("Google sign-in was cancelled (" + e + ").")
	}
	c, cerr := r.Cookie(oauthCookie)
	var st oauthState
	if cerr != nil || !s.unsign(c.Value, &st) || s.now().Unix() > st.Exp {
		return "", "", errors.New("The sign-in attempt expired. Try again.")
	}
	if r.URL.Query().Get("state") != st.State {
		return "", "", errors.New("The sign-in response did not match this browser. Try again.")
	}
	oc, verifier, err := s.oidc.setup(r.Context())
	if err != nil {
		return "", "", errors.New("Google sign-in is unavailable right now.")
	}
	tok, err := oc.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		return "", "", errors.New("Google did not accept the sign-in code. Try again.")
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := verifier.Verify(r.Context(), raw)
	if err != nil {
		return "", "", errors.New("Google's sign-in token did not verify.")
	}
	if idt.Nonce != st.Nonce {
		return "", "", errors.New("The sign-in response did not match this browser. Try again.")
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := idt.Claims(&claims); err != nil || claims.Email == "" {
		return "", "", errors.New("Google did not share an email address.")
	}
	if !claims.EmailVerified {
		return "", "", errors.New(claims.Email + " is not a verified Google address.")
	}
	if !s.cfg.AllowedEmails[strings.ToLower(claims.Email)] {
		return "", "", errors.New(claims.Email + " is not allowed to view these backlogs. Ask the owner to add it, or sign in with another account.")
	}
	return claims.Email, st.Next, nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.setCookie(w, sessionCookie, "", "/", -time.Second)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
