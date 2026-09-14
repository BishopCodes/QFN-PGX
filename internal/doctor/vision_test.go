package doctor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/engine"
)

// The two halves of snapshot vision are independent facts: the tower in
// config.json, and the mm processor file next to it (the RadixArk port has
// the first and not the second). A missing dir — or no dir at all — is a
// checkpoint that hasn't been pulled: Config=false says "we learned nothing",
// which must never be reported as "no vision".
func TestSnapshotVisionReportsBothHalves(t *testing.T) {
	if mm := SnapshotMMAt(""); mm.Tower || mm.Processor || mm.Config {
		t.Fatalf(`"" must not read the CWD or claim a config: %+v`, mm)
	}
	dir := t.TempDir()
	if mm := SnapshotMMAt(dir); mm.Tower || mm.Processor || mm.Config {
		t.Fatalf("snapshot-less dir learned nothing, so it may not claim a config: %+v", mm)
	}
	text := t.TempDir()
	if err := os.WriteFile(filepath.Join(text, "config.json"), []byte(`{"architectures":["ForCausalLM"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if mm := SnapshotMMAt(text); mm.Tower || !mm.Config {
		t.Fatalf("a readable text-only config must report no tower WITH the config readable: %+v", mm)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"vision_config":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if mm := SnapshotMMAt(dir); !mm.Tower || mm.Processor {
		t.Fatalf("tower without the processor file must read tower-only: %+v", mm)
	}
	if err := os.WriteFile(filepath.Join(dir, "processor_config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if mm := SnapshotMMAt(dir); !mm.Tower || !mm.Processor {
		t.Fatalf("complete vision snapshot: %+v", mm)
	}
}

func TestVisionCheckClassification(t *testing.T) {
	cases := []struct {
		name     string
		deps     VisionDeps
		wantStat string
		wantSub  string
		where    string // msg or hint — a substring may not migrate between them
	}{
		{"ok", VisionDeps{Args: []string{"--limit-mm-per-prompt", `{"image": 4}`}, SnapshotHasVision: true, SnapshotReadable: true,
			Post: fakePost(200, `{"choices":[{}]}`, nil)}, "ok", "round-trip", "msg"},
		{"no vision config", VisionDeps{SnapshotHasVision: false, SnapshotReadable: true}, "warn", "no vision_config", "msg"},
		// An unreadable checkpoint is unknown, never "no vision tower".
		{"snapshot not readable", VisionDeps{SnapshotHasVision: false, SnapshotReadable: false}, "warn", "posture is unknown", "msg"},
		{"engine down", VisionDeps{SnapshotHasVision: true, SnapshotReadable: true, Args: nil}, "warn", "not running", "msg"},
		{"flag missing", VisionDeps{SnapshotHasVision: true, SnapshotReadable: true, Args: []string{"--max-model-len", "262144"}}, "bad", "without", "msg"},
		// images=0 is OUR config knob, and outranks any registry or checkpoint
		// blame: the lane was launched refusing images.
		{"limit zero", VisionDeps{SnapshotHasVision: true, SnapshotReadable: true,
			Args: []string{"--limit-mm-per-prompt", `{"image": 0}`},
			Post: fakePost(400, `At most 0 image(s) as input`, nil)}, "bad", "engine.images", "hint"},
		{"modality refused → snapshot mm files", VisionDeps{SnapshotHasVision: true, SnapshotReadable: true,
			Args: []string{"--limit-mm-per-prompt", `{"image": 4}`},
			Post: fakePost(400, `ValueError: model does not support modality 'image'`, nil)}, "bad", "processor_config.json", "hint"},
		{"matrix all pass", VisionDeps{SnapshotHasVision: true, SnapshotReadable: true,
			Args:   []string{"--limit-mm-per-prompt", `{"image": 16}`},
			Images: []int{1, 4, 16}, Post: fakePost(200, `{"choices":[{}]}`, nil)}, "ok", "probes", "msg"},
		{"matrix fails at 16", VisionDeps{SnapshotHasVision: true, SnapshotReadable: true,
			Args:   []string{"--limit-mm-per-prompt", `{"image": 16}`},
			Images: []int{1, 4, 16}, Post: fakePostSeq([]int{200, 200, 400}, `this model does not support that input`, nil)}, "bad", "16-image", "msg"},
		{"other error", VisionDeps{SnapshotHasVision: true, SnapshotReadable: true, Args: []string{"--limit-mm-per-prompt", "x"},
			Post: fakePost(500, `internal blast`, nil)}, "warn", "500", "msg"},
		{"conn refused", VisionDeps{SnapshotHasVision: true, SnapshotReadable: true, Args: []string{"--limit-mm-per-prompt", "x"},
			Post: fakePost(0, "", errors.New("connection refused"))}, "warn", "reach", "msg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.deps.Base == nil {
				tc.deps.Base = func() string { return "http://127.0.0.1:18300" }
			}
			if tc.deps.Key == nil {
				tc.deps.Key = func() string { return "k" }
			}
			ch := VisionCheck(context.Background(), tc.deps)
			got := ch.Msg
			if tc.where == "hint" {
				got = ch.Hint
			}
			if ch.Status != tc.wantStat {
				t.Errorf("status = %s, want %s (%s)", ch.Status, tc.wantStat, ch.Msg)
			}
			if !bodyHas(got, tc.wantSub) {
				t.Errorf("%s missing %q: %s / %s", tc.where, tc.wantSub, ch.Msg, ch.Hint)
			}
		})
	}
}

// A "fix the snapshot" hint is only actionable if it names the directory the
// check read (resolved through the same locator doctor's other checks use) and
// says whether processor_config.json is missing there.
func TestVisionHintNamesSnapshotDir(t *testing.T) {
	cache := t.TempDir()
	snap := filepath.Join(cache, "hub", "models--RadixArk--Qwen3.8-Flash-Next-NVFP4", "snapshots", "rev1")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	hint := func() string {
		return VisionCheck(context.Background(), VisionDeps{
			Base: func() string { return "http://x" }, Key: func() string { return "" },
			Locator:           engine.NewSnapshotLocator(cache),
			Engine:            config.Engine{Model: "RadixArk/Qwen3.8-Flash-Next-NVFP4", Mode: "nvfp4", Images: 4},
			SnapshotHasVision: true, SnapshotReadable: true,
			Args: []string{"--limit-mm-per-prompt", `{"image": 4}`},
			Post: fakePost(400, `ValueError: model does not support modality 'image'`, nil),
		}).Hint
	}
	if h := hint(); !strings.Contains(h, snap) || !strings.Contains(h, "MISSING") || !strings.Contains(h, "4 images/prompt") {
		t.Fatalf("missing file must name dir, status and image count: %s", h)
	}
	if err := os.WriteFile(filepath.Join(snap, "processor_config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if h := hint(); !strings.Contains(h, "present") {
		t.Fatalf("present file must say so: %s", h)
	}
	// No locator (nothing to read) must not guess a path.
	noDir := VisionCheck(context.Background(), VisionDeps{
		Base: func() string { return "http://x" }, Key: func() string { return "" },
		SnapshotHasVision: true,
		Args:              []string{"--limit-mm-per-prompt", `{"image": 4}`},
		Post:              fakePost(400, `ValueError: model does not support modality 'image'`, nil),
	}).Hint
	if strings.Contains(noDir, "launch allows") || strings.Contains(noDir, "MISSING") {
		t.Fatalf("unknown dir must append no note: %s", noDir)
	}
}

// The two causes of an image refusal are told apart by evidence, not by
// guessing which file is missing: the modalities the engine publishes outrank
// the checkpoint, the checkpoint's own mm manifests come second, and the
// engine's traceback line is quoted verbatim so a human can overrule both.
func TestVisionCheckRanksTheTwoCauses(t *testing.T) {
	flag := []string{"--limit-mm-per-prompt", `{"image": 4}`}
	yes := func(ctx context.Context) ([]string, bool) { return []string{"text", "image"}, true }
	no := func(ctx context.Context) ([]string, bool) { return []string{"text"}, true }
	shrug := func(ctx context.Context) ([]string, bool) { return nil, false }
	refused := fakePost(400, `ValueError: model does not support modality 'image'`, nil)
	trace := "2026-09-13 INFO serving.py:63 POST /v1/chat/completions\n"

	for _, tc := range []struct {
		name     string
		mods     func(context.Context) ([]string, bool)
		logs     func(context.Context) string
		complete bool // checkpoint also ships processor_config.json
		wantStat string
		wantSub  string
		where    string // msg or hint
	}{
		{"registry wiring: published modalities exclude image", no, nil, false, "bad", "text-only", "hint"},
		{"registry wiring named by arch, not by repo", no, nil, false, "bad", "qwen4_exp", "hint"},
		{"modalities unknown: fall back to the files", shrug, nil, false, "bad", "processor_config.json", "hint"},
		{"the port's missing manifest is the answer", yes, nil, false, "bad", "preprocessor_config.json without processor_config.json", "hint"},
		{"complete checkpoint: the traceback is the answer", yes, quote(trace + "RuntimeError: Failed to load the processor"), true, "bad", "Failed to load the processor", "msg"},
		{"the engine's own line is quoted", shrug, quote(trace + "ValueError: Qwen4ExpForConditionalGeneration is not a multimodal model"), false, "bad", "is not a multimodal model", "msg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A RadixArk-shaped checkpoint on disk: tower + preprocessor,
			// no processor manifest, model_type present so a registry hint
			// can name the arch instead of pointing at a repo.
			cache := t.TempDir()
			snap := filepath.Join(cache, "hub", "models--RadixArk--Qwen3.8-Flash-Next-NVFP4", "snapshots", "rev1")
			if err := os.MkdirAll(snap, 0o755); err != nil {
				t.Fatal(err)
			}
			for f, body := range map[string]string{"config.json": `{"model_type":"qwen4_exp","vision_config":{}}`, "preprocessor_config.json": `{}`} {
				if err := os.WriteFile(filepath.Join(snap, f), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.complete {
				if err := os.WriteFile(filepath.Join(snap, "processor_config.json"), []byte(`{}`), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ch := VisionCheck(context.Background(), VisionDeps{
				Base: func() string { return "http://x" }, Key: func() string { return "" },
				SnapshotHasVision: true, SnapshotReadable: true, Args: flag, Post: refused,
				Locator:    engine.NewSnapshotLocator(cache),
				Modalities: tc.mods, Logs: tc.logs,
				Engine: config.Engine{Model: "RadixArk/Qwen3.8-Flash-Next-NVFP4", Mode: "nvfp4", Images: 4},
			})
			got := ch.Msg
			if tc.where == "hint" {
				got = ch.Hint
			}
			if ch.Status != tc.wantStat || !strings.Contains(got, tc.wantSub) {
				t.Fatalf("status=%s %s=%q, want %s in %s", ch.Status, tc.where, got, tc.wantSub, tc.name)
			}
		})
	}
}

// "registry wired for images" is decided by the modalities the engine
// publishes; the checkpoint's two mm manifests are separate facts and only
// matter past that gate. ModelType is what lets a hint name the arch.
func TestSnapshotMMAtSeparatesTheManifests(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"model_type":"qwen4_exp","vision_config":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "preprocessor_config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mm := SnapshotMMAt(dir)
	if !mm.Tower || mm.Processor || !mm.Preprocessor || mm.ModelType != "qwen4_exp" || !mm.Config {
		t.Fatalf("RadixArk-port shape reads wrong: %+v", mm)
	}
	if err := os.WriteFile(filepath.Join(dir, "processor_config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if mm = SnapshotMMAt(dir); !mm.Processor {
		t.Fatalf("nvidia-port shape reads wrong: %+v", mm)
	}
}

// tracebackLine quotes the exception, not the log: the last error-ish line is
// the one naming the failing class, and a clean tail must add nothing.
func TestTracebackLine(t *testing.T) {
	tail := "INFO 09-13 serving.py:63 POST /v1/chat/completions\n"
	tail += "Traceback (most recent call last):\n  File \"registry.py\", line 297\n"
	tail += "ValueError: Qwen4ExpForConditionalGeneration is not a multimodal model\n"
	if got := tracebackLine(tail); !strings.Contains(got, "is not a multimodal model") {
		t.Fatalf("want the exception line, got %q", got)
	}
	if got := tracebackLine("INFO 09-13 serving.py:63 started\n\n"); got != "" {
		t.Fatalf("no exception must yield nothing, got %q", got)
	}
	if got := tracebackLine(""); got != "" {
		t.Fatalf("empty log must yield nothing, got %q", got)
	}
}

// Absent or renamed field is "the build says nothing", which is not "the model
// cannot see". Reading a missing field as no-vision would misdiagnose every
// engine older than the field itself.
func TestDeclaredModalitiesUnknownIsNotNoVision(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
		ok   bool
	}{
		{"current field", `{"data":[{"id":"qwen38","supported_input_modalities":["text","image"]}]}`, []string{"text", "image"}, true},
		{"legacy field", `{"data":[{"id":"qwen38","supported_modalities":["text"]}]}`, []string{"text"}, true},
		{"older ModelCard, no field", `{"data":[{"id":"qwen38","object":"model"}]}`, nil, false},
		{"not json", `<html>502</html>`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("path = %s, want /v1/models", r.URL.Path)
				}
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got, ok := DeclaredModalities(context.Background(), srv.URL, "k")
			if ok != tc.ok || strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v,%v want %v,%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// quote adapts a fixed log tail to the Logs seam.
func quote(s string) func(context.Context) string {
	return func(context.Context) string { return s + "\n" }
}

func fakePost(code int, body string, err error) func(context.Context, string, string, []byte) (int, string, error) {
	return func(context.Context, string, string, []byte) (int, string, error) { return code, body, err }
}

// fakePostSeq returns per-call codes (last code repeats) — matrix probes.
func fakePostSeq(codes []int, body string, err error) func(context.Context, string, string, []byte) (int, string, error) {
	i := 0
	return func(context.Context, string, string, []byte) (int, string, error) {
		c := codes[len(codes)-1]
		if i < len(codes) {
			c = codes[i]
		}
		i++
		return c, body, err
	}
}

// The file answer may only be asserted when the file is actually missing — a
// checkpoint that ships all its manifests must not be told to copy one in —
// and the build-wiring premise may only be stated as fact when the engine
// published the image modality; with no modalities published it is a guess.
func TestRefusalHintAssertsOnlyWhatItKnows(t *testing.T) {
	complete := SnapshotMM{Tower: true, Preprocessor: true, Processor: true, Config: true, ModelType: "qwen4_exp"}
	if h := refusalHint(complete, "", 4, true); strings.Contains(h, "without processor_config.json") {
		t.Fatalf("a complete checkpoint must not be told to copy a file it has: %q", h)
	}
	port := complete
	port.Processor = false
	if h := refusalHint(port, "", 4, true); !strings.Contains(h, "without processor_config.json") {
		t.Fatalf("the port's missing manifest must be named: %q", h)
	}
	if h := refusalHint(port, "", 4, false); strings.Contains(h, "this checkpoint cannot resolve") {
		t.Fatalf("with no published modalities the build wiring is a hypothesis: %q", h)
	}
}

// mmLimit reads the knob the repo itself sets: spec.go emits
// --limit-mm-per-prompt {"image": 0} for engine.images=0, and reading that as
// "flag present" would blame the build for a config value.
func TestMLimitReadsTheImageCount(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		n    int
		ok   bool
	}{
		{[]string{"--limit-mm-per-prompt", `{"image": 4}`}, 4, true},
		{[]string{"--limit-mm-per-prompt", `{"image": 0}`}, 0, true},
		{[]string{"--limit-mm-per-prompt=" + `{"image":2}`}, 2, true},
		{[]string{"--limit-mm-per-prompt", `{"image": 4, "video": 0}`}, 4, true},
		{[]string{"--max-model-len", "262144"}, 0, false},
		{[]string{"--limit-mm-per-prompt", "image=4"}, 0, false},
	} {
		n, ok := mmLimit(tc.argv)
		if n != tc.n || ok != tc.ok {
			t.Errorf("%v -> %d,%v want %d,%v", tc.argv, n, ok, tc.n, tc.ok)
		}
	}
}
