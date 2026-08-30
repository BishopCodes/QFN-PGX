# QFN-PGX

**Qwen3.8-Flash-Next 176B on one DGX Spark — as one static Go binary.**

A PLE-mmap vLLM lane (weights stream from NVMe through the page cache, so a
176B MoE fits one GB10's 128 GB unified pool), wrapped in an interactive CLI
and an always-on web console. Born from
[qwen3.8-Flash-DGX](https://github.com/BishopCodes/BishopCodes/qwen3.8-Flash-DGX)
plus the operational tricks from
[dgx-spark-qwen38](https://github.com/BishopCodes/dgx-spark-qwen38) — the
keepalive proxy, memory guards, boot parsing, and the systemd machinery,
rebuilt in Go with a fifth of the footprint (this box is already in swap just
running the model; the console shouldn't add to it).

```
$ qfn init          # defaults wizard → config.toml + age-encrypted password
$ qfn build         # vendored Dockerfile → qwen38-flash-dgx image
$ qfn pull          # ~122 GiB checkpoint into the HF cache (resumable)
$ qfn up            # memory-guarded launch; boot in ~10 min
$ qfn serve         # console + proxy on http://localhost:8799
$ sudo qfn service install   # reboot-persistent: start the engine from the browser
```

## What you get

**`qfn` CLI** — one static binary (no Node, no glibc deps, ~12 MB). A first-run
wizard walks every default with its trade-off visible (YaRN above 262k,
gpu_mem ≤ 0.875, why MTP 2 is this checkpoint's sweet spot, hybrid fp8 side
layers vs. published nvfp4). `qfn up` refuses to launch when the pool can't
hold the model (the GB10 memory trap, guarded), and profiles layer over
`config.toml` like git branches: `qfn up long-ctx`.

**Web console (`:8799`)** — model status with one-click start/restart/stop
(the same preflight-guarded launch path the CLI uses; boot phase + log tail
stream live), a backpressure banner wired to the signals that actually predict
vLLM trouble (swap %, PSI mem/io, KV pressure), per-core CPU, the container's
true pool charge via its cgroup, NVMe read rate (that's PLE streaming), and a
requests feed: phase (prefilling/decoding), tokens, tokens/s, TTFT — every
live request, SSE-abort detection included.

**The front door** — `/v1/*` on 8799 is the only way in (engine lockdown:
loopback docker bind + a generated API key only `qfn` holds). Two dialects
speak through it: **OpenAI** (`/v1/chat/completions`, `/v1/completions`) and
**Anthropic** (`/v1/messages`, translated in-proxy — Messages-format requests,
tools, tool_results, images, and a proper Anthropic SSE event sequence:
thinking/text/tool_use content blocks with `input_json_delta` streaming), so
Claude Code-class agents can point straight at the Spark. The proxy injects
SSE keepalives at event boundaries — no more proxy timeouts mid-decode —
records usage per request, and cancels upstream GPU work the moment your
client hangs up. `qfn chat` goes through it too, so the dashboard sees REPL
traffic. Machine auth for agents: `qfn lockdown front-key` →
`Authorization: Bearer …`. `serve.sampling_defaults = true` fills omitted
sampling params from the checkpoint's `generation_config.json` (client-set
values are never overridden). Named per-client API keys are schema-ready
(`serve.api_keys`) for when multi-tenant matters.

**`qfn launch`** — one command wires a coding agent to the front door:
`qfn launch claude|codex|opencode|dsh` (or `generic -- <cmd>`) discovers the
served model, injects the right env (`ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`,
context-window hints) with the front key, and **clears every cloud provider
key from the child environment** — ANTHROPIC/OPENAI/GEMINI/GROQ/XAI/… — so
the agent physically cannot fall back to a paid endpoint mid-task. `--dry-run`
shows the full plan.

**Metrics that aren't lies** — first scrape sets a baseline, rates are
windowed, histogram quantiles differ cumulative windows (never lifetime
averages), counter resets rebaseline instead of exploding. GPU *memory* comes
from host `/proc` because NVML on GB10 is decorative; `nvidia-smi` is used
only for util/power/clocks/temp.

**Ops** — `qfn doctor` preflights docker/runtime/checkpoint/disk/memory
pressure; `qfn bench` is the upstream smoke battery (cold-prefill tok/s,
prefix-cache HIT, byte-exact determinism at T=0, decode tok/s incl. TTFT,
optional needle) with JSON output and a drift history; `qfn stats -w` gives
the same numbers headless; `qfn service` owns its systemd units (inventory
tracked, `uninstall --list` before anything is removed).

## Layout

```
cmd/qfn/          main — signal-scoped ctx, single static binary
internal/cli/     cobra commands + the defaults wizard (the heart of init)
internal/config/  config.toml (+ commented template), profiles overlay, validation
internal/engine/  serve.sh byte-ported to Go: argv goldens tested; boot-log phase parser
internal/collector/ host/proc/cgroup/nvidia-smi/vLLM /metrics sampling
internal/proxy/   SSE keepalive relay, abort propagation, usage registry,
                  Anthropic ⇄ OpenAI translation
internal/server/  console HTTP: login/lockout, SSE multiplexer, engine ops
internal/auth/    age-encrypted credentials, sessions, per-IP lockout
internal/doctor/  preflight battery + the memory guard (shared by CLI + web)
internal/bench/   smoke-test battery ports; internal/chat/ streaming REPL
internal/service/ systemd units with ownership inventory
engine/           VENDORED build context from qwen3.8-Flash-DGX (see ATTRIBUTION.md)
web/              embedded console UI (vanilla ES, no build step)
```

## Security posture

The console password (≥12 chars, set in the wizard) is Argon2id-hashed and
stored alongside the engine's API key in an age-encrypted file. Sessions are
HttpOnly/SameSite=Strict and die with `qfn serve`. Failed logins lock an IP
out for 1→64 min (persisted). Bind beyond loopback can't disable auth —
validation refuses. Lockdown can't be enabled without a key. Everything else
is a plain-HTTP LAN tool by design: the recommended remote view is
`ssh -L 8799:localhost:8799 <spark>`.

## Build

```
make build     # host binary
make arm64     # CGO_ENABLED=0 static aarch64 — this is what runs on the Spark
make test      # go test ./...
make deploy SPARK=bishop@10.0.1.42   # scp + swap the binary
```

Upstream sync: `engine/` files stay byte-verbatim (re-sync recipe in
`engine/ATTRIBUTION.md`); every deliberate launch deviation (loopback bind,
`--api-key`, no cgroup memory cap) lives in Go, covered by argv golden tests.

Apache-2.0 parts in `engine/` belong to their authors (vLLM image, fp8 tool by
@Saren-Arterius); everything else is MIT.
