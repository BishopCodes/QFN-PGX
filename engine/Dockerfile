# Qwen3.8-Flash-Next on a single DGX Spark / GB10, via vLLM.
#
# Starts from the official Qwen3.8-Flash-Next vLLM image and layers a handful of
# patches on it. Every one of them is a no-op unless its runtime flag is set, so
# the image still behaves like upstream with the flags off:
#
#   1. PLE table served from disk via mmap            (VLLM_PLE_MMAP=1)      — the one that makes it fit
#   2. GB10 FLA fixes                                  (always on, harmless elsewhere)
#   3. Mamba state-copy race fix + bounds guard        (always on)
#   4. Prefix-caching block_size fix                   (always on; needed for --enable-prefix-caching)
#   5. Exact, deterministic QSA top-k                  (VLLM_QSA_EXACT_TOPK=1)
#   6. NVFP4 experts + fp8 side-layers "hybrid" mode   (VLLM_FP8_HYBRID=1)
#   7. NVIDIA official ckpt: FP8-block MTP + FP8 PLE   (always on; inert on RadixArk)
#   8. Token embeddings pinned in host RAM             (QWEN38_HOST_EMBEDDINGS=1)
#   9. QSA FP8 KV cache via B12x kernels               (build-time: --build-arg B12X_COMMIT)
#
#   docker build -t qwen38-flash-dgx .
#   docker build -t qwen38-flash-dgx --build-arg B12X_COMMIT=c76a40ee684cb3ef7d2c223d56a9b9cff25a3a1e .   # = `qfn build --b12x`
#
# The base image is multi-arch (arm64 for the Spark's Grace CPU). Pinned by digest
# for reproducibility; bump the tag below if the upstream recipe moves.
FROM vllm/vllm-openai:qwen38-flash-next@sha256:fc120ece0a388cc0aa1caad4a9f1cd92113484ab7ec2fd0efadd62585be05bf8
# NOTE (2026-09-08, replaces the 08-31 note): the "build predates mm wiring" theory
# is contradicted by receipts — independent recipes on THIS EXACT base digest serve
# image input on SM120 and SM121 (1/4/16-image probes, github.com/tpurtell/
# sm12x-exl3-qwen3.8-flash-next; tag fc120ece confirmed unchanged 2026-09-07).
# #53896 (merged 08-31) evidently carried the mm wiring this image was built from
# the release/qwen38next branch already. Verify locally: `qfn doctor --vision-matrix`.
# If a probe ever fails on "not supported", suspect the SNAPSHOT's mm files
# (processor_config.json is missing from the RadixArk port) before the engine build.

# Package layout inside the official image (vLLM 0.1.dev20073, torch 2.13 cu130,
# numpy 2.2.6 — the patch needs numpy, already present).
ARG SP=/usr/local/lib/python3.12/dist-packages
ARG PLE=${SP}/vllm/models/qwen3_8_flash_next/nvidia/ple_layer.py

# --- 1. PLE n-gram table from disk ------------------------------------------------
COPY src/vllm_ple_mmap.py ${SP}/vllm_ple_mmap.py
RUN cp ${PLE} ${PLE}.orig \
 && printf '\n\n# --- qwen38-flash-dgx: serve the PLE n-gram table from disk (VLLM_PLE_MMAP=1) ---\nfrom vllm_ple_mmap import apply as _ple_mmap_apply\n_ple_mmap_apply(Qwen3_8FlashNextNGramEmbedding)\n' >> ${PLE} \
 && python3 -c "import ast; ast.parse(open('${PLE}').read()); print('ple_layer.py patched OK')"

# --- 2. GB10 FLA fixes, contributed by @Saren-Arterius ----------------------------
#     (https://github.com/Saren-Arterius/qwen3.8-Flash-DGX-AutoRound)
# 1) sm_121 reports 99 KiB of shared memory per block; the flash-linear-attention
#    gate asks for 100 KiB, so all 36 GDN layers silently fell back to small tiles.
# 2) fla#953: tl.dot race on Blackwell with num_warps=4 in chunk_delta_h -> pin 2.
ARG FLA_UTILS=${SP}/vllm/third_party/flash_linear_attention/ops/utils.py
ARG FLA_CDH=${SP}/vllm/third_party/flash_linear_attention/ops/chunk_delta_h.py
RUN sed -i 's|DEFAULT = 102400|DEFAULT = 101376  # spark-fla-shmem: GB10 99KiB, big GDN tiles fit|' ${FLA_UTILS} \
 && grep -q "spark-fla-shmem" ${FLA_UTILS} && echo "fla shmem gate patched" \
 && sed -i 's|for num_warps in \[2, 4\]|for num_warps in [2]  # spark-fla-warps: fla#953 Blackwell tl.dot race|' ${FLA_CDH} \
 && grep -q "spark-fla-warps" ${FLA_CDH} && echo "fla num_warps pinned"

