#!/bin/sh
# Container-only OverlayBD build recipe shared by the Builder and Fastlet.
set -eu
out=${1:?usage: build-overlaybd.sh OUTPUT_DIR}
tools_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
revision=${OVERLAYBD_REV:-5119193d540010e09c0af98add1ef2b0f4a51bc0}

apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
  autoconf automake build-essential ca-certificates cmake git \
  libaio-dev libcurl4-openssl-dev libext2fs-dev libgflags-dev libglib2.0-dev \
  libnl-3-dev libnl-genl-3-dev libssl-dev libtool libzstd-dev \
  pkg-config uuid-dev zlib1g-dev
rm -rf /var/lib/apt/lists/*

git clone https://github.com/containerd/overlaybd.git /src/overlaybd
cd /src/overlaybd
git checkout --detach "$revision"
install -m 0644 "$tools_dir/overlaybd-import-raw.cpp" src/tools/overlaybd-import-raw.cpp
sed -i '/add_executable(overlaybd-create/i add_executable(overlaybd-import-raw overlaybd-import-raw.cpp)\ntarget_include_directories(overlaybd-import-raw PUBLIC ${PHOTON_INCLUDE_DIR})\ntarget_link_libraries(overlaybd-import-raw photon_static overlaybd_lib)\n' src/tools/CMakeLists.txt
# Third-party source is built container-only; suppress its default-warning
# noise so real errors stay visible in build logs.
cmake -S . -B build -DCMAKE_BUILD_TYPE=Release -DBUILD_TESTING=OFF \
  -DCMAKE_CXX_FLAGS="-w" \
  -DBUILD_STREAM_CONVERTOR=OFF -DBUILD_UBLK_FRONTEND=ON -DORIGIN_EXT2FS=OFF
cmake --build build --target overlaybd-import-raw overlaybd-ublkd \
  overlaybd-create overlaybd-apply overlaybd-commit overlaybd-merge overlaybd-resize \
  --parallel "${JOBS:-8}"
mkdir -p "$out/bin" "$out/lib"
for tool in overlaybd-import-raw overlaybd-ublkd overlaybd-create overlaybd-apply overlaybd-commit overlaybd-merge overlaybd-resize; do
  install -m 0755 "build/output/$tool" "$out/bin/$tool"
done
# OverlayBD's patched ext2fs library must take precedence over the system copy.
cp -L build/_deps/e2fsprogs-src/build/libext2fs/lib/libext2fs.so "$out/lib/libext2fs.so.2"
