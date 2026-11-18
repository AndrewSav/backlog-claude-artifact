package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

var slugRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)

func validSlug(s string) bool { return slugRE.MatchString(s) }

// ProjectMeta is what the project list and the JSON API show about a project.
type ProjectMeta struct {
	Slug        string         `json:"slug"`
	Project     string         `json:"project"`
	States      []string       `json:"states"`
	Counts      map[string]int `json:"counts"`
	Tasks       int            `json:"tasks"`
	Generated   string         `json:"generated,omitempty"`
	UpdatedAt   time.Time      `json:"updatedAt"`
	PublishedBy string         `json:"publishedBy"`
}

// Store keeps each project in DATA_DIR/projects/<slug>/ as page.html,
// data.json and meta.json. meta.json is written last, so a project shows in
// the list only once its page is complete.
type Store struct {
	dir string
	mu  sync.RWMutex
}

var errNotFound = errors.New("not found")

func NewStore(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "projects")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (s *Store) Put(meta ProjectMeta, page, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.dir, meta.Slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	m, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "page.html"), page); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "data.json"), data); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "meta.json"), m)
}

func (s *Store) read(slug, name string) ([]byte, error) {
	if !validSlug(slug) {
		return nil, errNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := os.ReadFile(filepath.Join(s.dir, slug, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNotFound
	}
	return b, err
}

func (s *Store) Meta(slug string) (ProjectMeta, error) {
	var m ProjectMeta
	b, err := s.read(slug, "meta.json")
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

func (s *Store) Page(slug string) ([]byte, error) {
	if _, err := s.Meta(slug); err != nil {
		return nil, err
	}
	return s.read(slug, "page.html")
}

func (s *Store) Data(slug string) ([]byte, error) {
	if _, err := s.Meta(slug); err != nil {
		return nil, err
	}
	return s.read(slug, "data.json")
}

// List returns every complete project, most recently updated first.
func (s *Store) List() ([]ProjectMeta, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []ProjectMeta
	for _, e := range entries {
		if !e.IsDir() || !validSlug(e.Name()) {
			continue
		}
		if m, err := s.Meta(e.Name()); err == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

func (s *Store) Delete(slug string) error {
	if !validSlug(slug) {
		return errNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.dir, slug)
	if _, err := os.Stat(filepath.Join(dir, "meta.json")); err != nil {
		return errNotFound
	}
	return os.RemoveAll(dir)
}
