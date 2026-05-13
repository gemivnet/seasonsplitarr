package qbittorrent

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// session manages SID cookies for the qBit-compatible auth surface. Sessions
// are in-memory and process-local — restarts force Sonarr to re-login, which
// it does automatically on the next request.
type session struct {
	mu       sync.RWMutex
	tokens   map[string]time.Time
	ttl      time.Duration
	username string
	password string
}

func newSession(username, password string) *session {
	return &session{
		tokens:   map[string]time.Time{},
		ttl:      24 * time.Hour,
		username: username,
		password: password,
	}
}

// validate returns true if the supplied creds match the configured pair,
// using constant-time compares so the response time doesn't leak which of
// username/password was wrong.
func (s *session) validate(user, pass string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.password)) == 1
	return userOK && passOK
}

func (s *session) issue() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.tokens[tok] = time.Now().Add(s.ttl)
	s.mu.Unlock()
	return tok
}

func (s *session) revoke(tok string) {
	s.mu.Lock()
	delete(s.tokens, tok)
	s.mu.Unlock()
}

func (s *session) check(r *http.Request) bool {
	c, err := r.Cookie("SID")
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.RLock()
	exp, ok := s.tokens[c.Value]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		s.mu.Lock()
		delete(s.tokens, c.Value)
		s.mu.Unlock()
		return false
	}
	return true
}

// requireAuth wraps a handler so callers without a valid SID cookie are
// rejected. Matches qBittorrent's WebUI behavior — Sonarr already knows how
// to follow the login → cookie flow.
func (s *Shim) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.session.check(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}
