package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"sync"
	"time"
)

// Sessions is an in-memory HttpOnly-cookie session table. Tokens are random
// 32 bytes; only their SHA-256 is kept, so a state dump is useless and a serve
// restart invalidates all sessions (documented behaviour — re-login, not reboot
// pain, since reboots are rare for a user of this box and the password is in
// their head).
type Sessions struct {
	mu     sync.Mutex
	table  map[string]time.Time // sha256(token) hex -> idle expiry
	idle   time.Duration
	now    func() time.Time
}

// NewSessions builds a table with the given idle TTL (12 h per plan).
func NewSessions(idle time.Duration) *Sessions {
	return &Sessions{table: make(map[string]time.Time), idle: idle, now: time.Now}
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// Create mints a session and returns the bearer token (cookie value).
func (s *Sessions) Create() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.table[hashToken(tok)] = s.now().Add(s.idle)
	return tok, nil
}

// Validate reports a valid session and slides its idle expiry.
func (s *Sessions) Validate(tok string) bool {
	if tok == "" {
		return false
	}
	key := hashToken(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.table[key]
	if !ok {
		return false
	}
	now := s.now()
	if !now.Before(exp) {
		delete(s.table, key)
		return false
	}
	s.table[key] = now.Add(s.idle)
	return true
}

// Revoke logs one session out.
func (s *Sessions) Revoke(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.table, hashToken(tok))
}

// Len is the live-session count (dashboard "signed in" indicator / tests).
func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	return len(s.table)
}

func (s *Sessions) pruneLocked() {
	now := s.now()
	for k, exp := range s.table {
		if !now.Before(exp) {
			delete(s.table, k)
		}
	}
}

// ConstantTimeEqualString is exported for handler use (bearer compare).
func ConstantTimeEqualString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
