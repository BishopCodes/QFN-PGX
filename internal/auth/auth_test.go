package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(t.TempDir())
	if err := s.EnsureFirstRun("correct-horse-battery-staple"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFirstRunRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if !s.HasPassword() {
		t.Fatal("password should exist")
	}
	if !s.VerifyPassword("correct-horse-battery-staple") {
		t.Fatal("right password rejected")
	}
	if s.VerifyPassword("wrong") {
		t.Fatal("wrong password accepted")
	}
	k, err := s.EngineKey()
	if err != nil || len(k) != 64 {
		t.Fatalf("engine key: len=%d err=%v", len(k), err)
	}
	fi, err := os.Stat(s.keyPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("age.key perms %o, want 600", fi.Mode().Perm())
	}
	fi, _ = os.Stat(s.credPath())
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials.age perms %o, want 600", fi.Mode().Perm())
	}
}

func TestNoStoreIsMissingCredentials(t *testing.T) {
	s := NewStore(t.TempDir())
	if s.HasPassword() {
		t.Fatal("empty store claims a password")
	}
	if _, err := s.EngineKey(); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("want ErrNoCredentials, got %v", err)
	}
}

func TestLooseKeyPermsRefused(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.EnsureFirstRun("pw-please-12345"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.keyPath(), 0o644); err != nil {
		t.Fatal(err)
	}
	s.InvalidateCache()
	if s.VerifyPassword("pw-please-12345") {
		t.Fatal("verify must refuse world-readable age.key")
	}
}

func TestHotReloadOnPasswordChange(t *testing.T) {
	s := newTestStore(t)
	if !s.VerifyPassword("correct-horse-battery-staple") {
		t.Fatal("baseline")
	}
	// Second store models the running server; first models `qfn passwd`.
	server := NewStore(s.dir)
	if !server.VerifyPassword("correct-horse-battery-staple") {
		t.Fatal("server baseline")
	}
	if err := s.SetPassword("new-pw-after-change-9"); err != nil {
		t.Fatal(err)
	}
	// mtime granularity can be 1s on some filesystems; nudge it explicitly.
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(server.credPath(), future, future)
	if !server.VerifyPassword("new-pw-after-change-9") {
		t.Fatal("server did not hot-reload the new password")
	}
}

func TestEngineKeyRotation(t *testing.T) {
	s := newTestStore(t)
	old, _ := s.EngineKey()
	fresh, err := s.RotateEngineKey()
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old || fresh == "" {
		t.Fatal("rotation produced no new key")
	}
	again, _ := s.EngineKey()
	if again != fresh {
		t.Fatal("rotated key not persisted")
	}
}

func TestFrontKeyRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if k, err := s.FrontKey(); err != nil || k != "" {
		t.Fatalf("front key should start empty: %q %v", k, err)
	}
	k, err := s.RotateFrontKey()
	if err != nil || k == "" {
		t.Fatal(err)
	}
	got, _ := s.FrontKey()
	if got != k {
		t.Fatal("front key not persisted")
	}
}

func TestPayloadNotReadableInClear(t *testing.T) {
	s := newTestStore(t)
	b, err := os.ReadFile(s.credPath())
	if err != nil {
		t.Fatal(err)
	}
	// The age container has a recognizable header but never plaintext.
	if len(b) < 10 || string(b[:6]) == `{"v":` {
		t.Fatal("credentials.age appears to be plaintext")
	}
}

func TestSessionsLifecycleAndTTL(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := NewSessions(12 * time.Hour)
	s.now = func() time.Time { return now }

	tok, err := s.Create()
	if err != nil || tok == "" {
		t.Fatal(err)
	}
	if !s.Validate(tok) {
		t.Fatal("fresh token rejected")
	}
	if s.Validate("deadbeef") {
		t.Fatal("bogus token accepted")
	}
	now = now.Add(11 * time.Hour)
	if !s.Validate(tok) {
		t.Fatal("sliding TTL failed inside window")
	}
	now = now.Add(12*time.Hour + time.Second) // last touch was 11 h ago? no: touched at +11h, so now +23h > +11h+12h
	if s.Validate(tok) {
		t.Fatal("expired session accepted")
	}
	tok2, _ := s.Create()
	s.Revoke(tok2)
	if s.Validate(tok2) {
		t.Fatal("revoked session accepted")
	}
}

func TestLockoutMathAndPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lockout.json")
	now := time.Unix(1_800_000_000, 0)
	l := NewLimiter(path)
	l.now = func() time.Time { return now }

	// 5 attempts are allowed inside the window; the block opens on the 6th failure.
	if ok, _ := l.Allow("10.0.0.9"); !ok {
		t.Fatal("blocked at the gate")
	}
	for i := 0; i < 6; i++ {
		l.Fail("10.0.0.9")
	}
	if ok, d := l.Allow("10.0.0.9"); ok || d != time.Minute {
		t.Fatalf("want 1m lockout, got ok=%v d=%v", ok, d)
	}

	// Survives a restart: reload from disk.
	l2 := NewLimiter(path)
	l2.now = func() time.Time { return now }
	if ok, _ := l2.Allow("10.0.0.9"); ok {
		t.Fatal("lockout did not survive reload")
	}

	// After the block passes, the streak doubles the next time.
	now = now.Add(time.Minute + time.Second)
	if ok, _ := l.Allow("10.0.0.9"); !ok {
		t.Fatal("still blocked after window")
	}
	for i := 0; i < 6; i++ {
		l.Fail("10.0.0.9")
	}
	if d, _ := l.Blocked("10.0.0.9"); d != 2*time.Minute {
		t.Fatalf("want 2m streak-2 lockout, got %v", d)
	}
	// Cap at 64 minutes.
	for s := 0; s < 10; s++ {
		now = now.Add(time.Hour)
		for i := 0; i < 6; i++ {
			l.Fail("10.0.0.9")
		}
	}
	if d, _ := l.Blocked("10.0.0.9"); d > 64*time.Minute {
		t.Fatalf("lockout exceeded cap: %v", d)
	}
	// Success clears the address.
	l.Success("10.0.0.9")
	if ok, _ := l.Allow("10.0.0.9"); !ok {
		t.Fatal("success did not clear lockout")
	}
}