# --- 3. Mamba state copy: vllm#50729 + bounds guard --------------------------------
# src/mamba_utils_guarded.py = vLLM's v1/worker/mamba_utils.py with
#   - vllm#50729 "[Bugfix][Mamba] Fix overlapping state copy race" (@AndreasKaratzas), and
#   - a bounds guard by @Saren-Arterius: a state copy with an out-of-range block id is
#     skipped and counted ("mamba state-copy guard: N out-of-range ...") instead of
#     taking down the CUDA context with an illegal memory access.
ARG MU=${SP}/vllm/v1/worker/mamba_utils.py
RUN cp ${MU} ${MU}.orig
COPY src/mamba_utils_guarded.py ${MU}
RUN python3 -c "import ast; ast.parse(open('${MU}').read()); print('mamba_utils.py guarded OK')"

# --- 4. Prefix caching: block_size fix ----------------------------------------------
# vLLM's EngineCore overwrites cache_config.block_size with the smallest KV-group block
# size (here the QSA raw-key ring: 8/16 tokens) while the Mamba block is 1600. Two
# consumers used the former as the latter, so a prefix hit restored an all-zero Mamba
# state. See docs/HOW-IT-WORKS.md. With this, --enable-prefix-caching is correct.
COPY src/patch_mamba_block_size.py /tmp/patch_mamba_block_size.py
RUN python3 /tmp/patch_mamba_block_size.py ${SP} && rm /tmp/patch_mamba_block_size.py

# --- 5. Exact QSA top-k (VLLM_QSA_EXACT_TOPK=1) ---------------------------------------
# The stock persistent_topk kernel is non-deterministic on GB10 and can drop real
# top-k candidates (vllm#51782; reported here by @k3dani, issue #3). The exact path
# uses torch.topk over the visible columns. Opt-in; scripts/serve.sh enables it.
COPY src/patch_qsa_exact_topk.py /tmp/patch_qsa_exact_topk.py
RUN python3 /tmp/patch_qsa_exact_topk.py ${SP}/vllm/models/qwen3_8_flash_next/nvidia/ops/qsa.py && rm /tmp/patch_qsa_exact_topk.py

# --- 6. Hybrid mode: NVFP4 experts + blockwise-fp8 side layers (VLLM_FP8_HYBRID=1) ----
# Lets the GDN in/out projections, QSA q/k/v/o and shared experts be stored as
# blockwise fp8 (DeepSeek layout, produced by tools/fp8_convert.py) while the MoE
# experts stay NVFP4. Port of @Saren-Arterius's int4+fp8 dispatch to ModelOpt-NVFP4.
# No-op unless VLLM_FP8_HYBRID=1 (and harmless on a plain NVFP4 checkpoint).
ARG MO=${SP}/vllm/model_executor/layers/quantization/modelopt.py
ARG QSA=${SP}/vllm/models/qwen3_8_flash_next/nvidia/qsa.py
COPY src/vllm_fp8_hybrid_modelopt.py ${SP}/vllm_fp8_hybrid_modelopt.py
RUN cp ${MO} ${MO}.orig \
 && printf '\n\n# --- qwen38-flash-dgx: NVFP4 + blockwise-fp8 side layers (VLLM_FP8_HYBRID=1) ---\nfrom vllm_fp8_hybrid_modelopt import apply as _fp8_hybrid_apply\n_fp8_hybrid_apply()\n' >> ${MO} \
 && python3 -c "import ast; ast.parse(open('${MO}').read()); print('modelopt.py hooked OK')" \
 && cp ${QSA} ${QSA}.orig \
 && sed -i 's/quant_config=model\.without_modelopt_fp4(quant_config)/quant_config=_fp8_hybrid_excluded(quant_config)/' ${QSA} \
 && sed -i 's/^from \. import model$/from . import model\nfrom vllm_fp8_hybrid_modelopt import excluded_quant_config as _fp8_hybrid_excluded/' ${QSA} \
 && grep -q "_fp8_hybrid_excluded(quant_config)" ${QSA} && grep -q "^from vllm_fp8_hybrid_modelopt import" ${QSA} \
 && python3 -c "import ast; ast.parse(open('${QSA}').read()); print('qsa.py hooked OK')"

