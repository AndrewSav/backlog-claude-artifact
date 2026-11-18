package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hiddeco/sshsig"
	"golang.org/x/crypto/ssh"
)

// SSH publish signatures: the client signs
//
//	backlog-publish\n<METHOD>\n<slug>\n<unix seconds>\n<sha256 hex of body>
//
// with `ssh-keygen -Y sign -n backlog-artifact` (the private key normally
// stays in ssh-agent) and sends the armored signature, base64-encoded, in
// X-Backlog-Signature, with the timestamp in X-Backlog-Timestamp. The server
// checks the signature against the public keys in its authorized_keys file.
const (
	sigNamespace  = "backlog-artifact"
	sigMaxSkew    = 5 * time.Minute
	headerSig     = "X-Backlog-Signature"
	headerSigTime = "X-Backlog-Timestamp"
)

func signedMessage(method, slug, ts string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte("backlog-publish\n" + method + "\n" + slug + "\n" + ts + "\n" + hex.EncodeToString(sum[:]))
}

// replayCache remembers signatures seen within the allowed clock skew, so a
// captured request cannot be sent again.
type replayCache struct {
	mu   sync.Mutex
	seen map[[32]byte]time.Time
}

func newReplayCache() *replayCache { return &replayCache{seen: map[[32]byte]time.Time{}} }

// Use records sig and reports whether it was new.
func (c *replayCache) Use(sig []byte, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, t := range c.seen {
		if now.Sub(t) > 2*sigMaxSkew+time.Minute {
			delete(c.seen, k)
		}
	}
	h := sha256.Sum256(sig)
	if _, dup := c.seen[h]; dup {
		return false
	}
	c.seen[h] = now
	return true
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// authorizePublish returns a label for who published, or an error that is
// safe to show to the client.
func (s *Server) authorizePublish(r *http.Request, slug string, body []byte) (string, error) {
	if tok := bearer(r); tok != "" {
		if name, ok := s.publishTokens.Get().Match(tok); ok {
			return "token:" + name, nil
		}
		return "", errors.New("the bearer token is not a publish token")
	}
	armoredB64 := r.Header.Get(headerSig)
	ts := r.Header.Get(headerSigTime)
	if armoredB64 == "" || ts == "" {
		return "", errors.New("send a publish token as a bearer token, or an SSH signature in " + headerSig + " and " + headerSigTime)
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", errors.New(headerSigTime + " must be unix seconds")
	}
	now := s.now()
	if d := now.Sub(time.Unix(sec, 0)); d > sigMaxSkew || d < -sigMaxSkew {
		return "", fmt.Errorf("the signature timestamp is %s away from the server clock (limit %s)", d.Round(time.Second), sigMaxSkew)
	}
	armored, err := base64.StdEncoding.DecodeString(armoredB64)
	if err != nil {
		return "", errors.New(headerSig + " is not valid base64")
	}
	sig, err := sshsig.Unarmor(armored)
	if err != nil {
		return "", errors.New(headerSig + " is not an SSH signature: " + err.Error())
	}
	key, ok := s.publishKeys.Get().Find(sig.PublicKey)
	if !ok {
		return "", fmt.Errorf("the key %s is not in the publish authorized_keys file", ssh.FingerprintSHA256(sig.PublicKey))
	}
	msg := signedMessage(r.Method, slug, ts, body)
	if err := sshsig.Verify(bytes.NewReader(msg), sig, key.key, sig.HashAlgorithm, sigNamespace); err != nil {
		return "", errors.New("the SSH signature does not match this request: " + err.Error())
	}
	if !s.replay.Use(armored, now) {
		return "", errors.New("this signature was already used")
	}
	label := key.comment
	if label == "" {
		label = ssh.FingerprintSHA256(key.key)
	}
	return "ssh:" + label, nil
}
