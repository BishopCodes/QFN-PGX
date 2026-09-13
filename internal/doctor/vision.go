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
// Upstream fact worth knowing: multimodal wiring for this architecture
// merged into vLLM main on 2026-08-31 (#53896), but receipts of independent
// recipes on THIS pinned base digest (fc120ece) serve image input on SM120
// and SM121 — the release/qwen38next branch that built the recipe image
// evidently already carried the wiring. So a "not supported" answer today
// points at the SNAPSHOT's mm files (RadixArk's port lacks
// processor_config.json), not the engine build.
package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 1x1 PNG, red.
const probePNGb64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// VisionDeps wires the probe; all seams are injectable for tests.
type VisionDeps struct {
	Base              func() string // engine base URL (direct, not console)
	Key               func() string // engine API key
	Model             string
	Args              []string // running container argv ("" if engine not up)
	SnapshotHasVision bool
	Images            []int // probe battery; nil/empty = [1]
	Post              func(ctx context.Context, url, key string, body []byte) (int, string, error)
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
	if !d.SnapshotHasVision {
		c.Status, c.Msg = "warn", "snapshot config.json has no vision_config — image input impossible with this snapshot"
		c.Hint = "both published NVFP4 snapshots carry the vision tower: RadixArk/Qwen3.8-Flash-Next-NVFP4 and nvidia/Qwen3.8-Flash-Next-NVFP4 — a snapshot without vision_config means the wrong repo was pulled"
		return c
	}
	hasFlag := false
	for _, a := range d.Args {
		if strings.Contains(a, "limit-mm-per-prompt") {
			hasFlag = true
		}
	}
	if len(d.Args) == 0 {
		c.Status, c.Msg = "warn", "engine not running — start it (`qfn up`) and run doctor again"
		return c
	}
	if !hasFlag {
		c.Status, c.Msg = "bad", "running engine was launched WITHOUT --limit-mm-per-prompt"
		c.Hint = "the container predates the images flag — `qfn restart` (or stop + up) to relaunch with new argv"
		return c
	}
	for _, n := range ns {
		pctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		status, body, err := d.Post(pctx, strings.TrimRight(d.Base(), "/")+"/v1/chat/completions", d.Key(), visionBody(d.Model, n))
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
			c.Hint = "receipts show THIS pinned base serving images (tpurtell recipe, same digest, 1/4/16 probes) — so suspect the snapshot's mm files first: the RadixArk port lacks processor_config.json (copy it from nvidia/Qwen3.8-Flash-Next-NVFP4 into the snapshot dir, or serve that snapshot)"
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

func nLabel(n int) string {
	if n == 1 {
		return "1×1"
	}
	return fmt.Sprintf("%d", n)
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
