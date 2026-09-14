// Vision probe: end-to-end "can this engine take an image?" evidence.
//
// Image support needs three independent things to line up, and each failing
// mode says something different — so we check all three instead of guessing:
//  1. the snapshot's config.json declares vision_config (weights + arch)
//  2. the running container's argv declares --limit-mm-per-prompt
//  3. the live engine actually accepts an N-PNG chat request (N = Images,
//     default 1; the --vision-matrix battery is 1, 4, 16 like the tpurtell
//     recipe's qualification)
//
// An image refusal has two causes, and reading vLLM's own path settles which
// evidence decides them: multimodal/registry.py:create_processor gates on
// ModelConfig.is_multimodal_model — _model_info.supports_multimodal, a
// property of the REGISTERED CLASS — and only then builds a processor. So
// "is not a multimodal model" is a build/registry fact no checkpoint edit
// changes, while a processor-load failure is the checkpoint's mm manifests.
// The HTTP 400 usually carries the first; only the container log carries the
// second, which is why VisionDeps takes both Logs and Modalities.
package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/engine"
)

// SnapshotMM is what a checkpoint directory (the HOST dir, not the
// in-container /hf path) declares about image input. The facts are separate
// because the remediations are: Tower = vision weights and arch are in the
// checkpoint (config.json's vision_config); Processor names the combined
// processor class (processor_config.json) and is the manifest vLLM resolves a
// processor from; Preprocessor (preprocessor_config.json) carries image
// preprocessing alone — the RadixArk port ships that and not Processor,
// nvidia ships both. ModelType lets a hint name the arch whose wiring is
// missing. Config is the honesty gate: false means config.json was never read,
// so the snapshot says NOTHING — an unpulled checkpoint is normal, not a
// failure, and must never read as "no vision".
type SnapshotMM struct {
	Tower        bool
	Processor    bool
	Preprocessor bool
	Config       bool // config.json was readable and parsed
	ModelType    string
}

func SnapshotMMAt(dir string) SnapshotMM {
	var mm SnapshotMM
	if dir == "" {
		return mm
	}
	if b, err := os.ReadFile(filepath.Join(dir, "config.json")); err == nil {
		var m struct {
			ModelType string `json:"model_type"`
			// Pointer: the question is presence, and vision_config's
			// shape varies (object in a full checkpoint, anything in a stub).
			VisionConfig *json.RawMessage `json:"vision_config"`
		}
		if json.Unmarshal(b, &m) == nil {
			mm.Tower = m.VisionConfig != nil
			mm.ModelType = m.ModelType
			mm.Config = true
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "processor_config.json")); err == nil {
		mm.Processor = true
	}
	if _, err := os.Stat(filepath.Join(dir, "preprocessor_config.json")); err == nil {
		mm.Preprocessor = true
	}
	return mm
}

// SnapshotVision is SnapshotMMAt's two-fact summary, kept for callers that
// only ask "tower there? processor there?".

// 1x1 PNG, red.
const probePNGb64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// VisionDeps wires the probe; all seams are injectable for tests.
type VisionDeps struct {
	Base func() string // engine base URL (direct, not console)
	Key  func() string // engine API key
	Args []string      // running container argv ("" if engine not up)

	// Locator and Engine together resolve the checkpoint dir at check time
	// (nil Locator = the check can't see the host filesystem). Together with
	// SnapshotHasVision they let a "fix the snapshot" hint name a destination
	// and the configured image count instead of a rumour.
	Locator           engine.SnapshotLocator
	Engine            config.Engine // Model/Mode select the snapshot dir
	SnapshotHasVision bool
	Images            []int // probe battery; nil/empty = [1]
	Post              func(ctx context.Context, url, key string, body []byte) (int, string, error)

	// Modalities reports what the engine says it can take for the served
	// model; ok=false means it publishes nothing, which is unknown and NOT
	// "no vision" (see DeclaredModalities). nil = unknown. This outranks any
	// file on disk: a model the registry never wired for images refuses one
	// however complete the checkpoint is.
	Modalities func(ctx context.Context) (mods []string, ok bool)
	// SnapshotReadable says config.json was actually read. Without it, a
	// missing checkpoint would be reported as "no vision tower".
	SnapshotReadable bool
	// Logs returns the tail of the engine container log ("" = unavailable).
	// A refused request's HTTP body is one line; the traceback naming the
	// failing class or processor only ever lands here.
	Logs func(ctx context.Context) string
}

