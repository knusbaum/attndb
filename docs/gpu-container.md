# MIGraphX GPU container candidate

This is a **build recipe and acceptance contract, not a validated deployment**.
The CPU `Dockerfile` and default Compose service are unchanged. No image was
pulled/built, container started, or production service replaced during this work:
Docker socket access was unavailable and bypassing it was out of scope.

The intended machine, angel, was reported as Ryzen AI MAX+ 395 / Radeon 8060S
(`gfx1151`), with `/dev/kfd` and `/dev/dri/renderD128`. AMD lists this GPU for
the versions below. Its Void Linux 6.18 host kernel is outside AMD's supported
OS/kernel matrix. Ubuntu userspace does **not** replace or validate that kernel.

## Candidate versions and evidence

Checked against primary documentation and live package indexes on 2026-11-24:

| Component | Pin / platform |
| --- | --- |
| Build and runtime libc userspace | Ubuntu 24.04, linux/amd64, Python 3.12 |
| Go toolchain | 1.26.2, copied from golang:1.26.2-bookworm; cgo compilation runs on Ubuntu |
| libtokenizers | 1.27.0 linux-amd64 static release |
| ROCm core/libraries/devel + gfx1151 device pack | 10.2.0 |
| Both `migraphx` and `migraphx-libs` | 2.19.0+rocm10.2.0 |
| ONNX Runtime | 1.29.0, cp312 manylinux_2_28_x86_64 |
| Standalone `onnxruntime-ep-migraphx` | 1.0.0+rocm10.2.0, cp312 manylinux_2_28_x86_64 |

The actual EP wheel metadata (read with bounded HTTP ranges, not a full wheel
download) requires `onnxruntime~=1.29.0` and exactly
`migraphx-libs==2.19.0+rocm10.2.0`. Its exported registration name is
`MIGraphXExecutionProvider`, and the packaged library is `libmigraphx-ep.so`.
The MIGraphX wheel metadata requires Python >=3.12 and the same exact libs
release. ROCm's Linux x86-64 core, libraries, devel and gfx1151 device wheels are
listed at 10.2.0. This verifies published package availability and declared
compatibility, **not successful resolution, ELF loading, or GPU execution**.

Do not substitute the older integrated `onnxruntime-migraphx` 1.23.x wheel:
attndb's Go binding uses ORT C API 26 and standalone plugin registration/device
enumeration. Do not mix a host ROCm installation into the container.

Primary sources:

