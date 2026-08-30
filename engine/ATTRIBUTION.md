# Vendored engine assets

Everything in this `engine/` directory is vendored verbatim (no modifications) from:

- **Repository:** <https://github.com/BishopCodes/qwen3.8-Flash-DGX>
  (originally <https://github.com/blazux/qwen3.8-Flash-DGX>)
- **License:** Apache License 2.0 — see [`LICENSE`](./LICENSE), Copyright 2026 blazux
- **What:** the vLLM image build (`Dockerfile`), the runtime patches (`src/`), and the
  checkpoint tooling (`tools/`) for serving Qwen3.8-Flash-Next on a single DGX Spark
  (GB10) with the PLE n-gram table served from NVMe via `mmap`.

The QFN-PGX CLI reproduces `scripts/serve.sh`'s docker invocation (kept in sync by
tests in `internal/engine`) and adds a loopback bind + API-key lockdown on top; the
launch deltas are applied by the Go code, **never** by editing these files.

See the upstream [`docs/HOW-IT-WORKS.md`](https://github.com/BishopCodes/qwen3.8-Flash-DGX/blob/main/docs/HOW-IT-WORKS.md)
for why each patch exists. Upstream measured numbers are referenced in this README but
not re-claimed; re-measure with `qfn bench`.

To re-sync vendored files after an upstream change:

```sh
rsync -a --delete <qwen3.8-Flash-DGX-clone>/{Dockerfile,src,tools,LICENSE} engine/
# then verify the launch-spec tests still pass: go test ./internal/engine/
```
