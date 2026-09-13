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
# NOTE: re-sync clobbers our own sections 7-9 of the Dockerfile — re-apply them.
```

## Second source: tpurtell/sm12x-exl3-qwen3.8-flash-next

`src/port-nvidia-mtp-fp8.py`, `src/port-nvidia-ple-fp8.py`, `src/port-host-embedding.py`,
`src/qwen_host_embedding.py`, `src/port-qsa-fp8.py`, and `src/b12x_qsa_attention.py`
are vendored verbatim from <https://github.com/tpurtell/sm12x-exl3-qwen3.8-flash-next>
(Apache License 2.0), fetched 2026-09-08 from `main` (repo created 2026-09-07). They
target the same base-image digest this Dockerfile pins, so their assert-anchors apply
byte-exactly. The B12x kernel library itself is **not** vendored — `docker build
--build-arg B12X_COMMIT=c76a40ee684cb3ef7d2c223d56a9b9cff25a3a1e` pulls the pinned
`tpurtell/sparkinfer-glmrt` tarball at build time. Where the ports overlap a concern
of ours (the NVIDIA FP8 PLE branch vs `vllm_ple_mmap.py`'s disk gather), our mmap wins
when VLLM_PLE_MMAP=1 (it never materializes the embedding) and theirs serves the
stock path; both are exercised by profiles.

