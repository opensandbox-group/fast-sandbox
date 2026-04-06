#!/bin/bash
set -ex

echo "Starting Kata installation..."
export NODE_NAME="${NODE_NAME:-$(hostname)}"
export DEBUG=true

# Copy kata binaries to host
if [ -d /opt/kata ]; then
  echo "Copying Kata to /host/opt/kata..."
  mkdir -p /host/opt/kata
  cp -a /opt/kata/* /host/opt/kata/
fi

# Configure containerd
if [ -f /host/etc/containerd/config.toml ]; then
  echo "Configuring containerd..."
  if ! grep -q "kata-clh" /host/etc/containerd/config.toml; then
    cat >> /host/etc/containerd/config.toml <<'TOML'

# Kata Containers runtime configurations
[plugins."io.containerd.runtime.v1.linux".runtimes.kata-clh]
  runtime_type = "io.containerd.kata-clh.v2"
  runtime_path = "/opt/kata/bin/containerd-shim-kata-v2"
  privileged_without_host_devices = true
  [plugins."io.containerd.runtime.v1.linux".runtimes.kata-clh.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-clh.toml"

[plugins."io.containerd.runtime.v1.linux".runtimes.kata-qemu]
  runtime_type = "io.containerd.kata-qemu.v2"
  runtime_path = "/opt/kata/bin/containerd-shim-kata-v2"
  privileged_without_host_devices = true
  [plugins."io.containerd.runtime.v1.linux".runtimes.kata-qemu.options]
    ConfigPath = "/opt/kata/share/defaults/kata-containers/configuration-qemu.toml"
TOML
  fi
fi

echo "Kata installation complete!"
ls -la /host/opt/kata/bin/ 2>/dev/null || echo "No binaries found"

# Keep running
sleep infinity