- [AMD ONNX Runtime installation](https://rocm.docs.amd.com/projects/ai-ecosystem/en/latest/inference/onnxruntime.html)
- [AMD MIGraphX installation](https://rocm.docs.amd.com/projects/ai-ecosystem/en/latest/inference/migraphx.html)
- [ROCm 10.2 installation, including gfx1151 pip extra](https://rocm.docs.amd.com/en/docs-10.2.0/install/rocm.html)
- [AMD compatibility matrix](https://rocm.docs.amd.com/en/docs-10.2.0/compatibility/compatibility-matrix.html)
- [EP wheel index and linked metadata-bearing wheels](https://stable.repo.amd.com/rocm/onnxruntime/whl-next/onnxruntime-ep-migraphx/)
- [MIGraphX wheel index](https://stable.repo.amd.com/rocm/migraphx/whl-next/migraphx/)
- [MIGraphX libs index](https://stable.repo.amd.com/rocm/migraphx/whl-next/migraphx-libs/)
- [ROCm core index](https://stable.repo.amd.com/rocm/whl-next/rocm-sdk-core/),
  [libraries](https://stable.repo.amd.com/rocm/whl-next/rocm-sdk-libraries/),
  [devel](https://stable.repo.amd.com/rocm/whl-next/rocm-sdk-devel/),
  [gfx1151](https://stable.repo.amd.com/rocm/whl-next/rocm-sdk-device-gfx1151/)
- [Microsoft ORT 1.29.0 release files on PyPI](https://pypi.org/pypi/onnxruntime/1.29.0/json)
- [AMD native wheel layout and initialization](https://github.com/ROCm/TheRock/blob/main/docs/packaging/python_packaging.md)
- [AMD rocm-sdk usage](https://github.com/ROCm/TheRock/blob/main/RELEASES.md#using-multi-arch-rocm-python-packages)

## Build recipe (not executed here)

Use a native Linux amd64 builder. GPU access is not needed during the build.
Reuse the CPU Dockerfile's existing model-export stage, avoiding a second export
implementation. Run from the repository root:

```sh
docker buildx build --platform linux/amd64 --target models \
  -f Dockerfile -t attndb-models:local --load .

docker buildx build --platform linux/amd64 \
  -f Dockerfile.migraphx -t attndb:migraphx-rocm10.2.0 --load \
  --build-arg MODELS_IMAGE=attndb-models:local \
  --build-arg GO_VERSION=1.26.2 \
  --build-arg UBUNTU_VERSION=24.04 \
  --build-arg TOKENIZERS_VERSION=1.27.0 \
  --build-arg ROCM_VERSION=10.2.0 \
  --build-arg MIGRAPHX_VERSION=2.19.0 \
  --build-arg ORT_VERSION=1.29.0 \
  --build-arg MIGRAPHX_EP_VERSION=1.0.0 \
  --build-arg GPU_TARGET=gfx1151 .
```

The local models image must be visible to the builder: the local Docker driver
can use a loaded image; an isolated/remote builder needs an explicitly supplied
registry image or equivalent named build context. Do not publish it implicitly.
The model stage is large and expensive. An existing model-stage image can be
reused via `MODELS_IMAGE`; it must contain `/models`, not only
`/opt/attndb/models`.

Artifacts copied read-only into the image:

- `/opt/attndb/models/{model.onnx,model.onnx.data,tokenizer.json}`:
  `lightonai/GTE-ModernColBERT-v1`.
- `/opt/attndb/models/single/{model.onnx,model.onnx.data,tokenizer.json}`:
  `Alibaba-NLP/gte-modernbert-base`.

The inherited exporters use current model revisions and some unpinned Python
export dependencies. Base images and apt packages also are not digest/hash
locked. These component version pins are therefore **not a bit-reproducible
supply-chain lock**. Record the built image digest, model checksums and
`/opt/attndb/python-packages.txt` before promoting a tested image. Change the
tuple together; other version combinations/architectures are not validated.

## Native runtime layout

The venv and ROCm wheel tree remain intact. `rocm-sdk init` materializes the
native devel layout at build time, and `/opt/rocm` links to its root. This includes
compiler/headers for runtime graph compilation; no kernel driver is installed.
Build and runtime both use Ubuntu 24.04 to avoid linking a Go/cgo executable
against Debian trixie's newer glibc.

Default environment:

```text
ATTNDB_ORT_LIB=/opt/attndb/venv/lib/python3.12/site-packages/onnxruntime/capi/libonnxruntime.so.1.29.0
ATTNDB_MIGRAPHX_EP_LIB=/opt/attndb/venv/lib/python3.12/site-packages/onnxruntime_ep_migraphx/libmigraphx-ep.so
ATTNDB_MIGRAPHX_CACHE_DIR=/state/migraphx/rocm10.2.0-migraphx2.19.0-ort1.29.0-gfx1151
ROCM_PATH=/opt/rocm
HOME=/state
XDG_CACHE_HOME=/state/cache
ATTNDB_MAX_SINGLE_LEN=512
```

`LD_LIBRARY_PATH` includes ORT's capi directory, `migraphx_libs`,
`/opt/rocm/lib` and `/opt/rocm/lib64`. The ORT SONAME link points to the same
versioned ORT file. The build runs `pip check` and `ctypes.CDLL` checks for ORT
and the plugin, failing on unresolved native dependencies; these do not test GPU
access. The Go process does not depend on importing Python to initialize ROCm.

The 512-token pooled cap is an explicit conservative GPU default, **different
from the normal 8192-token default**. It bounds warmup compilation and memory,
but truncates long-document single-vector representations. Increase
`ATTNDB_MAX_SINGLE_LEN` only after measuring the configured production shapes
and memory on the actual APU. The smoke test uses the same cap as the service.
The current provider disables CPU EP fallback and uses float32, not fp16.

Cache ownership is numeric UID/GID **10001:10001**. Model-hash subdirectories
include both graph and external-weight hashes. Keep the parent scoped to the
GPU/runtime tuple; give changed plugin builds a fresh root too. Do not share
writable caches across untrusted users or concurrent smoke/service tests.

## Compose-ready runtime contract

This example is a handoff snippet, not a change to production infrastructure.
Provide host paths and the two actual device GIDs. Resolve them with:

```sh
stat -c '%n gid=%g mode=%a' /dev/kfd /dev/dri/renderD128
```

Prepare bind mounts owned/writable by 10001:10001 (or use a deliberately chosen
runtime UID/GID consistently). Bind mounts override image directory ownership.
Use **local persistent storage** for `/state` (including `/state/oauth`) and
for Qdrant’s storage directory. **Do not place either on NFS or `/mnt/slice`**:
OAuth state relies on local filesystem locking and atomic durable updates, and
Qdrant requires its own local persistent database storage. The vault mounted at
`/vault` may use NFS, including `/mnt/slice`; that does not make it suitable for
OAuth or Qdrant state. Verify the backing filesystem, not just the mount path.
Keep OAuth state private, outside the vault; a precreated `/state/oauth` must
be mode 0700. The read-only bcrypt verifier secret must be readable by the runtime UID.
Generate the verifier with `attndb auth-hash -out <new-file>` using its hidden
terminal prompt; the mounted file contains only the salted hash, never the raw
password. Unset legacy `ATTNDB_AUTH_PASSWORD` and `ATTNDB_AUTH_PASSWORD_FILE`
settings. Do not put secret values in image build arguments or source control.

```yaml
services:
  attndb-gpu:
    image: attndb:migraphx-rocm10.2.0
    platform: linux/amd64
    build:
      context: .
      dockerfile: Dockerfile.migraphx
      args:
        MODELS_IMAGE: attndb-models:local
    user: "10001:10001"
    devices:
      - /dev/kfd:/dev/kfd
      - /dev/dri/renderD128:/dev/dri/renderD128
    group_add:
      - "${KFD_GID:?set host kfd numeric group}"
      - "${RENDER_GID:?set host renderD128 numeric group}"
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    read_only: true
    tmpfs:
      - /tmp:rw,nosuid,nodev,size=2g,mode=1777
    volumes:
      - ${ATTNDB_DOCS:?set vault path}:/vault # NFS is allowed for vault documents
      - ${ATTNDB_STATE:?set dedicated local persistent state path}:/state # never NFS or /mnt/slice
    secrets:
      - attndb_password_hash
    environment:
      ATTNDB_AUTH_ISSUER: https://attn.knusbaum.com
      ATTNDB_AUTH_RESOURCE: https://attn.knusbaum.com/mcp
      ATTNDB_AUTH_PASSWORD_HASH_FILE: /run/secrets/attndb_password_hash
      ATTNDB_AUTH_STATE_DIR: /state/oauth
      ATTNDB_MAX_SINGLE_LEN: "512"
    command:
      - serve
      - -store
      - qdrant
      - -qdrant
      - qdrant:6334
      - -encoder
      - onnx
      - -provider
      - migraphx
      - -model
      - /opt/attndb/models
      - -docs
      - /vault
      - -calib
      - /state/calibration.json
      - -addr
      - 0.0.0.0:8765
    # No host port published. Attach to the actual internal Qdrant/proxy network.
secrets:
  attndb_password_hash:
    file: ${ATTNDB_PASSWORD_HASH_FILE:?set bcrypt verifier file path}
```

No privileged mode, host networking, host IPC, SYS_PTRACE capability, whole
`/dev/dri` mount, or host ROCm libraries are required by this contract. Leave
Docker's seccomp policy enabled. The 2 GiB temporary workspace is a starting
limit, not a measured requirement; compilation may need more. Device/kernel
failures are reasons to investigate, not reasons to silently fall back to CPU
or broaden privileges.

The image itself deliberately contains no authentication secrets. All auth
settings absent still means the existing unauthenticated development mode.
Use the complete [OAuth configuration](mcp-auth.md) before exposing a production
listener. The reverse proxy must preserve streaming. Its network, TLS, Qdrant,
storage backup and deployment wiring remain infrastructure-owner work.

## Acceptance: isolated GPU smoke, then application integration

Run only when container/GPU testing is authorized and available. This does not
start a daemon, open a network port, contact Qdrant, or mount a vault. Use a
dedicated writable smoke-state directory, not production state.

```sh
docker run --rm --network none --read-only \
  --user 10001:10001 \
  --device /dev/kfd --device /dev/dri/renderD128 \
  --group-add "$(stat -c %g /dev/kfd)" \
  --group-add "$(stat -c %g /dev/dri/renderD128)" \
  --cap-drop ALL --security-opt no-new-privileges \
  --tmpfs /tmp:rw,nosuid,nodev,size=2g,mode=1777 \
  --mount type=bind,src="$ATTNDB_SMOKE_STATE",dst=/state \
  --entrypoint smoke-migraphx \
  attndb:migraphx-rocm10.2.0
```

`scripts/smoke-migraphx.sh` and `cmd/attndb-gpu-smoke` perform:

1. Native Go ORT initialization, explicit plugin registration, and actual EP
   device enumeration. A missing MIGraphX device is a hard failure; a Python
   `get_available_providers()` list alone is not sufficient.
2. Both production encoder constructors and their warmup shapes, then query
   **and** document inference for each model with CPU EP fallback disabled.
3. Checks for expected dimensions, finite/nonzero embeddings and nonempty
   serialized cache files beneath both model fingerprints.
4. A second independent process using the same isolated persistent cache.
   Both must exit zero. Cache files are retained and listed for inspection.

The isolated cache directory is printed. A second-process pass proves inference
works with persisted cache present, **not by itself that every graph loaded from
cache**. Capture provider cache-hit diagnostics and cold/warm timings before
claiming cache reuse or speedup. Small shape/control operations still follow
ORT's own execution semantics; this is a no-CPU-*EP-fallback* check, not a claim
that no CPU instructions run.

Before service promotion, separately validate real MCP indexing/search against
a disposable vault and test Qdrant namespace, OAuth, streaming, configured
sequence lengths, startup/warmup time, peak RAM/GPU shared memory, restart cache
reuse and sustained concurrent traffic. No such deployment/GPU result has
been obtained here. The host driver/userspace compatibility, model operator
coverage and full-model GPU memory requirements remain acceptance blockers.
