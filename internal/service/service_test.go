package service

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fake struct {
	mu     sync.Mutex
	calls  [][]string
	writes map[string]string
}

func (f *fake) run(ctx context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "is-enabled qfn-engine") {
		return "disabled", errors.New("exit 1")
	}
	return "enabled", nil
}

func newFakeManager(t *testing.T) (*Manager, *fake) {
	t.Helper()
	dir := t.TempDir()
	unitDir := filepath.Join(dir, "units")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fake{writes: map[string]string{}}
	m := New(filepath.Join(dir, "state"), Deps{
		Runner:  f.run,
		UnitDir: unitDir,
		Write: func(path string, content []byte, mode os.FileMode) error {
			f.writes[path] = string(content)
			return os.WriteFile(path, content, mode)
		},
	})
	m.unitDir = unitDir
	return m, f
}

func TestInstallRendersAndEnables(t *testing.T) {
	m, f := newFakeManager(t)
	done, err := m.Install(context.Background(), "/usr/local/bin/qfn", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 {
		t.Fatalf("units: %v", done)
	}
	for _, u := range done {
		b, err := os.ReadFile(m.unitPath(u))
		if err != nil {
			t.Fatalf("%s not written: %v", u, err)
		}
		s := string(b)
		if !strings.Contains(s, "ExecStart=/usr/local/bin/qfn serve") &&
			!strings.Contains(s, "ExecStart=/usr/local/bin/qfn up --yes") {
			t.Fatalf("%s exec line missing:\n%s", u, s)
		}
		if !strings.Contains(s, "Restart=always") {
			t.Fatalf("%s must restart always", u)
		}
	}
	var sawReload, sawEnable bool
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "daemon-reload") {
			sawReload = true
		}
		if strings.Contains(j, "enable --now qfn-serve.service") {
			sawEnable = true
		}
	}
	if !sawReload || !sawEnable {
		t.Fatalf("systemctl calls: %v", f.calls)
	}
}

func TestUninstallListAndRemove(t *testing.T) {
	m, _ := newFakeManager(t)
	if _, err := m.Install(context.Background(), "/bin/qfn", true); err != nil {
		t.Fatal(err)
	}
	owned, err := m.Owned(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(owned.Names) != 2 {
		t.Fatalf("owned: %v", owned.Names)
	}
	removed, err := m.Uninstall(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed: %v", removed)
	}
	for _, u := range removed {
		if _, err := os.Stat(m.unitPath(u)); !os.IsNotExist(err) {
			t.Fatalf("%s still present", u)
		}
	}
	owned2, _ := m.Owned(context.Background())
	if len(owned2.Names) != 0 {
		t.Fatalf("still owned: %v", owned2.Names)
	}
}

func TestInstallIdempotent(t *testing.T) {
	m, _ := newFakeManager(t)
	if _, err := m.Install(context.Background(), "/bin/qfn", false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(context.Background(), "/bin/qfn", false); err != nil {
		t.Fatalf("second install must be clean: %v", err)
	}
}

func TestUnitUserPrefersRealHumanUnderSudo(t *testing.T) {
	t.Setenv("SUDO_USER", "human")
	got := UnitUser()
	if os.Geteuid() == 0 {
		if got != "human" {
			t.Fatalf("root must defer to SUDO_USER, got %q", got)
		}
		return
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if got != u.Username {
		t.Fatalf("non-root must ignore SUDO_USER, got %q want %q", got, u.Username)
	}
}
