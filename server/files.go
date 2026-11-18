package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// watchedFile re-reads and re-parses a config file whenever its mtime or size
// changes, so keys and tokens can be added without a restart. A missing file
// counts as empty.
type watchedFile[T any] struct {
	path  string
	parse func([]byte) (T, error)

	mu      sync.Mutex
	loaded  bool // value reflects the file as of mtime/size (or its absence)
	missing bool
	mtime   time.Time
	size    int64
	value   T
}

func newWatchedFile[T any](path string, parse func([]byte) (T, error)) *watchedFile[T] {
	return &watchedFile[T]{path: path, parse: parse}
}

func (w *watchedFile[T]) Get() T {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, err := os.Stat(w.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("%s: %v", w.path, err)
		}
		if !w.loaded || !w.missing {
			var zero T
			w.value, w.loaded, w.missing = zero, true, true
		}
		return w.value
	}
	if w.loaded && !w.missing && st.ModTime().Equal(w.mtime) && st.Size() == w.size {
		return w.value
	}
	data, err := os.ReadFile(w.path)
	if err != nil {
		log.Printf("%s: %v", w.path, err)
		return w.value
	}
	v, err := w.parse(data)
	if err != nil {
		log.Printf("%s: %v (keeping the previous contents)", w.path, err)
		return w.value
	}
	w.value, w.mtime, w.size, w.loaded, w.missing = v, st.ModTime(), st.Size(), true, false
	return w.value
}

// ---- tokens ----

type token struct {
	name string
	hash [32]byte
}

type tokenList []token

// parseTokens reads one token per line, optionally labelled as "name:token".
// Blank lines and lines starting with # are ignored.
func parseTokens(data []byte) (tokenList, error) {
	var out tokenList
	sc := bufio.NewScanner(bytes.NewReader(data))
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, tok := "", line
		if i := strings.Index(line, ":"); i >= 0 {
			name, tok = strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		}
		if len(tok) < 16 {
			log.Printf("token on line %d is shorter than 16 characters and was ignored", n)
			continue
		}
		if name == "" {
			name = "token#" + strconv.Itoa(n)
		}
		out = append(out, token{name: name, hash: sha256.Sum256([]byte(tok))})
	}
	return out, sc.Err()
}

// Match returns the token's name. Every entry is compared, in constant time.
func (l tokenList) Match(candidate string) (string, bool) {
	if candidate == "" {
		return "", false
	}
	h := sha256.Sum256([]byte(candidate))
	found := ""
	for _, t := range l {
		if subtle.ConstantTimeCompare(h[:], t.hash[:]) == 1 {
			found = t.name
		}
	}
	return found, found != ""
}

func (l tokenList) HasName(name string) bool {
	for _, t := range l {
		if t.name == name {
			return true
		}
	}
	return false
}

// ---- SSH keys ----

type authorizedKey struct {
	key     ssh.PublicKey
	comment string
}

type keyList []authorizedKey

func parseAuthorizedKeys(data []byte) (keyList, error) {
	var out keyList
	for i, line := range bytes.Split(data, []byte("\n")) {
		t := bytes.TrimSpace(line)
		if len(t) == 0 || t[0] == '#' {
			continue
		}
		pub, comment, _, _, err := ssh.ParseAuthorizedKey(t)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		out = append(out, authorizedKey{key: pub, comment: comment})
	}
	return out, nil
}

func (l keyList) Find(pub ssh.PublicKey) (authorizedKey, bool) {
	want := pub.Marshal()
	for _, k := range l {
		if bytes.Equal(k.key.Marshal(), want) {
			return k, true
		}
	}
	return authorizedKey{}, false
}
