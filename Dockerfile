# syntax=docker/dockerfile:1
#
# attndb MCP document server.
#
# Multi-arch (linux/amd64 + linux/arm64) IMAGES, built from an amd64 HOST: the
# native dependencies are fetched per TARGETARCH, so one build produces both
# architectures. The models stage pins to BUILDPLATFORM (see below) because its
# pip dependency has no linux/aarch64 wheel — so the build host itself must be
# amd64 today; an arm64 host (e.g. Docker Desktop on Apple Silicon) cannot build
# this image for any target. A single foreign-arch build works on the default
# builder (docker buildx build --platform linux/arm64 -t attndb:arm64 --load .);
# both architectures in one command needs the container driver, since the
# default `docker` driver cannot export a multi-platform manifest list — see
# docs/building.md for the full sequence.
#
# The encoder weights are pulled from HuggingFace and converted to ONNX during
# the build (the `models` stage), so nothing has to be distributed alongside the
# image and there is no host export step. That stage is the expensive part —
# minutes, and a few GB of RAM for the torch export — but it is a separate layer,
# so it is cached across rebuilds and only re-runs when its inputs change.
#
# To skip it and use weights you already exported, mount them over
# /opt/attndb/models at run time (see docker-compose.yml).

ARG GO_VERSION=1.26
ARG DEBIAN_SUITE=trixie
ARG ORT_VERSION=1.27.0
ARG TOKENIZERS_VERSION=1.27.0
ARG PYTHON_VERSION=3.11

# ---------- models ----------
# Pull the encoders from HuggingFace and convert them to ONNX. Mirrors the
# README's host export step, minus pipenv: pip is enough inside a throwaway
# image, and torch comes from PyTorch's CPU-only index because PyPI's Linux
# wheel is the CUDA build (~2.5 GB of nvidia-* deps this offline, CPU-only
# export never uses).
#
# Pinned to BUILDPLATFORM — the machine running the build — not TARGETPLATFORM.
# What this stage emits is architecture-independent data (ONNX graphs, weights,
# tokenizer JSON; the .data files are byte-identical across hosts), so building
# it for the target buys nothing and costs two ways:
#   - correctness: pylate's transitive dep fast-plaid publishes no linux/aarch64
#     wheel and no sdist, so `pip install colbert-export` cannot resolve there at
#     all — on a native arm64 machine just as surely as under emulation;
#   - speed: cross-building it would run the torch export under QEMU, the
#     dense-FP workload emulation handles worst.
FROM --platform=$BUILDPLATFORM python:${PYTHON_VERSION}-slim AS models
ARG COLBERT_MODEL=lightonai/GTE-ModernColBERT-v1
ARG TORCH_CPU_INDEX=https://download.pytorch.org/whl/cpu
WORKDIR /

# torch first, from the CPU index, so the transitive resolution below finds the
# requirement already satisfied and never reaches for the CUDA build.
RUN pip install --no-cache-dir --index-url ${TORCH_CPU_INDEX} torch \
 && pip install --no-cache-dir colbert-export onnxscript

# Per-token ColBERT -> /models. quantize=False: the int8 export is another
# ~150 MB and the Go side only ever opens model.onnx.
RUN python -c "from colbert_export import export_model; \
    export_model('${COLBERT_MODEL}', output_dir='/models', quantize=False)"

# Single-vector embedder -> /models/single (the script writes a relative path).
COPY scripts/export_single.py /tmp/export_single.py
RUN python /tmp/export_single.py

# Fail the build loudly here rather than at container start if an export silently
# produced nothing. The .onnx graph is only a couple of MB — the weights live in
# the external .data file, so check that too (a truncated one would otherwise
# pass and only blow up when ORT loads the session).
RUN set -eux; \
    for f in /models/model.onnx /models/tokenizer.json \
             /models/single/model.onnx /models/single/tokenizer.json; do \
      test -s "$f" || { echo "missing/empty: $f" >&2; exit 1; }; \
    done; \
    for f in /models/model.onnx.data /models/single/model.onnx.data; do \
      sz=$(stat -c %s "$f" 2>/dev/null || echo 0); \
      test "$sz" -gt 100000000 || { echo "weights too small: $f ($sz bytes)" >&2; exit 1; }; \
    done

# ---------- build ----------
# Also pinned to BUILDPLATFORM: the Go toolchain runs natively and cross-compiles
# to TARGETARCH, rather than running an arm64 toolchain under emulation.
#
# This is not just a speed choice. Building Go under qemu-user is unreliable: the
# toolchain drives parallel `compile` subprocesses with raw clone/futex/signals,
# and emulating that races. Observed here on an amd64 host targeting arm64 — every
# `compile` child exited into a zombie while the parent spun at 100% CPU waiting
# to reap them, livelocked past 26 minutes, after an identical earlier build had
# happened to succeed in 149s. Cross-compiling removes the emulator from the build
# entirely: nothing arm64 executes, it only gets written.
#
# Consequence worth knowing: binfmt/qemu is then only needed to *run* a foreign
# image locally, never to build one.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-${DEBIAN_SUITE} AS build
ARG ORT_VERSION
ARG TOKENIZERS_VERSION
ARG TARGETARCH
ARG BUILDARCH
WORKDIR /src

