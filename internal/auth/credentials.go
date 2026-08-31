// Package auth holds QFN-PGX's secret material and web-session plumbing.
//
// The password gate for the always-on console and the engine API key used for
// lockdown live in one age-encrypted file (credentials.age), following the
// pattern proven in tempotrack: the age identity sits next to it at 0600 and
// nothing secret ever touches config.toml in the clear.
package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"path/filepath"
	"time"

	"filippo.io/age"
	"golang.org/x/crypto/argon2"
)

// Argon2id parameters for the console password. 64 MiB / 3 passes is a
// ~100 ms verify on the Spark's X5 cores — enough to blunt scripted attempts
// while the box is memory-tight, and it rides out swap pressure fine because
// verification is rare (rate-limited by ratelimit.go).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
)

// ErrNoCredentials is returned before the first-run wizard has completed.
var ErrNoCredentials = errors.New("qfn: no credentials yet — run `qfn init` (or set a password with `qfn passwd`)")

// Payload is the decrypted contents of credentials.age.
type Payload struct {
	PwSalt    []byte `json:"pw_salt"`
	PwHash    []byte `json:"pw_hash"`
	EngineKey string `json:"engine_key,omitempty"` // vLLM --api-key when lockdown on
	FrontKey  string `json:"front_key,omitempty"`  // optional machine bearer key for :8799 (rotate via qfn serve --rotate-key)
	APIKeys   []APIKeyRec `json:"api_keys,omitempty"` // named machine keys (qfn keys add)
	Version   int    `json:"v"`
}

// Store manages <dir>/age.key + <dir>/credentials.age with hot-reload.
type Store struct {
	dir string

	cache    *Payload
	cacheMod time.Time

	// Seams for tests.
	now func() time.Time
}

// NewStore points at a credentials directory (usually config.Dir()).
func NewStore(dir string) *Store { return &Store{dir: dir, now: time.Now} }

// Paths.
func (s *Store) keyPath() string  { return filepath.Join(s.dir, "age.key") }
func (s *Store) credPath() string { return filepath.Join(s.dir, "credentials.age") }

// EnsureFilePerms refuses to load or write identity material that any group or
// other user could read — the whole encryption story is void otherwise.
func checkPrivate(path string, mode os.FileMode) error {
	if mode&0o077 != 0 {
		return fmt.Errorf("%s has permissions %o — must be 0600 (chmod 600 %s)", path, mode.Perm(), path)
	}
	return nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashPassword(pw string, salt []byte) []byte {
	return argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
}

// EnsureFirstRun creates the age identity (if absent) and an initial payload.
// If pw == "" and a payload already exists with an engine key, it is kept.
func (s *Store) EnsureFirstRun(pw string) error {
	if err := s.ensureIdentity(); err != nil {
		return err
	}
	p, existErr := s.load()
	if existErr != nil && !errors.Is(existErr, ErrNoCredentials) {
		return existErr
	}
	if p == nil {
		p = &Payload{Version: 1}
	}
	if pw != "" {
		salt := make([]byte, saltLen)
		if _, err := rand.Read(salt); err != nil {
			return err
		}
		p.PwSalt = salt
		p.PwHash = hashPassword(pw, salt)
	}
	if p.EngineKey == "" {
		k, err := randomHex(32)
		if err != nil {
			return err
		}
		p.EngineKey = k
	}
	return s.save(p)
}

func (s *Store) ensureIdentity() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	raw, err := os.ReadFile(s.keyPath())
	if err == nil {
		if fi, serr := os.Stat(s.keyPath()); serr == nil {
			if cerr := checkPrivate(s.keyPath(), fi.Mode()); cerr != nil {
				return cerr
			}
		}
		_, perr := age.ParseX25519Identity(strings.TrimSpace(string(raw)))
		if perr != nil {
			return fmt.Errorf("%s: %w", s.keyPath(), perr)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return err
	}
	return os.WriteFile(s.keyPath(), []byte(id.String()+"\n"), 0o600)
}

func (s *Store) identity() (*age.X25519Identity, error) {
	fi, err := os.Stat(s.keyPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoCredentials
		}
		return nil, err
	}
	if err := checkPrivate(s.keyPath(), fi.Mode()); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(s.keyPath())
	if err != nil {
		return nil, err
	}
	return age.ParseX25519Identity(strings.TrimSpace(string(raw)))
}

func (s *Store) save(p *Payload) error {
	id, err := s.identity()
	if err != nil {
		return err
	}
	plaintext, err := json.Marshal(p)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	w, err := age.Encrypt(&out, id.Recipient())
	if err != nil {
		return err
	}
	if _, err := w.Write(plaintext); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	tmp := s.credPath() + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.credPath()); err != nil {
		return err
	}
	s.cache = p
	if fi, err := os.Stat(s.credPath()); err == nil {
		s.cacheMod = fi.ModTime()
	}
	return nil
}

// load decrypts credentials.age, using the mtime cache when unchanged.
func (s *Store) load() (*Payload, error) {
	fi, err := os.Stat(s.credPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoCredentials
		}
		return nil, err
	}
	if s.cache != nil && !fi.ModTime().After(s.cacheMod) {
		return s.cache, nil
	}
	id, err := s.identity()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.credPath())
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(bytes.NewReader(b), id)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot decrypt (wrong age.key?): %w", s.credPath(), err)
	}
	plaintext, err := io.ReadAll(io.LimitReader(r, 1<<16))
	if err != nil {
		return nil, err
	}
	var p Payload
	if err := json.Unmarshal(plaintext, &p); err != nil {
		return nil, fmt.Errorf("%s: corrupt payload: %w", s.credPath(), err)
	}
	s.cache, s.cacheMod = &p, fi.ModTime()
	return &p, nil
}

