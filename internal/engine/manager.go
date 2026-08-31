package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Manager serializes engine operations (docker run/stop) so the CLI and the
// web console cannot step on each other, and broadcasts op events for the
// console's progress view. All lifecycle goes through here: one code path.
type Manager struct {
	docker Docker
	hc     *http.Client

	opMu     sync.Mutex
	busy     *OpInfo
	opSeq    int
	subMu    sync.Mutex
	subs     map[chan Event]struct{}
}

// OpInfo describes the in-flight or last completed engine operation.
type OpInfo struct {
	Seq     int       `json:"seq"`
	Kind    string    `json:"kind"` // up|down|restart
	Actor   string    `json:"actor"`
	Started time.Time `json:"started"`
	DoneAt  time.Time `json:"done_at,omitzero"`
	Err     string    `json:"err,omitempty"`
	Done    bool      `json:"done"`
}

// Event is broadcast on op start/finish.
type Event struct {
	Op   OpInfo `json:"op"`
	Kind string `json:"kind"` // start|finish
}

// ErrBusy is returned from TryBegin when another op holds the lock.
var ErrBusy = errors.New("another engine operation is in progress")

// NewManager wires a Manager to a Docker implementation.
func NewManager(d Docker) *Manager {
	return &Manager{
		docker: d,
		hc:     &http.Client{Timeout: 5 * time.Second},
		subs:   make(map[chan Event]struct{}),
	}
}

// Subscribe returns a channel of op events plus its cancel func.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	m.subMu.Lock()
	m.subs[ch] = struct{}{}
	m.subMu.Unlock()
	return ch, func() {
		m.subMu.Lock()
		delete(m.subs, ch)
		m.subMu.Unlock()
	}
}

func (m *Manager) publish(ev Event) {
	m.subMu.Lock()
	defer m.subMu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- ev:
		default: // slow consumer: drop, events are advisory
		}
	}
}

// TryBegin acquires the op lock (serve uses this to answer 409 instead of
// queueing an unbounded wait).
func (m *Manager) TryBegin(kind, actor string) (OpInfo, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.busy != nil && !m.busy.Done {
		return OpInfo{}, fmt.Errorf("%w (%s by %s since %s)", ErrBusy, m.busy.Kind, m.busy.Actor, m.busy.Started.Format(time.RFC3339))
	}
	m.opSeq++
	op := OpInfo{Seq: m.opSeq, Kind: kind, Actor: actor, Started: time.Now()}
	m.busy = &op
	m.publish(Event{Op: op, Kind: "start"})
	return op, nil
}

// Finish closes out an op.
func (m *Manager) Finish(op OpInfo, err error) {
	m.opMu.Lock()
	if m.busy != nil && m.busy.Seq == op.Seq {
		op.DoneAt = time.Now()
		op.Done = true
		if err != nil {
			op.Err = err.Error()
		}
		opCopy := op
		m.busy = &opCopy
	}
	m.opMu.Unlock()
	m.publish(Event{Op: op, Kind: "finish"})
}

// LastOp returns the current/most recent op for status surfaces.
func (m *Manager) LastOp() (OpInfo, bool) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.busy == nil {
		return OpInfo{}, false
	}
	return *m.busy, true
}

// Up launches the container: `docker rm -f` (idempotent, mirrors serve.sh's
// `docker rm -f … || true`) then `docker run` with the full spec argv.
func (m *Manager) Up(ctx context.Context, op OpInfo, args []string, name string) error {
	err := m.up(ctx, args, name)
	m.Finish(op, err)
	return err
}

func (m *Manager) up(ctx context.Context, args []string, name string) error {
	// serve.sh tolerates rm -f failure entirely (`|| true`): a missing or
	// stopped container is exactly the state we want before `docker run`.
	_, _ = m.docker.Run(ctx, "rm", "-f", name)
	out, err := m.docker.Run(ctx, args...)
	if err != nil {
		return fmt.Errorf("docker run failed: %w\n%s", err, out)
	}
	return nil
}

// Down stops (graceful) and removes the container.
func (m *Manager) Down(ctx context.Context, op OpInfo, name string) error {
	err := m.down(ctx, name)
	m.Finish(op, err)
	return err
}

func (m *Manager) down(ctx context.Context, name string) error {
	if err := Stop(ctx, m.docker, name, 30*time.Second); err != nil {
		return err
	}
	out, err := m.docker.Run(ctx, "rm", "-f", name)
	if err != nil && !looksNotFound(out, err) {
		return fmt.Errorf("docker rm %s: %w (%s)", name, err, out)
	}
	return nil
}

// Logs streams container logs into w until ctx ends.
func (m *Manager) Logs(ctx context.Context, name string, w io.Writer) error {
	return m.docker.FollowLogs(ctx, name, w)
}

// Docker exposes the underlying client for doctor probes.
func (m *Manager) Docker() Docker { return m.docker }

// ---- engine HTTP (localhost-side probes; lockdown key required when on) ----

// Health GETs /health. With a lockdown key set it is still open (vLLM's
// --api-key guards /v1 routes), so this doubles as the readiness signal the
// boot parser and collector rely on.
func (m *Manager) Health(ctx context.Context, baseURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Models GETs /v1/models with the bearer key; returns the served model ids.
func (m *Manager) Models(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errors.New("engine rejected our lockdown key (did it change under a running container?)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("engine /v1/models: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	var ids []string
	for _, d := range payload.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}
