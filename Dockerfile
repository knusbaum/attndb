# syntax=docker/dockerfile:1
#
# attndb MCP document server.
#
# Multi-arch (linux/amd64 + linux/arm64): the native dependencies are fetched
# per TARGETARCH, so the same Dockerfile builds on an x86 server and an Apple
# Silicon laptop. Build both with buildx:
#
#   docker buildx build --platform linux/amd64,linux/arm64 -t attndb .
#
# The ~1.3 GB of exported ONNX weights are NOT baked in — they are mounted at
# run time (see docker-compose.yml). Export them on the host first; there is no
# `attndb pull` yet.

ARG GO_VERSION=1.26
ARG DEBIAN_SUITE=trixie
ARG ORT_VERSION=1.27.0
ARG TOKENIZERS_VERSION=1.27.0

# ---------- build ----------
FROM golang:${GO_VERSION}-${DEBIAN_SUITE} AS build
ARG ORT_VERSION
ARG TOKENIZERS_VERSION
ARG TARGETARCH
WORKDIR /src

# Native deps. libtokenizers is a static archive linked at build time;
# ONNX Runtime is a shared library dlopen'd at run time. Both name their release
# assets by architecture, and the two projects spell it differently
# (amd64/x64, arm64/aarch64) — hence the mapping.
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) ort_arch=x64;     tok_arch=amd64 ;; \
      arm64) ort_arch=aarch64; tok_arch=arm64 ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
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
RUN CGO_ENABLED=1 CGO_LDFLAGS="-L/out/libs" \
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

# The encoder dlopen's this exact path (overrides the ./libs default, which
# assumes a source checkout).
ENV ATTNDB_ORT_LIB=/usr/local/lib/libonnxruntime.so.${ORT_VERSION}

# Mount points: the vault is read-write (the write_doc/edit_doc tools), the
# models read-only, and state holds the generated calibration file.
VOLUME ["/vault", "/models", "/state"]
EXPOSE 8765

ENTRYPOINT ["attndb"]
# Bind 0.0.0.0, not the localhost default — otherwise the port publish can't
# reach it from outside the container.
CMD ["serve", "-store", "qdrant", "-qdrant", "qdrant:6334", \
     "-encoder", "onnx", "-provider", "cpu", \
     "-docs", "/vault", "-model", "/models", \
     "-calib", "/state/calibration.json", \
     "-addr", "0.0.0.0:8765"]
