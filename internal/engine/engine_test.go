package engine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BishopCodes/qfn-pgx/internal/config"
)

type fakeLoc struct{}

func (fakeLoc) SnapshotInContainer(e config.Engine) (string, []string, error) {
	if e.Mode == "hybrid" {
		return "/hf/hub/models--RadixArk--Qwen3.8-Flash-Next-NVFP4/snapshots/rev1-fp8hybrid",
			[]string{"-e", "VLLM_FP8_HYBRID=1", "-e", "VLLM_USE_DEEP_GEMM=0"}, nil
	}
	return "/hf/hub/models--RadixArk--Qwen3.8-Flash-Next-NVFP4/snapshots/rev1", nil, nil
}

func defaultArgs(t *testing.T, e config.Engine, o LaunchOpts) []string {
	t.Helper()
	args, err := DockerArgs(e, fakeLoc{}, o)
	if err != nil {
		t.Fatal(err)
	}
	return args
}

func TestDockerArgsMirrorsServeSh(t *testing.T) {
	e := config.Defaults().Engine
	args := defaultArgs(t, e, LaunchOpts{EngineAPIKey: "k", HFCacheHost: "/hf"})
	joined := strings.Join(args, " ")

	// Run-mode flags.
	for _, want := range []string{
		"run -d --name qwen38-flash --restart unless-stopped",
		"--gpus all --ipc=host --shm-size 16g",
		"-p 127.0.0.1:18300:8000", // loopback bind = the QFN-PGX delta
		"-v /hf:/hf -e HF_HOME=/hf -e HF_HUB_OFFLINE=1",
		"-e VLLM_PLE_MMAP=1 -e VLLM_PLE_MMAP_WORKERS=32 -e VLLM_PLE_MMAP_PREWARM=0",
		"-e VLLM_QSA_EXACT_TOPK=1",
		"-e VLLM_USE_FLASHINFER_SAMPLER=1 -e VLLM_ALLOW_LONG_MAX_MODEL_LEN=0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	// Engine argv in serve.sh order.
	enginePart := joined[strings.Index(joined, "qwen38-flash-dgx"):]
	for _, want := range []string{
		"qwen38-flash-dgx /hf/hub/models--RadixArk--Qwen3.8-Flash-Next-NVFP4/snapshots/rev1 --served-model-name qwen3.8-flash-next",
		"--host 0.0.0.0 --port 8000 --load-format safetensors",
		"--max-model-len 262144 --max-num-seqs 8 --gpu-memory-utilization 0.85",
		"--enable-prefix-caching --enable-chunked-prefill --max-num-batched-tokens 8192",
		"-cc.cudagraph_mode=PIECEWISE",
		"-cc.splitting_ops=[\"vllm::unified_attention_with_output\"",
		"\"vllm::ple_mmap_lookup\"]",
		"--no-enable-flashinfer-autotune --kv-cache-dtype auto",
		"--enable-auto-tool-choice --tool-call-parser qwen3_coder --reasoning-parser qwen3",
		`--speculative-config {"method":"mtp","num_speculative_tokens":2}`,
		"--api-key k", // lockdown delta, appended last
	} {
		if !strings.Contains(enginePart, want) {
			t.Errorf("engine argv missing %q", want)
		}
	}
	// Ordering guard: lockdown key must not end up before EXTRA-style overrides.
	keyIdx := slices.Index(args, "--api-key")
	specIdx := slices.Index(args, "--speculative-config")
	if keyIdx < specIdx {
		t.Error("--api-key must follow --speculative-config (serve.sh tail ordering)")
	}
	// No hybrid env in nvfp4 mode.
	if strings.Contains(joined, "VLLM_FP8_HYBRID") {
		t.Error("nvfp4 mode must not carry hybrid env")
	}
}

func TestDockerArgsHybridYarnMTP(t *testing.T) {
	e := config.Defaults().Engine
	e.Mode = "hybrid"
	e.Yarn = true
	e.Ctx = 500000
	e.GpuMem = 0.80
	e.Prewarm = true
	args := defaultArgs(t, e, LaunchOpts{EngineAPIKey: "kk", HFCacheHost: "/hf"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "VLLM_FP8_HYBRID=1 -e VLLM_USE_DEEP_GEMM=0") {
		t.Error("hybrid env missing")
	}
	if !strings.Contains(joined, "snapshots/rev1-fp8hybrid") {
		t.Error("hybrid snapshot dir missing")
	}
	if !strings.Contains(joined, "--hf-overrides {\"text_config\"") {
		t.Error("yarn overrides missing")
	}
	if !strings.Contains(joined, `"num_speculative_tokens":2,"max_model_len":500000`) {
		t.Error("MTP+YaRN draft max_model_len fix missing")
	}
	if !strings.Contains(joined, "VLLM_ALLOW_LONG_MAX_MODEL_LEN=1") {
		t.Error("yarn must allow long max len")
	}
	if !strings.Contains(joined, "--gpu-memory-utilization 0.8 ") && !strings.HasSuffix(joined, "0.8") {
		if !strings.Contains(joined, "--gpu-memory-utilization 0.8") {
			t.Error("0.80 must render as 0.8")
		}
	}
}

func TestDockerArgsNoPrefixCacheAndNoMTP(t *testing.T) {
	e := config.Defaults().Engine
	e.PrefixCache = false
	e.MTP = 0
	e.Lockdown = false
	args := defaultArgs(t, e, LaunchOpts{HFCacheHost: "/hf"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--no-enable-prefix-caching") {
		t.Error("--no-enable-prefix-caching missing")
	}
	if strings.Contains(joined, "--speculative-config") {
		t.Error("MTP=0 must omit speculative config")
	}
	if strings.Contains(joined, "--api-key") {
		t.Error("lockdown off must omit --api-key")
	}
}

func TestDockerArgsLockdownRequiresKey(t *testing.T) {
	e := config.Defaults().Engine
	if _, err := DockerArgs(e, fakeLoc{}, LaunchOpts{HFCacheHost: "/hf"}); err == nil {
		t.Fatal("lockdown without key must refuse to build argv")
	}
}

func TestDockerArgsExtraAndCaps(t *testing.T) {
	e := config.Defaults().Engine
	e.Extra = "--disable-log-requests --swap-space 8"
	e.CPUSet = "5-9,15-19"
	e.ContainerMem = "100g"
	args := defaultArgs(t, e, LaunchOpts{EngineAPIKey: "k", HFCacheHost: "/hf"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--cpuset-cpus 5-9,15-19 --memory 100g --memory-swap 100g") {
		t.Errorf("caps missing: %s", joined)
	}
	// EXTRA is appended before the tail (argparse last-wins for it upstream too).
	iExtra := slices.Index(args, "--disable-log-requests")
	iTail := slices.Index(args, "--enable-auto-tool-choice")
	if iExtra+1 >= iTail || args[iExtra+1] != "--swap-space" {
		t.Error("EXTRA flags must be split and ordered verbatim before the tail")
	}
}

// ---- snapshot locator ----

func TestSnapshotLocator(t *testing.T) {
	hf := t.TempDir()
	rev := "abc123"
	plain := filepath.Join(hf, "hub", "models--RadixArk--Qwen3.8-Flash-Next-NVFP4", "snapshots", rev)
	hyb := plain + "-fp8hybrid"
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	loc := NewSnapshotLocator(hf)
	e := config.Defaults().Engine

	snap, env, err := loc.SnapshotInContainer(e)
	if err != nil || !strings.HasSuffix(snap, "/snapshots/"+rev) || len(env) != 0 {
		t.Fatalf("nvfp4 resolve: %q %v %v", snap, env, err)
	}

	e.Mode = "hybrid"
	if _, _, err := loc.SnapshotInContainer(e); err == nil || !strings.Contains(err.Error(), "prepare-hybrid") {
		t.Fatalf("hybrid without .prepared must hint prepare-hybrid, got %v", err)
	}
	if err := os.MkdirAll(hyb, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hyb, ".prepared"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	snap, env, err = loc.SnapshotInContainer(e)
	if err != nil || !strings.HasSuffix(snap, rev+"-fp8hybrid") || !slices.Contains(env, "VLLM_FP8_HYBRID=1") {
		t.Fatalf("hybrid resolve: %q %v %v", snap, env, err)
	}

	// Missing cache entirely.
	loc2 := NewSnapshotLocator(filepath.Join(t.TempDir(), "nope"))
	if _, _, err := loc2.SnapshotInContainer(config.Defaults().Engine); err == nil ||
		!strings.Contains(err.Error(), "qfn pull") {
		t.Fatalf("missing snapshot must hint `qfn pull`, got %v", err)
	}
}

func TestStatusLocator(t *testing.T) {
	hf := t.TempDir()
	plain := filepath.Join(hf, "hub", "models--RadixArk--Qwen3.8-Flash-Next-NVFP4", "snapshots", "rev1")
	os.MkdirAll(plain, 0o755)
	os.MkdirAll(plain+"-fp8hybrid", 0o755)
	os.WriteFile(filepath.Join(plain+"-fp8hybrid", ".prepared"), nil, 0o644)
	pl := NewSnapshotLocator(hf).(*pathLocator)
	st := pl.Status(config.Defaults().Engine)
	if !st.RepoExists || st.Snapshot != "rev1" || !st.HybridPrepared {
		t.Fatalf("%+v", st)
	}
}

// ---- docker argv capture ----

type captureDocker struct {
	runs [][]string
}

func (c *captureDocker) Run(ctx context.Context, args ...string) (string, error) {
	c.runs = append(c.runs, args)
	return "", nil
}
func (c *captureDocker) FollowLogs(ctx context.Context, name string, w io.Writer) error {
	return nil
}

func TestManagerOpQueue(t *testing.T) {
	d := &captureDocker{}
	m := NewManager(d)
	op, err := m.TryBegin("up", "cli")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.TryBegin("up", "serve"); err == nil {
		t.Fatal("second op must be refused while one is in flight")
	}
	if err := m.Up(context.Background(), op, []string{"-d", "img"}, "c1"); err != nil {
		t.Fatal(err)
	}
	if len(d.runs) != 2 || d.runs[0][0] != "rm" || d.runs[1][0] != "run" {
		t.Fatalf("expected rm -f then run: %v", d.runs)
	}
	if last, ok := m.LastOp(); !ok || !last.Done {
		t.Fatalf("op not finished: %+v", last)
	}
	if _, err := m.TryBegin("down", "serve"); err != nil {
		t.Fatalf("queue must free up after finish: %v", err)
	}
}

// ---- boot log ----

func TestBootTracker(t *testing.T) {
	bt := &BootTracker{}
	feed := func(lines ...string) Phase {
		var p Phase
		for _, l := range lines {
			p, _ = bt.Feed(l)
		}
		return p
	}
	if p := feed("(EngineCore) Loading safetensors checkpoint shards:  42% Completed | 8/19"); p != PhaseWeights {
		t.Fatalf("weights phase, got %v", p)
	}
	if bt.detail != "shards 8/19" {
		t.Fatalf("shard progress: %q", bt.detail)
	}
	if p := feed("Capturing CUDA graph shapes"); p != PhaseGraphs {
		t.Fatalf("graphs phase, got %v", p)
	}
	// Monotonic: an older-marker line must not demote.
	if p := feed("Loading safetensors checkpoint shards:  10% Completed | 1/19"); p != PhaseGraphs {
		t.Fatalf("phase demoted: %v", p)
	}
	if p := feed("INFO: Application startup complete."); p != PhaseReady {
		t.Fatalf("ready phase, got %v", p)
	}
	// Failure markers win regardless of position.
	bt2 := &BootTracker{}
	bt2.Feed("INFO Application startup complete.")
	if p, _ := bt2.Feed("Traceback (most recent call last):"); p != PhaseFailed {
		t.Fatalf("failure must override ready, got %v", p)
	}
}
