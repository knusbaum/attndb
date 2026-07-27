# Building attndb

For running attndb day to day, use Docker — see the [README](../README.md). This
doc is for building the binary yourself (native encoders, CoreML, multi-arch
images) or developing attndb itself.

## Default build (stub encoders)

No native dependencies. Deterministic hash embeddings — enough to exercise the
whole pipeline, not real semantic search:

```
go build ./...
go run ./cmd/attndb query -k 3 "retry backoff when the identity service is unavailable"
```

## Real encoders (ONNX), without Docker

The default build uses stub encoders. The real semantic encoders live behind the
`onnx` build tag because they link native libraries (ONNX Runtime + HF
tokenizers).

1. **Export the models** (the only Python in the project — runs offline, in an
   isolated pipenv env so it never touches your global interpreter):
   ```
   pipenv install     # uses the Pipfile (colbert-export, onnxscript)
   pipenv run python -c "from colbert_export import export_model; export_model('lightonai/GTE-ModernColBERT-v1', output_dir='models', quantize=True)"
   pipenv run python scripts/export_single.py     # gte-modernbert-base -> models/single/
   ```
   This produces `models/` (per-token ColBERT) and `models/single/`
   (single-vector), each with `model.onnx`, `model.onnx.data`, and
   `tokenizer.json`. Neither `models/` (~1.4 GB) nor `Pipfile.lock` is committed:
   a lock holds one entry per package, but torch differs by platform on the CPU
   wheel index (Linux `2.11.0+cpu` vs macOS `2.11.0`), so a committed lock would
   break whichever OS it wasn't generated on — `pipenv install` resolves it per
   machine instead. On Linux, torch comes from PyTorch's CPU-only index (PyPI's
   Linux wheel is the CUDA build and drags in ~2.5 GB of `nvidia-*` packages the
   offline, CPU-only export never uses).