# --- 7. NVIDIA official NVFP4 checkpoint loading (always on; inert on RadixArk) ----
# nvidia/Qwen3.8-Flash-Next-NVFP4 packs MTP routed experts as 128x128 block FP8
# ("FP8_PB_WO") and the PLE table as global-scale FP8 under a MIXED_PRECISION
# ModelOpt config. The stock loader mis-routes both on this base — upstream fixed
# them in PR #55513 (still open) and #54882/d4d703c (not in this image). These two
# assert-anchored ports close the gap locally. Inert for the RadixArk checkpoint:
# its plain ModelOpt config never enters the mixed-precision branches.
# Vendored from github.com/tpurtell/sm12x-exl3-qwen3.8-flash-next (Apache-2.0);
# see engine/ATTRIBUTION.md.
COPY src/port-nvidia-mtp-fp8.py /tmp/port-nvidia-mtp-fp8.py
COPY src/port-nvidia-ple-fp8.py /tmp/port-nvidia-ple-fp8.py
RUN python3 /tmp/port-nvidia-mtp-fp8.py ${SP}/vllm \
 && python3 /tmp/port-nvidia-ple-fp8.py ${SP}/vllm \
 && python3 -c "import ast; ast.parse(open('${PLE}').read()); print('ple_layer.py nvidia-ple port OK')"

# --- 8. Token embeddings pinned in host RAM (QWEN38_HOST_EMBEDDINGS=1) -------------
# Moves the 1.184 GiB (248320x2560 bf16) token embedding table — target *and* MTP
# draft — out of the GPU slice into pinned host RAM, rows gathered per step over
# PCIe (~1.2 GiB x2 back to the pool for KV). Off by default (ENV below); qfn sets
# the env per engine.host_embeddings. Vendored from the tpurtell recipe (Apache-2.0).
COPY src/qwen_host_embedding.py ${SP}/vllm/model_executor/layers/qwen_host_embedding.py
COPY src/port-host-embedding.py /tmp/port-host-embedding.py
RUN python3 /tmp/port-host-embedding.py ${SP}/vllm \
 && python3 -c "import ast; ast.parse(open('${SP}/vllm/model_executor/layers/vocab_parallel_embedding.py').read()); print('vocab_parallel_embedding.py hooked OK')"
ENV QWEN38_HOST_EMBEDDINGS=0

# --- 9. QSA FP8 KV cache via B12x sparse-GQA kernels (build-time opt-in) ------------
# engine.kv_dtype="fp8" (~2x KV memory) is refused by the stock QSA layers ("requires
# a BF16 main KV cache"). The B12x sparse-GQA kernel (sparkinfer-glmrt, CUTE-DSL,
# sm_121a) reads FP8 KV pages directly; the bridge routes QSA attention to it.
# This changes the QSA attention path for BF16 too, so it is BUILD-gated: the default
# image (B12X_COMMIT empty) is untouched. `qfn build --b12x` = --build-arg
# B12X_COMMIT=c76a40ee684cb3ef7d2c223d56a9b9cff25a3a1e; then run with kv_dtype="fp8"
# only after `qfn bench` clears quality (the upstream recipe ships FP8 KV but ran no
# BF16-vs-FP8 KV full-model quality control).
ARG B12X_COMMIT=
ENV CUTE_DSL_ARCH=sm_121a
COPY src/b12x_qsa_attention.py ${SP}/vllm/models/qwen3_8_flash_next/nvidia/b12x_qsa_attention.py
COPY src/port-qsa-fp8.py /tmp/port-qsa-fp8.py
RUN set -e; \
    if [ -n "${B12X_COMMIT}" ]; then \
      python3 -c "import os, tarfile, urllib.request; from pathlib import Path; c=os.environ['B12X_COMMIT']; urllib.request.urlretrieve('https://github.com/tpurtell/sparkinfer-glmrt/archive/'+c+'.tar.gz', '/tmp/b12x.tar.gz'); a=tarfile.open('/tmp/b12x.tar.gz'); a.extractall('/opt', filter='data'); a.close(); Path('/opt/sparkinfer-glmrt-'+c).rename('/opt/b12x'); Path('/tmp/b12x.tar.gz').unlink()" \
   && python3 -m pip install --no-deps -e /opt/b12x \
   && python3 -c 'import b12x; print("b12x installed")' \
   && python3 /tmp/port-qsa-fp8.py ${SP}/vllm; \
    else rm -f ${SP}/vllm/models/qwen3_8_flash_next/nvidia/b12x_qsa_attention.py; echo "B12x build skipped (default image unchanged; kv_dtype=fp8 needs --build-arg B12X_COMMIT)"; fi