// logTail is the nil-safe call into the Logs seam.
func (d VisionDeps) logTail(ctx context.Context) string {
	if d.Logs == nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return d.Logs(cctx)
}

// snapshotDir is the checkpoint's directory on this host, resolved through the
// same locator doctor's checkpoint check uses. "" = unknown (no locator, no
// snapshot pulled, or an unresolvable mode).
func (d VisionDeps) snapshotDir() string {
	if d.Locator == nil {
		return ""
	}
	return engine.SnapshotHostDir(d.Locator, d.Engine)
}

func visionBody(model string, n int) []byte {
	parts := []any{map[string]any{"type": "text", "text": "reply with the single word: ok"}}
	for range n {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{
			"url": "data:image/png;base64," + probePNGb64}})
	}
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": parts}},
		"max_tokens": 4, "stream": false,
	})
	return b
}

// VisionCheck runs the probe battery and classifies the first failure mode.
func VisionCheck(ctx context.Context, d VisionDeps) Check {
	c := Check{ID: "vision"}
	ns := d.Images
	if len(ns) == 0 {
		ns = []int{1}
	}
	// Launch flags before anything that costs money to check. A zero image
	// limit means no prompt can carry an image part however good the
	// checkpoint and the build are, and it is one config line to fix — while
	// the checkpoint answer below costs a pull and the build answer a rebuild.
	// (With the engine down there is no argv to read, so the checkpoint-side
	// facts still get the floor: they stand on their own.)
	if len(d.Args) > 0 {
		if n, ok := mmLimit(d.Args); ok && n == 0 {
			c.Status, c.Msg = "bad", "the running engine was launched with --limit-mm-per-prompt {\"image\": 0}: this lane is configured text-only"
			c.Hint = "set engine.images in config.toml and `qfn restart` — until the container is relaunched the engine will refuse images, whatever the checkpoint or build does" + lookedIn(d.snapshotDir())
			return c
		}
	}
	if !d.SnapshotHasVision {
		if !d.SnapshotReadable {
			c.Status, c.Msg = "warn", "the checkpoint's config.json could not be read, so its vision posture is unknown (pulled yet?)"
			c.Hint = "`qfn pull` this model, then run this check again" + lookedIn(d.snapshotDir())
			return c
		}
		c.Status, c.Msg = "warn", "this checkpoint declares no vision tower: config.json carries no vision_config, so image input is impossible with it"
		c.Hint = "diff that file against the repo's own config.json — a partial pull is the usual cause, and unlike a missing manifest it is not fixable by copying one file in" + snapshotNote(d.snapshotDir(), d.Engine.Images)
		return c
	}
	if len(d.Args) == 0 {
		c.Status, c.Msg = "warn", "engine not running — start it (`qfn up`) and run doctor again"
		return c
	}
	hasFlag := false
	for _, a := range d.Args {
		if strings.Contains(a, "limit-mm-per-prompt") {
			hasFlag = true
		}
	}
	if !hasFlag {
		c.Status, c.Msg = "bad", "running engine was launched WITHOUT --limit-mm-per-prompt"
		c.Hint = "the container predates the images flag — `qfn restart` (or stop + up) to relaunch with new argv"
		return c
	}
	// The cheapest discriminator, asked before any probe burns a slot: an
	// engine that publishes its modalities says outright whether the registry
	// wired this arch for images — a build fact, not a checkpoint fact.
	declared := false
	if d.Modalities != nil {
		// An empty list is not a declaration of "text only", so it must not
		// print as one ("takes  for this model") nor blame the build: it
		// declares nothing, which is the same silence as ok=false.
		if mods, ok := d.Modalities(ctx); ok && len(mods) > 0 {
			if declared = modsImage(mods); !declared {
				snapDir := d.snapshotDir()
				c.Status, c.Msg = "bad", fmt.Sprintf("engine declares it takes %s for this model — no image modality", strings.Join(mods, "/"))
				c.Hint = registryHint(SnapshotMMAt(snapDir))
				return c
			}
		}
	}
	for _, n := range ns {
		pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		status, body, err := d.Post(pctx, strings.TrimRight(d.Base(), "/")+"/v1/chat/completions", d.Key(), visionBody(d.Engine.Model, n))
		cancel()
		if err != nil {
			c.Status, c.Msg = "warn", fmt.Sprintf("vision probe (%s image) could not reach the engine: %s", nLabel(n), errOr(err, body))
			return c
		}
		switch {
		case status < 400:
			continue
		case bodyHas(body, "at most", "not allowed"):
			c.Status, c.Msg = "bad", fmt.Sprintf("engine rejects %s-image input: limit is below the probe count despite our flag (flag syntax silently ignored?)", nLabel(n))
			c.Hint = "check the container start line in the engine logs"
		case bodyHas(body, "not supported", "not a multimodal", "does not support", "no modality"):
			c.Status, c.Msg = "bad", fmt.Sprintf("engine refuses %s-image input as a modality it does not support", nLabel(n))
			if q := tracebackLine(d.logTail(ctx)); q != "" {
				c.Msg += fmt.Sprintf(" — the engine's own line: %s", q)
			}
			snapDir := d.snapshotDir()
			c.Hint = refusalHint(SnapshotMMAt(snapDir), snapDir, d.Engine.Images, declared)
		default:
			c.Status, c.Msg = "warn", fmt.Sprintf("vision probe failed (%s image, HTTP %d): %s", nLabel(n), status, firstLine(shorten(body)))
			c.Hint = "engine's verbatim error above; run `qfn chat --image file.png \"describe\"` for a fuller probe"
		}
		return c
	}
	if len(ns) > 1 {
		c.Status, c.Msg = "ok", fmt.Sprintf("engine accepts image input (probes %v round-trip)", ns)
	} else {
		c.Status, c.Msg = "ok", "engine accepts image input (1×1 probe round-trip)"
	}
	return c
}

