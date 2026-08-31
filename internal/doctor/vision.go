// Vision probe: end-to-end "can this engine take an image?" evidence.
//
// Image support needs three independent things to line up, and each failing
// mode says something different — so we check all three instead of guessing:
//  1. the snapshot's config.json declares vision_config (weights + arch)
//  2. the running container's argv declares --limit-mm-per-prompt
//  3. the live engine actually accepts a 1×1-PNG request
//
// Upstream fact worth knowing: multimodal wiring for this architecture
// landed in vLLM on 2026-08-31 ([Model] Support Qwen3.8-Flash-Next, #53896).
// Engine images pinned before that boot, serve text, and hold the visual
// weights — yet still refuse images: the model class predates the mm registry.
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
	Post              func(ctx context.Context, url, key string, body []byte) (int, string, error)
}

func visionBody(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "reply with the single word: ok"},
			map[string]any{"type": "image_url", "image_url": map[string]string{
				"url": "data:image/png;base64," + probePNGb64}},
		}}},
		"max_tokens": 4, "stream": false,
	})
	return b
}

// VisionCheck runs the three-stage probe and classifies the failure mode.
func VisionCheck(ctx context.Context, d VisionDeps) Check {
	c := Check{ID: "vision"}
	if !d.SnapshotHasVision {
		c.Status, c.Msg = "warn", "snapshot config.json has no vision_config — image input impossible with this snapshot"
		c.Hint = "use the full NVFP4 snapshot (RadixArk/Qwen3.8-Flash-Next-NVFP4)"
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
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	status, body, err := d.Post(ctx, strings.TrimRight(d.Base(), "/")+"/v1/chat/completions", d.Key(), visionBody(d.Model))
	if err != nil {
		c.Status, c.Msg = "warn", "vision probe could not reach the engine: "+errOr(err, body)
		return c
	}
	switch {
	case status < 400:
		c.Status, c.Msg = "ok", "engine accepts image input (1×1 probe round-trip)"
	case bodyHas(body, "at most 0", "not allowed"):
		c.Status, c.Msg = "bad", "engine rejects images: limit is 0 despite our flag (flag syntax silently ignored?)"
		c.Hint = "check the container start line in the engine logs"
	case bodyHas(body, "not supported", "not a multimodal", "does not support", "no modality"):
		c.Status, c.Msg = "bad", "engine BUILD predates multimodal support for this architecture"
		c.Hint = "vLLM wired it upstream 2026-08-31 (#53896) — update the pinned base image and rebuild: `qfn build` (then `qfn up`)"
	default:
		c.Status, c.Msg = "warn", fmt.Sprintf("vision probe failed (HTTP %d): %s", status, firstLine(shorten(body)))
		c.Hint = "engine's verbatim error above; run `qfn chat --image file.png \"describe\"` for a fuller probe"
	}
	return c
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
