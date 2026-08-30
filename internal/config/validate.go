package config

import (
	"fmt"
	"net"
	"regexp"
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
		return fmt.Errorf("engine.kv_dtype must be \"auto\" or \"fp8\" (fp8 is refused by the QSA layers — keep auto unless experimenting)")
	}
	if c.Engine.Workers < 1 || c.Engine.Workers > 128 {
		return fmt.Errorf("engine.workers must be 1..128")
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