// DeclaredModalities reads the input modalities GET /v1/models publishes.
// vLLM has shipped that field under two names and the pinned base's ModelCard
// may publish neither, hence the ok flag: absent means the build says nothing
// about vision, never that it denies it.
func DeclaredModalities(ctx context.Context, base, key string) (mods []string, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/models", nil)
	if err != nil {
		return nil, false
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Data []struct {
			Input  []string `json:"supported_input_modalities"`
			Legacy []string `json:"supported_modalities"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &out) != nil {
		return nil, false
	}
	// One model per lane: whichever entry publishes the field answers it.
	for _, m := range out.Data {
		// A declared-but-empty list is not a declaration of "text only": it
		// names no modality at all, so it carries no evidence and must read
		// as unknown — the probe below asks the engine outright instead.
		if len(m.Input) > 0 {
			return m.Input, true
		}
		if len(m.Legacy) > 0 {
			return m.Legacy, true
		}
	}
	return nil, false
}

func modsImage(mods []string) bool {
	for _, m := range mods {
		if strings.EqualFold(m, "image") {
			return true
		}
	}
	return false
}

// mmLimit reads the image count out of a running container's
// --limit-mm-per-prompt argv ({"image": 4}). ok=false = the flag or a readable
// count is not there.
func mmLimit(args []string) (n int, ok bool) {
	for i, a := range args {
		v := ""
		switch {
		case strings.HasPrefix(a, "--limit-mm-per-prompt="):
			v = strings.TrimPrefix(a, "--limit-mm-per-prompt=")
		case strings.Contains(a, "limit-mm-per-prompt") && i+1 < len(args):
			v = args[i+1]
		default:
			continue
		}
		var m map[string]float64
		if json.Unmarshal([]byte(v), &m) != nil {
			return 0, false
		}
		n, ok = int(m["image"]), true
	}
	return
}

// tracebackLine pulls the engine's own error line out of a container log tail:
// the last line that reads like an exception, since that is the one naming the
// class or processor that failed. Empty tail, empty answer.
func tracebackLine(logs string) string {
	lines := strings.Split(strings.TrimRight(logs, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" && bodyHas(l, "error:", "exception", "assert") {
			l = firstLine(l)
			// A traceback line's point is at the END (class + message), so
			// trim the front, never the middle — doctor.go's shorten would
			// ellipsis right through "is not a multimodal model".
			if r := []rune(l); len(r) > 200 {
				l = "…" + string(r[len(r)-200:])
			}
			return l
		}
	}
	return ""
}

// refusalHint explains a modality refusal the registry gate let through: the
// probe only runs on an engine that claims to take images, so "declared
// text-only" was already answered upstream by registryHint. What's left to
// rank is checkpoint manifest vs engine traceback, in that order — the file
// answer is cheap and specific, so it goes first when the facts support it.
// declared says whether the engine itself confirmed the image modality; when
// it didn't (no modalities published) the build wiring is a hypothesis, not a
// fact, and the hint must read like one.
func refusalHint(mm SnapshotMM, dir string, images int, declared bool) string {
	if mm.Preprocessor && !mm.Processor {
		lead := "this checkpoint cannot resolve its processor: it ships preprocessor_config.json without processor_config.json"
		if !declared {
			lead = "if this build does register the arch as multimodal, the checkpoint is the suspect: it ships preprocessor_config.json without processor_config.json"
		}
		return lead + " — copy that one file from nvidia/Qwen3.8-Flash-Next-NVFP4, or serve the nvidia profile (which ships it)" + snapshotNote(dir, images)
	}
	return "read the line above: \"is not a multimodal model\" means the running build never wired this arch for images and no checkpoint edit changes that (see engine/Dockerfile), while a line naming the processor means the checkpoint's mm manifests are the problem — processor_config.json copies from nvidia/Qwen3.8-Flash-Next-NVFP4" + snapshotNote(dir, images)
}

func registryHint(mm SnapshotMM) string {
	arch := mm.ModelType
	if arch == "" {
		arch = "this architecture"
	}
	return arch + " is registered as text-only in the running build, so no checkpoint edit changes it — the fix belongs to the engine image: move the pinned base in engine/Dockerfile to a build carrying this arch's multimodal wiring, or vendor that patch"
}

// snapshotNote makes a "fix the snapshot" hint actionable: the directory the
// probe actually read, whether processor_config.json is missing there, and
// what the launch allows (engine.images) — the same two facts wherever a hint
// tells the user to touch the checkpoint. Empty when the directory is unknown:
// never print a guessed path.
func snapshotNote(dir string, images int) string {
	if dir == "" {
		return ""
	}
	state := "processor_config.json MISSING (a copy is the fix only where the traceback names the processor)"
	if SnapshotMMAt(dir).Processor {
		state = "processor_config.json present"
	}
	return fmt.Sprintf(" — %s: %s; launch allows %d images/prompt", dir, state, images)
}

func nLabel(n int) string {
	if n == 1 {
		return "1×1"
	}
	return fmt.Sprintf("%d", n)
}

// lookedIn names a directory without claiming anything was learned from it.
func lookedIn(dir string) string {
	if dir == "" {
		return ""
	}
	return " — looked in " + dir
}

func bodyHas(body string, subs ...string) bool {
	l := strings.ToLower(body)
	for _, s := range subs {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// HTTPPost is the production Post seam.
func HTTPPost(ctx context.Context, url, key string, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, string(b), nil
}
