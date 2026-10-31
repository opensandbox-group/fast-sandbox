#!/bin/sh
# Package node-installed OCI binaries and the patched OverlayBD ext2fs library.
set -eu
source_dir=${1:?usage: package-tools.sh OVERLAYBD_DIR FSB_VM OUTPUT_DIR}
fsb_vm=${2:?}
out=${3:?}
mkdir -p "$out/bin" "$out/libexec" "$out/lib"
cp "$fsb_vm" "$out/bin/fsb-vm"
cp -L "$source_dir/lib/"* "$out/lib/"
for tool in overlaybd-ublkd overlaybd-create overlaybd-commit overlaybd-merge overlaybd-resize overlaybd-import-raw; do
  cp "$source_dir/bin/$tool" "$out/libexec/$tool"
  cat > "$out/bin/$tool" <<'WRAPPER'
#!/bin/sh
root=$(CDPATH= cd -P -- "$(dirname -- "$0")/.." && pwd)
export LD_LIBRARY_PATH="$root/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
exec "$root/libexec/$(basename -- "$0")" "$@"
WRAPPER
  chmod 0755 "$out/bin/$tool"
done
cd "$out"
find bin lib libexec -type f | LC_ALL=C sort | xargs sha256sum > SHA256SUMS
sha256sum SHA256SUMS | cut -d ' ' -f 1 > REVISION
