package auth

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Login lockout: 5 attempts per minute per source address; exceeding the
// window locks the address with exponential backoff (1, 2, 4 … capped at 64
// minutes). Lockout state persists in the state dir so a reboot does not reset
// it — the whole point of the console being reboot-persistent and internet-
// adjacent on a remote box is that brute force must stay futile across restarts.
const (
	attemptsPerWindow = 5
	window            = time.Minute
	lockoutBase       = time.Minute
	lockoutMax        = 64 * time.Minute
)

type ipState struct {
	Attempts     int       // failures in current window
	WindowStart  time.Time
	Streak       int // consecutive lockouts (drives the exponent)
	BlockedUntil time.Time
}

// Limiter gates login attempts per source IP.
type Limiter struct {
	path string // lockout persistence file ("" = in-memory only, for tests)

	mu    sync.Mutex
	state map[string]*ipState
	now   func() time.Time
}

// NewLimiter persists to path when non-empty; a corrupt file is treated as
// empty (fail-open on the *bookkeeping*, never on the gate itself).
func NewLimiter(path string) *Limiter {
	l := &Limiter{path: path, state: make(map[string]*ipState), now: time.Now}
	l.load()
	return l
}

// Allow reports whether an attempt may proceed and, if blocked, when it may next.
func (l *Limiter) Allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state[ip]
	if st == nil {
		return true, 0
	}
	if l.now().Before(st.BlockedUntil) {
		return false, st.BlockedUntil.Sub(l.now())
	}
	return true, 0
}

// Fail records a wrong credential and may open a lockout window.
func (l *Limiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.state[ip]
	if st == nil {
		st = &ipState{}
		l.state[ip] = st
	}
	if now.Sub(st.WindowStart) > window {
		st.WindowStart, st.Attempts = now, 0
	}
	st.Attempts++
	if st.Attempts > attemptsPerWindow {
		d := lockoutBase << min(st.Streak, 6) // 1,2,4,...,64 min
		if d > lockoutMax {
			d = lockoutMax
		}
		st.Streak++
		st.BlockedUntil = now.Add(d)
		st.WindowStart, st.Attempts = now.Add(d), 0
	}
	l.persistLocked()
}

// Success resets the address's failure bookkeeping.
func (l *Limiter) Success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.state[ip]; !ok {
		return
	}
	delete(l.state, ip)
	l.persistLocked()
}

// Blocked reports current lockouts (doctor output / tests).
func (l *Limiter) Blocked(ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state[ip]
	if st == nil {
		return 0, false
	}
	if d := st.BlockedUntil.Sub(l.now()); d > 0 {
		return d, true
	}
	return 0, false
}

func (l *Limiter) load() {
	if l.path == "" {
		return
	}
	b, err := os.ReadFile(l.path)
	if err != nil {
		return
	}
	var persisted map[string]*ipState
	if json.Unmarshal(b, &persisted) != nil {
		return
	}
	for ip, st := range persisted {
		if st != nil && time.Until(st.BlockedUntil) > -window {
			l.state[ip] = st
		}
	}
}

func (l *Limiter) persistLocked() {
	if l.path == "" {
		return
	}
	b, err := json.Marshal(l.state)
	if err != nil {
		return
	}
	tmp := l.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, l.path)
	}
}