2. **Fetch the native libs** into `libs/` (~70 MB, not committed) — both the
   tokenizer static lib (linked at build) and the ONNX Runtime shared lib
   (dlopen'd at run time):
   - **Linux (amd64)**:
     ```
     mkdir -p libs
     curl -sSL https://github.com/daulet/tokenizers/releases/download/v1.27.0/libtokenizers.linux-amd64.tar.gz | tar -xz -C libs
     curl -sSL https://github.com/microsoft/onnxruntime/releases/download/v1.27.0/onnxruntime-linux-x64-1.27.0.tgz -o /tmp/ort.tgz
     tar -xzf /tmp/ort.tgz -C /tmp
     cp -a /tmp/onnxruntime-linux-x64-1.27.0/lib/libonnxruntime.so* libs/
     ```
   - **macOS (Apple Silicon)**:
     ```
     mkdir -p libs
     curl -sSL https://github.com/daulet/tokenizers/releases/download/v1.27.0/libtokenizers.darwin-arm64.tar.gz | tar -xz -C libs
     curl -sSL https://github.com/microsoft/onnxruntime/releases/download/v1.27.0/onnxruntime-osx-arm64-1.27.0.tgz -o /tmp/ort.tgz
     tar -xzf /tmp/ort.tgz -C /tmp
     cp -a /tmp/onnxruntime-osx-arm64-1.27.0/lib/libonnxruntime.1.27.0.dylib libs/
     ```
   The CoreML execution provider is only in the macOS build of ONNX Runtime; if
   you want `-provider coreml`, a pip- or Homebrew-installed ORT dylib also works
   (see [Metal/CoreML](#metalcoreml-acceleration-macos) below).

3. **Build and run**:
   ```
   make onnx        # or: CGO_LDFLAGS="-L$(pwd)/libs" go build -tags onnx -o attndb ./cmd/attndb
   ./attndb query -encoder onnx -store memory -k 3 "how long do we keep customer information"
   ```
   ONNX Runtime is dlopen'd at run time. The default path is resolved per
   platform (`libs/libonnxruntime.so.<version>` on Linux,
   `libs/libonnxruntime.<version>.dylib` on macOS — see
   `internal/encode/onnx/colbert.go`); override with `ATTNDB_ORT_LIB`, which
   `make` sets for you.

Other Makefile targets: `make test` (stub-encoder test suite, no native deps),
`make vet`, `make fmt`, `make ingest`/`make search Q="..."` (assume Qdrant up and
`corpus/` populated). Machine-local overrides (codesigning, personal run
targets) go in a git-ignored `Makefile.local` — see `Makefile.local.example`.

## Metal/CoreML acceleration (macOS)

The ONNX Runtime CoreML Execution Provider runs ops on the Metal GPU. It is
built into the standard macOS ORT dylib (from pip or Homebrew).

```
# point at the ORT dylib; pip-installed ORT is the easiest source
export ATTNDB_ORT_LIB=$(python3 -c \
  "import onnxruntime, pathlib; \
   print(next(pathlib.Path(onnxruntime.__file__).parent.glob('capi/libonnxruntime*.dylib')))")

# one-shot ingest + search on the Metal GPU
./attndb query -encoder onnx -provider coreml -k 3 "retry backoff when the service is unavailable"

# benchmark: compare CPU vs CoreML on the same corpus
time ./attndb query -encoder onnx -provider cpu    -k 3 "..."
time ./attndb query -encoder onnx -provider coreml -k 3 "..."
```

`-provider coreml` enables `MLComputeUnits=CPUAndGPU` (Metal GPU + CPU fallback
for any ops CoreML can't handle). It also enables ORT verbose logging so
node-to-EP assignments appear on stderr — look for `Node ... assigned to
CoreMLExecutionProvider` to confirm ops actually ran on Metal. If CoreML is not
compiled into the dylib, the flag errors loud rather than silently falling back.

Inputs are padded to fixed shapes (ColBERT: 48/300 tokens; single-vector: 512
tokens) so CoreML can compile a static graph. Without static shapes the CoreML
EP falls back to CPU for transformer ops.

## Building multi-arch Docker images

The image (see the [README](../README.md#quickstart) for running it) is multi-arch
(`linux/amd64` + `linux/arm64`), and **building it needs no emulation**. Both the
Go stage and the Python model-export stage run on the *build* machine: Go
cross-compiles with a Debian cross toolchain (`gcc/g++-<arch>-linux-gnu` +
`libc6-dev-<arch>-cross`), and the model export emits architecture-independent
ONNX (pinned to `--platform=$BUILDPLATFORM` in the Dockerfile). Nothing foreign
executes during a build — it is only written.

**One dependency needed a workaround to build on an arm64 host.** `pylate` (via
`colbert-export`) pulls in `fast-plaid`, which publishes no `linux/aarch64`
wheel or sdist on PyPI — root cause: it links `libtorch` via the Rust `tch`
crate, and PyTorch itself doesn't publish a standalone linux/aarch64 `libtorch`
archive (only macOS-arm64 and Windows-arm64 exist). `third_party/` carries a wheel
built from fast-plaid's own source via true cross-compilation (no emulation) —
see `scripts/build-fastplaid-aarch64.sh`. The models stage installs it on
aarch64 instead of hitting PyPI. Rebuild it (and update the pin below) when
bumping the `TORCH_VERSION` build arg — the wheel is linked against one exact
torch version and won't work with a different one at runtime.

```
# your own architecture
docker compose build attndb

# a single foreign-arch image works on the default builder
docker buildx build --platform linux/arm64 -t attndb:arm64 --load .

# both at once needs the container driver — the default `docker` driver
# cannot export a multi-platform manifest list
docker buildx create --use --name attndb-builder
docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/attndb:v1 --push .
```

You only need qemu/binfmt to **run** a foreign-arch image locally
(`docker run --platform linux/arm64 …`), never to build one:

```
docker run --privileged --rm tonistiigi/binfmt --install arm64
```

Cross-compiling rather than emulating is a correctness choice as much as a speed
one. Building Go under qemu-user is unreliable — the toolchain drives parallel
`compile` subprocesses through raw clone/futex/signals, and emulating that
races: observed here as every child exiting into a zombie while the parent spun
at 100% CPU, livelocked past 26 minutes, after an identical earlier build had
happened to succeed in 149s. Cross-compiled, the same step takes ~11s.

The model-export stage is pinned to the build machine for a different reason:
what it produces is architecture-independent data, so building it anywhere but
the host gains nothing. See the fast-plaid wheel note above for the one
dependency that needed a vendored build to resolve on arm64 at all.
