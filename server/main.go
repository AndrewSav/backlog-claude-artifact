// backlog-server stores and serves Backlog.md pages published by the
// backlog-artifact Claude Code plugin.
//
// Publishing is authorised by an SSH signature (keys in an authorized_keys
// file) or a static publish token. Viewing is authorised by Google sign-in
// (allowlisted emails) or a static view token.
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen             string
	BaseURL            string
	DataDir            string
	PublishKeysFile    string
	PublishTokensFile  string
	ViewTokensFile     string
	AllowedEmails      map[string]bool
	GoogleClientID     string
	GoogleClientSecret string
	OIDCIssuer         string
	SessionSecret      []byte
	SecureCookies      bool
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil {
		return n
	}
	return def
}

func loadConfig() (*Config, error) {
	c := &Config{
		Listen:             env("LISTEN", ":8080"),
		BaseURL:            strings.TrimRight(env("BASE_URL", "http://localhost:8080"), "/"),
		DataDir:            env("DATA_DIR", "/data"),
		PublishKeysFile:    env("PUBLISH_AUTHORIZED_KEYS_FILE", "/config/publish_authorized_keys"),
		PublishTokensFile:  env("PUBLISH_TOKENS_FILE", "/config/publish_tokens"),
		ViewTokensFile:     env("VIEW_TOKENS_FILE", "/config/view_tokens"),
		AllowedEmails:      map[string]bool{},
		GoogleClientID:     env("GOOGLE_CLIENT_ID", ""),
		GoogleClientSecret: env("GOOGLE_CLIENT_SECRET", ""),
		OIDCIssuer:         env("OIDC_ISSUER", "https://accounts.google.com"),
		SessionSecret:      []byte(env("SESSION_SECRET", "")),
	}
	c.SecureCookies = strings.HasPrefix(c.BaseURL, "https://")
	for _, e := range strings.FieldsFunc(env("VIEW_ALLOWED_EMAILS", ""), func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		c.AllowedEmails[strings.ToLower(e)] = true
	}
	if len(c.SessionSecret) < 32 {
		return nil, errors.New("SESSION_SECRET must be at least 32 characters (try: openssl rand -hex 32)")
	}
	if (c.GoogleClientID == "") != (c.GoogleClientSecret == "") {
		return nil, errors.New("set both GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET, or neither")
	}
	return c, nil
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := dropPrivileges(cfg.DataDir); err != nil {
		log.Fatalf("startup: %v", err)
	}
	s, err := NewServer(cfg)
	if err != nil {
		log.Fatalf("startup: %v", err)
	}
	google := "off"
	if s.oidc != nil {
		google = fmt.Sprintf("on (%d allowed emails)", len(cfg.AllowedEmails))
	}
	log.Printf("backlog-server listening on %s, base %s, data %s, Google sign-in %s", cfg.Listen, cfg.BaseURL, cfg.DataDir, google)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}
