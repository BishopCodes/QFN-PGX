// Package service renders and installs the systemd units that make the console
// reboot-persistent (and optionally auto-boot the engine). Pattern borrowed
// from dgx-spark-qwen38's service machinery: units are owned by qfn, tracked
// in an inventory file, and `uninstall --list` can show exactly what it owns
// before it removes it.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// Unit names.
const (
	ServeUnit  = "qfn-serve"
	EngineUnit = "qfn-engine"
)

const serveTemplate = `# qfn-owned unit (qfn service install; qfn service uninstall to remove)
[Unit]
Description=QFN-PGX console (Qwen3.8-Flash-Next ops console + proxy)
After=docker.service
Wants=docker.service

[Service]
Type=simple
User=%[1]s
ExecStart=%[2]s serve
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`

const engineTemplate = `# qfn-owned unit (qfn service install --engine-autostart; qfn service uninstall to remove)
[Unit]
Description=QFN-PGX engine (qwen3.8-flash-next vLLM lane)
After=docker.service
Wants=docker.service

[Service]
Type=simple
User=%[1]s
ExecStart=%[2]s up --yes
ExecStop=%[2]s down
Restart=always
RestartSec=15
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
`

// Deps are injectable for tests (sudo systemctl wrapper in prod).
type Deps struct {
	Runner func(ctx context.Context, args ...string) (string, error) // e.g. sudo systemctl / systemctl
	UnitDir string // override for tests (default /etc/systemd/system)
	Write  func(path string, content []byte, mode os.FileMode) error
	Remove func(path string) error
	Read   func(path string) ([]byte, error)
}

// Manager installs/removes units.
type Manager struct {
	deps      Deps
	unitDir   string
	inventory string // json inventory path (state dir)
}

// New builds a Manager. binaryPath is the resolved qfn binary (os.Executable),
// stateDir the config paths.state_dir.
func New(stateDir string, d Deps) *Manager {
	if d.Runner == nil {
		d.Runner = runSudo
	}
	if d.Write == nil {
		d.Write = func(p string, c []byte, m os.FileMode) error {
			tmp := p + ".qfn.tmp"
			if err := os.WriteFile(tmp, c, m); err != nil {
				return err
			}
			return os.Rename(tmp, p)
		}
	}
	if d.Remove == nil {
		d.Remove = os.Remove
	}
	if d.Read == nil {
		d.Read = os.ReadFile
	}
	dir := d.UnitDir
	if dir == "" {
		dir = "/etc/systemd/system"
	}
	return &Manager{deps: d, unitDir: dir, inventory: filepath.Join(stateDir, "service-inventory.json")}
}

func (m *Manager) unitPath(unit string) string {
	return filepath.Join(m.unitDir, unit+".service")
}

// Install renders, writes, and enables units (idempotent).
func (m *Manager) Install(ctx context.Context, binaryPath string, withEngine bool) ([]string, error) {
	u, err := user.Current()
	if err != nil {
		return nil, err
	}
	var done []string
	render := func(unit, tmpl string) error {
		content := fmt.Sprintf(tmpl, u.Username, binaryPath)
		path := m.unitPath(unit)
		if err := m.deps.Write(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		if _, err := m.deps.Runner(ctx, "sudo", "systemctl", "daemon-reload"); err != nil {
			return err
		}
		if _, err := m.deps.Runner(ctx, "sudo", "systemctl", "enable", "--now", unit+".service"); err != nil {
			return err
		}
		done = append(done, unit)
		return nil
	}
	if err := render(ServeUnit, serveTemplate); err != nil {
		return done, err
	}
	if withEngine {
		if err := render(EngineUnit, engineTemplate); err != nil {
			return done, err
		}
	}
	return done, m.record(done)
}

// Uninstall removes units. dryRun=true only reports.
func (m *Manager) Uninstall(ctx context.Context, dryRun bool) ([]string, error) {
	owned, err := m.Owned(ctx)
	if err != nil {
		return nil, err
	}
	if dryRun {
		return owned.Names, nil
	}
	var removed []string
	for _, name := range owned.Names {
		if _, err := m.deps.Runner(ctx, "sudo", "systemctl", "disable", "--now", name+".service"); err != nil {
			return removed, err
		}
		if err := m.deps.Remove(m.unitPath(name)); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		removed = append(removed, name)
	}
	if _, err := m.deps.Runner(ctx, "sudo", "systemctl", "daemon-reload"); err != nil {
		return removed, err
	}
	_ = m.record(nil)
	return removed, nil
}

// Owned reports which qfn units exist + whether they're enabled, cross-checking
// the inventory so hand-placed copies are still visible (uninstall --list).
type OwnedReport struct {
	Names        []string `json:"names"`
	EnabledState map[string]string `json:"enabled_state"`
	Inventory    []string `json:"inventory"`
}

func (m *Manager) Owned(ctx context.Context) (*OwnedReport, error) {
	rep := &OwnedReport{EnabledState: map[string]string{}}
	for _, u := range []string{ServeUnit, EngineUnit} {
		if _, err := os.Stat(m.unitPath(u)); err == nil {
			rep.Names = append(rep.Names, u)
			out, err := m.deps.Runner(ctx, "systemctl", "is-enabled", u+".service")
			if err != nil {
				rep.EnabledState[u] = "disabled"
			} else {
				rep.EnabledState[u] = strings.TrimSpace(out)
			}
		}
	}
	if b, err := m.deps.Read(m.inventory); err == nil {
		_ = json.Unmarshal(b, &rep.Inventory)
	}
	return rep, nil
}

func (m *Manager) record(units []string) error {
	if err := os.MkdirAll(filepath.Dir(m.inventory), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(units)
	return os.WriteFile(m.inventory, b, 0o644)
}

func runSudo(ctx context.Context, args ...string) (string, error) {
	var cmd *exec.Cmd
	if len(args) > 0 && args[0] == "sudo" {
		cmd = exec.CommandContext(ctx, "sudo", args[1:]...)
	} else {
		cmd = exec.CommandContext(ctx, args[0], args[1:]...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
