// Package engine reproduces the upstream serve.sh docker invocation
// (engine/ATTRIBUTION.md) exactly, adds QFN-PGX's loopback-bind and
// API-key lockdown deltas, and models container lifecycle for the CLI and
// the web console through one shared code path.
package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/BishopCodes/qfn-pgx/internal/config"
)

// ServedModelName matches the hardcoded name in upstream serve.sh.
const ServedModelName = "qwen3.8-flash-next"

// Splitting-ops list, verbatim from serve.sh: the PLE gather is a CPU op +
// pageable copy and MUST run outside CUDA graphs — PIECEWISE capture only.
const splittingOps = `["vllm::unified_attention_with_output","vllm::unified_mla_attention_with_output","vllm::mamba_mixer2","vllm::mamba_mixer","vllm::short_conv","vllm::qwen3_8_flash_next_ple_short_conv","vllm::qwen3_8_flash_next_qsa_with_output","vllm::linear_attention","vllm::qwen_gdn_attention_core","vllm::qwen_gdn_attention_core_fused_norm_packed","vllm::sparse_attn_indexer","vllm::ple_mmap_lookup"]`

// yarnOverrides: YaRN (Qwen's published recipe), byte-identical to serve.sh.
const yarnOverrides = `{"text_config": {"rope_parameters": {"mrope_interleaved": true, "mrope_section": [11, 11, 10], "rope_type": "yarn", "rope_theta": 10000000, "partial_rotary_factor": 0.25, "factor": 4.0, "original_max_position_embeddings": 262144}}}`

// LaunchOpts carries the lockdown material that serve.sh never had.
type LaunchOpts struct {
	// EngineAPIKey is passed as --api-key when Engine.Lockdown is true.
	// Manager refuses to launch in lockdown mode with an empty key.
	EngineAPIKey string
	// HFCacheHost is the host-side HF cache dir (Paths.hf_cache, expanded).
	HFCacheHost string
}

// DockerArgs builds the full `docker run …` argv for the resolved engine
// config. It mirrors serve.sh ordering so `qfn`-managed containers are
// indistinguishable from the shell scripts except for the documented deltas:
//   - `-p BIND:PORT:8000` (upstream binds all interfaces)
//   - optional `--api-key` when lockdown is on
//   - optional `--memory/--memory-swap` when container_mem_cap is set
//   - optional `--cpuset-cpus` when cpuset is set
func DockerArgs(e config.Engine, loc SnapshotLocator, o LaunchOpts) ([]string, error) {
	if e.Lockdown && o.EngineAPIKey == "" {
		return nil, fmt.Errorf("engine.lockdown is on but no engine API key is available (run `qfn init` or `qfn lockdown on` again)")
	}
	if o.HFCacheHost == "" {
		// "-v :/hf" surfaces as a baffled docker error; name the real cause.
		return nil, errors.New("empty HF cache host path — check paths.hf_cache (`qfn config get paths.hf_cache`)")
	}
	snapIn, hybridEnv, err := loc.SnapshotInContainer(e)
	if err != nil {
		return nil, err
	}

	allowLong := "0"
	if e.Yarn {
		allowLong = "1"
	}

	args := []string{
		"run", "-d", "--name", e.Name, "--restart", "unless-stopped",
		"--gpus", "all", "--ipc=host", "--shm-size", "16g",
		"-p", fmt.Sprintf("%s:%d:8000", e.Bind, e.Port),
	}
	if e.CPUSet != "" {
		args = append(args, "--cpuset-cpus", e.CPUSet)
	}
	if e.ContainerMem != "" {
		// Charges page cache to the container — deliberate opt-in only (see doctor).
		args = append(args, "--memory", e.ContainerMem, "--memory-swap", e.ContainerMem)
	}
	args = append(args, hybridEnv...)
	args = append(args,
		"-v", o.HFCacheHost+":/hf",
		"-e", "HF_HOME=/hf", "-e", "HF_HUB_OFFLINE=1",
		"-e", "VLLM_PLE_MMAP=1",
		"-e", "VLLM_PLE_MMAP_WORKERS="+strconv.Itoa(e.Workers),
		"-e", "VLLM_PLE_MMAP_PREWARM="+b01(e.Prewarm),
		"-e", "VLLM_QSA_EXACT_TOPK="+b01(e.ExactTopK),
		"-e", "VLLM_USE_FLASHINFER_SAMPLER=1",
		"-e", "VLLM_ALLOW_LONG_MAX_MODEL_LEN="+allowLong,
		e.Image,
		snapIn, "--served-model-name", ServedModelName,
		"--host", "0.0.0.0", "--port", "8000", "--load-format", "safetensors",
		"--max-model-len", strconv.Itoa(e.Ctx),
		"--max-num-seqs", strconv.Itoa(e.Seqs),
		"--gpu-memory-utilization", strconv.FormatFloat(e.GpuMem, 'g', -1, 64),
	)
	if e.PrefixCache {
		args = append(args, "--enable-prefix-caching")
	} else {
		args = append(args, "--no-enable-prefix-caching")
	}
	// Vision is opt-in in vLLM's CLI contract; be explicit either way so the
	// engine's accepted modalities are visible in the argv and stable across
	// vLLM versions (defaults for unspecified modalities have moved before).
	args = append(args,
		"--enable-chunked-prefill", "--max-num-batched-tokens", "8192",
		"--limit-mm-per-prompt", fmt.Sprintf(`{"image": %d}`, max(e.Images, 0)),
		"-cc.cudagraph_mode=PIECEWISE",
		"-cc.splitting_ops="+splittingOps,
		"--no-enable-flashinfer-autotune",
		"--kv-cache-dtype", e.KVDtype,
	)
	if e.Yarn {
		args = append(args, "--hf-overrides", yarnOverrides)
	}
	if e.Extra != "" {
		args = append(args, strings.Fields(e.Extra)...)
	}
	args = append(args,
		"--enable-auto-tool-choice", "--tool-call-parser", "qwen3_coder", "--reasoning-parser", "qwen3",
	)
	if e.MTP > 0 {
		spec := fmt.Sprintf(`{"method":"mtp","num_speculative_tokens":%d}`, e.MTP)
		if e.Yarn {
			// serve.sh: dict hf_overrides do not propagate to the draft model;
			// forcing the draft's max_model_len through the spec config fixes
			// the YaRN+MTP abort.
			spec = fmt.Sprintf(`{"method":"mtp","num_speculative_tokens":%d,"max_model_len":%d}`, e.MTP, e.Ctx)
		}
		args = append(args, "--speculative-config", spec)
	}
	if e.Lockdown {
		args = append(args, "--api-key", o.EngineAPIKey)
	}
	return args, nil
}

func b01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
