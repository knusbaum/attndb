#!/bin/bash
# Cross-compiles a linux/aarch64 wheel for fast-plaid (github.com/lightonai/fast-plaid),
# a transitive dependency of colbert-export (via pylate) with no published linux/aarch64
# wheel or sdist on PyPI. See docs/building.md for the full explanation.
#
# Run this from an amd64 host (or any host — it cross-compiles, no emulation needed) when
# bumping TORCH_VERSION or FASTPLAID_TAG. Output lands in third_party/, replacing the existing
# wheel there; commit the result.
set -euo pipefail

TORCH_VERSION=2.9.0
FASTPLAID_TAG=1.4.6       # git tag in lightonai/fast-plaid
FASTPLAID_CI_CONFIG=ci-290.toml  # the tag's ci-*.toml matching TORCH_VERSION (2.9.0 -> 290)
PYTHON_TAG=cp311
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'docker run --rm -v "$WORK":/w alpine sh -c "rm -rf /w/repo/target" 2>/dev/null; rm -rf "$WORK"' EXIT

echo "== cloning fast-plaid @ $FASTPLAID_TAG =="
git clone --quiet https://github.com/lightonai/fast-plaid.git "$WORK/repo"
git -C "$WORK/repo" fetch --quiet --tags
git -C "$WORK/repo" checkout --quiet "$FASTPLAID_TAG"
cp "$WORK/repo/$FASTPLAID_CI_CONFIG" "$WORK/repo/pyproject.toml"

# The checked-in .cargo/config.toml forces LIBTORCH_USE_PYTORCH=1, which makes tch's
# build script `exec` a Python interpreter to introspect an installed torch package —
# fine natively, but wrong when cross-compiling (it would introspect the *host's*
# python/torch, not the target's). Drop that forced env var; we supply LIBTORCH
# directly below, pointing at an aarch64 torch wheel's files (never executed).
sed -i '/LIBTORCH_USE_PYTORCH/d' "$WORK/repo/.cargo/config.toml"

echo "== fetching torch==$TORCH_VERSION (aarch64 wheel, contents only — nothing executed) =="
mkdir -p "$WORK/torch"
pip download --no-deps --only-binary=:all: \
  --platform manylinux_2_28_aarch64 --python-version 311 --implementation cp --abi cp311 \
  -d "$WORK/torch" "torch==$TORCH_VERSION"
python3 -c "
import zipfile, glob
whl = glob.glob('$WORK/torch/torch-*.whl')[0]
zipfile.ZipFile(whl).extractall('$WORK/torch/extracted')
"

cat > "$WORK/build.sh" <<'EOF'
#!/bin/bash
set -eux
export DEBIAN_FRONTEND=noninteractive
dpkg --add-architecture arm64
apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  gcc-aarch64-linux-gnu g++-aarch64-linux-gnu libc6-dev-arm64-cross \
  python3 python3-dev python3-pip python3-venv curl ca-certificates pkg-config \
  libpython3.11-dev:arm64 >/dev/null

rustup target add aarch64-unknown-linux-gnu
python3 -m venv /tmp/venv
/tmp/venv/bin/pip install -q --upgrade pip
/tmp/venv/bin/pip install -q maturin

export CARGO_TARGET_AARCH64_UNKNOWN_LINUX_GNU_LINKER=aarch64-linux-gnu-gcc
export CC_aarch64_unknown_linux_gnu=aarch64-linux-gnu-gcc
export CXX_aarch64_unknown_linux_gnu=aarch64-linux-gnu-g++
export PKG_CONFIG_ALLOW_CROSS=1
export LIBTORCH=/torch/torch
export LIBTORCH_BYPASS_VERSION_CHECK=1
export LIBTORCH_CXX11_ABI=1

cd /repo
/tmp/venv/bin/maturin build --release --target aarch64-unknown-linux-gnu -i python3.11 --out /out --manylinux off
EOF

echo "== cross-compiling (no emulation) =="
mkdir -p "$WORK/out" "$WORK/cargo-registry"
docker run --rm --platform linux/amd64 \
  -v "$WORK/repo":/repo \
  -v "$WORK/torch/extracted":/torch \
  -v "$WORK/out":/out \
  -v "$WORK/cargo-registry":/usr/local/cargo/registry \
  -v "$WORK/build.sh":/build.sh:ro \
  rust:1-bookworm bash /build.sh

wheel="$(ls "$WORK"/out/fast_plaid-*-"$PYTHON_TAG"-*-linux_aarch64.whl)"
mkdir -p "$REPO_ROOT/third_party"
rm -f "$REPO_ROOT"/third_party/fast_plaid-*-linux_aarch64.whl
cp "$wheel" "$REPO_ROOT/third_party/"
echo "== wrote $REPO_ROOT/third_party/$(basename "$wheel") =="
echo "Update the FASTPLAID_WHEEL / torch pin in the Dockerfile's models stage to match if the filename changed."
