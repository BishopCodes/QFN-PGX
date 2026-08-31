package auth

import "testing"

func TestNamedAPIKeysRoundTrip(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AddAPIKey("opencode"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAPIKey("opencode"); err == nil {
		t.Fatal("duplicate name must be refused")
	}
	if _, err := s.AddAPIKey("bad name!"); err == nil {
		t.Fatal("invalid name must be refused")
	}
	tok, err := s.AddAPIKey("harness")
	if err != nil {
		t.Fatal(err)
	}
	if name, ok := s.MatchAPIKey(tok); !ok || name != "harness" {
		t.Fatalf("match: %q %v", name, ok)
	}
	if _, ok := s.MatchAPIKey("qfn_deadbeef"); ok {
		t.Fatal("unknown token must not match")
	}
	list, err := s.ListAPIKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Hash != "" {
		t.Fatalf("list leaks hashes or wrong size: %+v", list)
	}
	if err := s.RevokeAPIKey("harness"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.MatchAPIKey(tok); ok {
		t.Fatal("revoked key still valid")
	}
	if err := s.RevokeAPIKey("harness"); err == nil {
		t.Fatal("double revoke must error")
	}
}

func TestAPIKeysSurviveReload(t *testing.T) {
	s := newTestStore(t)
	tok, err := s.AddAPIKey("persist")
	if err != nil {
		t.Fatal(err)
	}
	s2 := &Store{dir: s.dir, now: s.now} // fresh cache, same files
	if name, ok := s2.MatchAPIKey(tok); !ok || name != "persist" {
		t.Fatalf("reload lost keys: %q %v", name, ok)
	}
}