// InvalidateCache forces the next read to decrypt again (tests, edge cases).
func (s *Store) InvalidateCache() { s.cache = nil }

// SetPassword re-hashes and persists. Safe to call while serve runs: the mtime
// bump makes the server hot-reload within one request cycle.
func (s *Store) SetPassword(pw string) error {
	p, err := s.load()
	if err != nil {
		if !errors.Is(err, ErrNoCredentials) {
			return err
		}
		p = &Payload{Version: 1}
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	p.PwSalt, p.PwHash = salt, hashPassword(pw, salt)
	return s.save(p)
}

// VerifyPassword constant-time checks the console password.
func (s *Store) VerifyPassword(pw string) bool {
	p, err := s.load()
	if err != nil || len(p.PwHash) == 0 {
		return false
	}
	got := hashPassword(pw, p.PwSalt)
	return subtle.ConstantTimeCompare(got, p.PwHash) == 1
}

// HasPassword reports whether a console password exists.
func (s *Store) HasPassword() bool {
	p, err := s.load()
	return err == nil && len(p.PwHash) > 0
}

// EngineKey returns the vLLM API key ("" before first EnsureFirstRun).
func (s *Store) EngineKey() (string, error) {
	p, err := s.load()
	if err != nil {
		return "", err
	}
	return p.EngineKey, nil
}

// RotateEngineKey replaces the lockdown key and returns it.
func (s *Store) RotateEngineKey() (string, error) {
	p, err := s.load()
	if err != nil {
		if !errors.Is(err, ErrNoCredentials) {
			return "", err
		}
		p = &Payload{Version: 1}
	}
	k, err := randomHex(32)
	if err != nil {
		return "", err
	}
	p.EngineKey = k
	return k, s.save(p)
}

// FrontKey returns the optional machine bearer key for the console port.
func (s *Store) FrontKey() (string, error) {
	p, err := s.load()
	if err != nil {
		return "", err
	}
	return p.FrontKey, nil
}

// RotateFrontKey generates/replaces the machine bearer key and returns it.
// The plaintext is shown exactly once (CLI); credentials.age keeps it for the
// server to accept against.
func (s *Store) RotateFrontKey() (string, error) {
	p, err := s.load()
	if err != nil {
		if !errors.Is(err, ErrNoCredentials) {
			return "", err
		}
		p = &Payload{Version: 1}
	}
	k, err := randomHex(32)
	if err != nil {
		return "", err
	}
	p.FrontKey = k
	return k, s.save(p)
}

// ---- named per-client API keys ------------------------------------------------
//
// One shared front key is great until you want to know WHO is hammering the
// front door, or hand opencode a key you can revoke without rotating everyone
// off. Named keys: the plaintext is shown once, only its SHA-256 is stored,
// and requests through one get attributed by name in the request feed.

// APIKeyRec is one named machine key (plaintext never stored).
type APIKeyRec struct {
	Name    string `json:"name"`
	Hash    string `json:"hash"`    // hex(sha256(token))
	Preview string `json:"preview"` // first 12 chars — enough to recognize, useless to steal
	Created int64  `json:"created"` // unix seconds
}

var ErrKeyExists = errors.New("qfn: an API key with that name already exists")

// AddAPIKey creates a named key; the plaintext is returned exactly once.
func (s *Store) AddAPIKey(name string) (string, error) {
	if !validKeyName(name) {
		return "", errors.New("qfn: key name must be 1-32 chars of letters, digits, '-' or '_'")
	}
	p, err := s.load()
	if err != nil {
		if !errors.Is(err, ErrNoCredentials) {
			return "", err
		}
		p = &Payload{Version: 1}
	}
	for _, k := range p.APIKeys {
		if k.Name == name {
			return "", ErrKeyExists
		}
	}
	tok, err := randomHex(20)
	if err != nil {
		return "", err
	}
	tok = "qfn_" + tok
	sum := sha256.Sum256([]byte(tok))
	p.APIKeys = append(p.APIKeys, APIKeyRec{
		Name: name, Hash: hex.EncodeToString(sum[:]),
		Preview: tok[:12] + "…", Created: s.now().Unix(),
	})
	return tok, s.save(p)
}

// ListAPIKeys returns metadata only — hashes are dropped.
func (s *Store) ListAPIKeys() ([]APIKeyRec, error) {
	p, err := s.load()
	if err != nil {
		if errors.Is(err, ErrNoCredentials) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]APIKeyRec, 0, len(p.APIKeys))
	for _, k := range p.APIKeys {
		k.Hash = ""
		out = append(out, k)
	}
	return out, nil
}

// RevokeAPIKey removes a named key. Revoke is immediate on the next request
// (the store hot-reloads); nothing to restart.
func (s *Store) RevokeAPIKey(name string) error {
	p, err := s.load()
	if err != nil {
		return err
	}
	out := p.APIKeys[:0]
	found := false
	for _, k := range p.APIKeys {
		if k.Name == name {
			found = true
			continue
		}
		out = append(out, k)
	}
	if !found {
		return fmt.Errorf("qfn: no API key named %q (`qfn keys list`)", name)
	}
	p.APIKeys = out
	return s.save(p)
}

// MatchAPIKey resolves a bearer token to its key name ("" + false if none).
func (s *Store) MatchAPIKey(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	p, err := s.load()
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	want := hex.EncodeToString(sum[:])
	for _, k := range p.APIKeys {
		if subtle.ConstantTimeCompare([]byte(k.Hash), []byte(want)) == 1 {
			return k.Name, true
		}
	}
	return "", false
}

func validKeyName(n string) bool {
	if n == "" || len(n) > 32 {
		return false
	}
	for _, r := range n {
		if !(r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
