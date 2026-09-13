package config

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// dockerSize matches docker's --memory shorthands like "100g" / "512m" / bytes.
var dockerSize = regexp.MustCompile(`^[0-9]+([bkmgBKMG]?)$`)

// Validate reports the first invalid field; the wizard calls it per step and
// Load calls it after decode so an invalid file never launches anything.
func (c Config) Validate() error {
	switch c.Engine.Mode {
	case "nvfp4", "hybrid":
	default:
		return fmt.Errorf("engine.mode must be \"nvfp4\" or \"hybrid\", got %q", c.Engine.Mode)
	}
	if c.Engine.MTP < 0 || c.Engine.MTP > 4 {
		return fmt.Errorf("engine.mtp must be 0..4 (MTP head trained for 3 steps; 2 is this checkpoint's measured peak)")
	}
	if c.Engine.MTPAdaptive != "" {
		if c.Engine.MTP <= 0 {
			return fmt.Errorf("engine.mtp_adaptive requires engine.mtp > 0 (it only reshapes speculation per batch size)")
		}
		if err := validAdaptive(c.Engine.MTPAdaptive); err != nil {
			return fmt.Errorf("engine.mtp_adaptive %q: %v (want \"N:LO-HI[,N:LO-HI…]\", e.g. \"3:1-4,1:5-16\")", c.Engine.MTPAdaptive, err)
		}
	}
	if c.Engine.Seqs < 1 || c.Engine.Seqs > 64 {
		return fmt.Errorf("engine.seqs must be 1..64")
	}
	if c.Engine.Ctx < 4096 || c.Engine.Ctx > 1048576 {
		return fmt.Errorf("engine.ctx must be 4096..1048576")
	}
	if c.Engine.Ctx > 262144 && !c.Engine.Yarn {
		return fmt.Errorf("engine.ctx %d exceeds the native 262144 window — set engine.yarn = true (validated with YaRN up to 500000)", c.Engine.Ctx)
	}
	if c.Engine.GpuMem <= 0 || c.Engine.GpuMem > 0.875 {
		return fmt.Errorf("engine.gpu_mem must be in (0, 0.875] — 0.875 OOM-killed on a 300k prefill with MTP upstream; keep the margin (0.80 for long-running service)")
	}
	switch c.Engine.KVDtype {
	case "auto", "fp8":
	default:
		return fmt.Errorf("engine.kv_dtype must be \"auto\" or \"fp8\" (fp8 needs a `qfn build --b12x` image — the QSA bridge is build-gated; bench quality before serving)")
	}
	if c.Engine.Workers < 1 || c.Engine.Workers > 128 {
		return fmt.Errorf("engine.workers must be 1..128")
	}
	if c.Engine.ReadAhead < 0 {
		return fmt.Errorf("engine.ple_readahead must be >= 0 (0 = off; 2048 = upstream Spark-tuned)")
	}
	if err := validBind("engine.bind", c.Engine.Bind); err != nil {
		return err
	}
	if err := validBind("serve.bind", c.Serve.Bind); err != nil {
		return err
	}
	if c.Engine.Port < 1 || c.Engine.Port > 65535 {
		return fmt.Errorf("engine.port must be 1..65535")
	}
	if c.Serve.Port < 1 || c.Serve.Port > 65535 {
		return fmt.Errorf("serve.port must be 1..65535")
	}
	if c.Serve.Port == c.Engine.Port {
		return fmt.Errorf("serve.port and engine.port must differ (console :%d collides with the engine)", c.Serve.Port)
	}
	if c.Engine.ContainerMem != "" && !dockerSize.MatchString(c.Engine.ContainerMem) {
		return fmt.Errorf("engine.container_mem_cap must look like \"100g\"/\"512m\" or plain bytes (leave empty unless you know why — see `qfn doctor`)")
	}
	if c.Serve.RequireAPIKey && len(c.Serve.APIKeys) == 0 {
		return fmt.Errorf("serve.require_api_key is true but no named keys exist yet (named machine keys land with the api_keys table; use `qfn config set serve.require_api_key false` until then)")
	}
	if !c.Serve.AuthEnabled && !IsLoopbackBind(c.Serve.Bind) {
		return fmt.Errorf("serve.auth_enabled cannot be false while serve.bind %q exposes the console beyond loopback", c.Serve.Bind)
	}
	return nil
}

func validBind(field, bind string) error {
	if bind == "localhost" {
		return nil
	}
	if ip := net.ParseIP(bind); ip == nil {
		return fmt.Errorf("%s must be an IP (e.g. 127.0.0.1, 0.0.0.0) or \"localhost\", got %q", field, bind)
	}
	return nil
}

// validAdaptive checks the "3:1-4,1:5-16" draft policy grammar: one or more
// comma-separated tokens:<lo>-<hi> ranges, lo<=hi, tokens 1..4, ranges strictly
// ascending and non-overlapping (vLLM's num_speculative_tokens_per_batch_size).
func validAdaptive(s string) error {
	var prevHi int
	for _, part := range strings.Split(s, ",") {
		tok, rng, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			return fmt.Errorf("missing ':' in %q", part)
		}
		t, err := strconv.Atoi(strings.TrimSpace(tok))
		if err != nil || t < 1 || t > 4 {
			return fmt.Errorf("draft tokens must be 1..4 in %q", part)
		}
		loS, hiS, ok := strings.Cut(strings.TrimSpace(rng), "-")
		if !ok {
			return fmt.Errorf("batch range must look like 1-4 in %q", part)
		}
		lo, err := strconv.Atoi(strings.TrimSpace(loS))
		if err != nil || lo < 1 {
			return fmt.Errorf("bad range low in %q", part)
		}
		hi, err := strconv.Atoi(strings.TrimSpace(hiS))
		if err != nil || hi < lo {
			return fmt.Errorf("bad range high in %q", part)
		}
		if lo <= prevHi {
			return fmt.Errorf("ranges must ascend without overlap (after %d)", prevHi)
		}
		prevHi = hi
	}
	return nil
}
