package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const maxPublishBytes = 20 << 20

type Server struct {
	cfg           *Config
	store         *Store
	publishKeys   *watchedFile[keyList]
	publishTokens *watchedFile[tokenList]
	viewTokens    *watchedFile[tokenList]
	replay        *replayCache
	oidc          *oidcAuth // nil when Google sign-in is not configured
	now           func() time.Time
}

func NewServer(cfg *Config) (*Server, error) {
	st, err := NewStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:           cfg,
		store:         st,
		publishKeys:   newWatchedFile(cfg.PublishKeysFile, parseAuthorizedKeys),
		publishTokens: newWatchedFile(cfg.PublishTokensFile, parseTokens),
		viewTokens:    newWatchedFile(cfg.ViewTokensFile, parseTokens),
		replay:        newReplayCache(),
		now:           time.Now,
	}
	if cfg.GoogleClientID != "" {
		s.oidc = &oidcAuth{cfg: cfg}
	}
	return s, nil
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })

	mux.HandleFunc("PUT /api/projects/{slug}", s.handlePublish)
	mux.HandleFunc("DELETE /api/projects/{slug}", s.handleDelete)
	mux.HandleFunc("GET /api/projects", s.requireViewer(s.handleAPIList))
	mux.HandleFunc("GET /api/projects/{slug}", s.requireViewer(s.handleAPIProject))

	mux.HandleFunc("GET /{$}", s.requireViewer(s.handleList))
	mux.HandleFunc("GET /p/{slug}", s.requireViewer(s.handlePage))

	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("GET /auth/logout", s.handleLogout)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ---- publishing ----

type publishRequest struct {
	Project string          `json:"project"`
	HTML    string          `json:"html"`
	Data    json.RawMessage `json:"data"`
}

// summary is the part of the page data the project list needs.
type summary struct {
	States    []string `json:"states"`
	Generated string   `json:"generated"`
	Tasks     []struct {
		State string `json:"state"`
	} `json:"tasks"`
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		jsonError(w, http.StatusBadRequest, "the project slug must be 1-64 lowercase letters, digits or hyphens")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPublishBytes))
	if err != nil {
		jsonError(w, http.StatusRequestEntityTooLarge, "the upload is larger than 20 MB")
		return
	}
	who, err := s.authorizePublish(r, slug, body)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, err.Error())
		return
	}
	var req publishRequest
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, http.StatusBadRequest, "the body is not valid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.HTML) == "" || len(req.Data) == 0 {
		jsonError(w, http.StatusBadRequest, "the body needs html and data")
		return
	}
	var sum summary
	if err := json.Unmarshal(req.Data, &sum); err != nil {
		jsonError(w, http.StatusBadRequest, "data is not the page data object: "+err.Error())
		return
	}
	meta := ProjectMeta{
		Slug: slug, Project: req.Project, States: sum.States, Counts: map[string]int{},
		Tasks: len(sum.Tasks), Generated: sum.Generated, UpdatedAt: s.now().UTC(), PublishedBy: who,
	}
	if meta.Project == "" {
		meta.Project = slug
	}
	for _, t := range sum.Tasks {
		meta.Counts[t.State]++
	}
	if err := s.store.Put(meta, []byte(req.HTML), req.Data); err != nil {
		log.Printf("publish %s: %v", slug, err)
		jsonError(w, http.StatusInternalServerError, "the server could not save the page")
		return
	}
	log.Printf("published %s (%d tasks, %d bytes) by %s", slug, meta.Tasks, len(req.HTML), who)
	writeJSON(w, http.StatusOK, map[string]string{"url": s.cfg.BaseURL + "/p/" + slug})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	who, err := s.authorizePublish(r, slug, body)
	if err != nil {
		jsonError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err := s.store.Delete(slug); err != nil {
		if errors.Is(err, errNotFound) {
			jsonError(w, http.StatusNotFound, "no such project")
			return
		}
		jsonError(w, http.StatusInternalServerError, "the server could not delete the project")
		return
	}
	log.Printf("deleted %s by %s", slug, who)
	w.WriteHeader(http.StatusNoContent)
}

// ---- viewing ----

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.List()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "the server could not read its projects")
		return
	}
	if list == nil {
		list = []ProjectMeta{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleAPIProject(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	meta, err := s.store.Meta(slug)
	if err != nil {
		jsonError(w, http.StatusNotFound, "no such project")
		return
	}
	data, err := s.store.Data(slug)
	if err != nil {
		jsonError(w, http.StatusNotFound, "no such project")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		ProjectMeta
		Data json.RawMessage `json:"data"`
	}{meta, data})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.List()
	if err != nil {
		http.Error(w, "the server could not read its projects", http.StatusInternalServerError)
		return
	}
	s.renderList(w, viewerFrom(r), list)
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	page, err := s.store.Page(r.PathValue("slug"))
	if err != nil {
		w.Header().Set("Content-Security-Policy", chromeCSP)
		w.WriteHeader(http.StatusNotFound)
		renderMessage(w, "Not found", "No backlog has been published under that name.")
		return
	}
	writePage(w, page)
}