# Native deps + a cross C toolchain when the target differs from the builder.
# libtokenizers is a static archive linked at build time; ONNX Runtime is a
# shared library dlopen'd at run time (so it is never linked, only shipped).
# Both projects name their release assets by architecture and spell it
# differently (amd64/x64, arm64/aarch64) — hence the mapping.
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) ort_arch=x64;     tok_arch=amd64; \
             cross_pkgs="gcc-x86-64-linux-gnu g++-x86-64-linux-gnu libc6-dev-amd64-cross" ;; \
      arm64) ort_arch=aarch64; tok_arch=arm64; \
             cross_pkgs="gcc-aarch64-linux-gnu g++-aarch64-linux-gnu libc6-dev-arm64-cross" ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    if [ "$TARGETARCH" != "$BUILDARCH" ]; then \
      apt-get update; \
      : "All three are required. gcc-*-cross alone has no target headers, so cgo \
         falls back to the host /usr/include and dies on arch-specific ones like \
         bits/wordsize.h; libc6-dev-*-cross supplies those. g++-*-cross supplies \
         the target libstdc++ that the ONNX Runtime bindings link (-lstdc++)."; \
      apt-get install -y --no-install-recommends $cross_pkgs; \
      rm -rf /var/lib/apt/lists/*; \
    fi; \
    mkdir -p /out/libs; \
    curl -sSL "https://github.com/daulet/tokenizers/releases/download/v${TOKENIZERS_VERSION}/libtokenizers.linux-${tok_arch}.tar.gz" \
      | tar -xz -C /out/libs; \
    curl -sSL -o /tmp/ort.tgz \
      "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/onnxruntime-linux-${ort_arch}-${ORT_VERSION}.tgz"; \
    tar -xzf /tmp/ort.tgz -C /tmp; \
    cp -a "/tmp/onnxruntime-linux-${ort_arch}-${ORT_VERSION}/lib/libonnxruntime.so"* /out/libs/; \
    rm -rf /tmp/ort.tgz "/tmp/onnxruntime-linux-${ort_arch}-${ORT_VERSION}"

# Dependencies first so edits to the source don't re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# The onnx build tag pulls in the real encoders (CGO: ONNX Runtime + tokenizers).
# CC is the cross compiler when targeting another architecture, plain gcc when
# building for this one.
RUN set -eux; \
    cc=gcc; cxx=g++; \
    if [ "$TARGETARCH" != "$BUILDARCH" ]; then \
      case "$TARGETARCH" in \
        amd64) cc=x86_64-linux-gnu-gcc;  cxx=x86_64-linux-gnu-g++ ;; \
        arm64) cc=aarch64-linux-gnu-gcc; cxx=aarch64-linux-gnu-g++ ;; \
      esac; \
    fi; \
    CGO_ENABLED=1 GOOS=linux GOARCH="$TARGETARCH" CC="$cc" CXX="$cxx" \
      CGO_LDFLAGS="-L/out/libs" \
      go build -tags onnx -trimpath -o /out/attndb ./cmd/attndb

# ---------- runtime ----------
FROM debian:${DEBIAN_SUITE}-slim
ARG ORT_VERSION

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Only the versioned file: COPY dereferences symlinks, so a glob would ship three
# full copies of a ~24 MB library. The encoder dlopen's this absolute path, so no
# SONAME symlink or ldconfig entry is needed.
COPY --from=build /out/libs/libonnxruntime.so.${ORT_VERSION} /usr/local/lib/
COPY --from=build /out/attndb /usr/local/bin/attndb

# Weights converted in the models stage. Baked in, so the image is self-contained
# and nothing needs distributing; mount over this path to supply your own.
COPY --from=models /models /opt/attndb/models

# The encoder dlopen's this exact path (overrides the ./libs default, which
# assumes a source checkout).
ENV ATTNDB_ORT_LIB=/usr/local/lib/libonnxruntime.so.${ORT_VERSION}

# Mount points: the vault is read-write (the write_doc/edit_doc tools) and state
# holds the generated calibration file.
VOLUME ["/vault", "/state"]
EXPOSE 8765

ENTRYPOINT ["attndb"]
# Bind 0.0.0.0, not the localhost default — otherwise the port publish can't
# reach it from outside the container.
CMD ["serve", "-store", "qdrant", "-qdrant", "qdrant:6334", \
     "-encoder", "onnx", "-provider", "cpu", \
     "-docs", "/vault", "-model", "/opt/attndb/models", \
     "-calib", "/state/calibration.json", \
     "-addr", "0.0.0.0:8765"]
