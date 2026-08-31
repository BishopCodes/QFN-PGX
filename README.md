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

**Web console (`:8799`)** — Tailwind-styled, push-only (one SSE multiplexer;
zero polling), with vendored TanStack Charts for the perf graph. Engine panel
shows live boot progress (phase · percent · ETA, parsed from the log stream);
one server-side `docker logs -f` pump fans out to every tab with a bounded
replay, so reconnects neither re-flood the screen nor hammer docker — the log
pane pauses following when you scroll up instead of yanking. Requests render
as a real table with click-in per-request timing; every bar carries its
used-of-total numbers; a `?` on every panel explains what it means (and the
`? help` button gives the full tour). **Playground**: chat the model while
flipping temperature / top_p / max-tokens / thinking live, with presets and
per-run ttft+tok/s stats — the config-testing loop without leaving the
browser. **Restart console** button: the web process exits and systemd
relaunches it, so `sudo make install` + one click = upgraded console, engine
untouched. CLI twins: `qfn serve stop|restart|status` (systemd-aware,
pidfile fallback).

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
`Authorization: Bearer …` — or mint individually revocable, request-attributed
ones with `qfn keys add <name>` (opencode, external harnesses, scripts;
`qfn keys list|rm` for the rest of the lifecycle). `serve.sampling_defaults = true` fills omitted
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
out for 1→64 min (persisted). The console binds `0.0.0.0` by default — it's a
plain-HTTP LAN tool — so it is reachable from other boxes; open the port in
your firewall (`sudo ufw allow 8799`) and keep an eye on who can reach the
LAN. Bind beyond loopback can't disable auth — validation refuses. (Want it
loopback-only? Set `serve.bind = "127.0.0.1"`, or reach it tunneled via
`ssh -L 8799:localhost:8799 <spark>`.) The engine itself always binds
loopback under lockdown; `/v1/*` on the console is the only way in.

## Build

```
make build     # host binary → ./bin/qfn
make install   # …and put it on PATH (sudo needed for /usr/local/bin; or PREFIX=~/.local/bin)
make arm64     # CGO_ENABLED=0 static aarch64 — this is what runs on the Spark
make webcss    # rebuild web/style.css from web/src/input.css (dev-only, Tailwind standalone CLI)
make test      # go test ./...
make deploy SPARK=bishop@10.0.1.42   # scp + swap the binary
```

On the Spark itself, a plain flow is: `git clone … && cd QFN-PGX && make build && sudo make install`

Unattended first-run (Ansible, CI, muscle-memory): `echo 'pw' | qfn init
--defaults --password-stdin`, then tune with `qfn config set`. The vendored
chart bundle regenerates via the `chartjs` recipe comment in the Makefile
(npm on the dev box; output committed so the Spark needs no toolchain).
— `make build` alone only leaves the binary at `./bin/qfn` (that tripped the first `command not found`).

Upstream sync: `engine/` files stay byte-verbatim (re-sync recipe in
`engine/ATTRIBUTION.md`); every deliberate launch deviation (loopback bind,
`--api-key`, no cgroup memory cap) lives in Go, covered by argv golden tests.

Apache-2.0 parts in `engine/` belong to their authors (vLLM image, fp8 tool by
@Saren-Arterius); everything else is MIT.
