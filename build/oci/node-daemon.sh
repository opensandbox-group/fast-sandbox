#!/bin/sh
# Run one ublkd per node. Fastlets connect directly to its shared UDS.
set -eu
state_root=${FAST_SANDBOX_STATE_ROOT:-/var/lib/fast-sandbox/firecracker}
socket_dir=/run/fast-sandbox/overlaybd
tools=/opt/fast-sandbox/oci/current/bin
cache="$state_root/overlaybd/cache"
mkdir -p "$socket_dir" "$cache"
chmod 0700 "$socket_dir"
exec 9>"$socket_dir/daemon.lock"
flock -n 9 || { echo 'another node ublkd owns the socket/cache' >&2; exit 1; }
rm -f "$socket_dir/ublkd.sock"
cat > "$socket_dir/service.json" <<EOF_JSON
{
  "logConfig": {"logLevel": 1, "logPath": "/dev/stdout"},
  "cacheConfig": {"cacheType": "file", "cacheDir": "$cache", "cacheSizeGB": ${OVERLAYBD_CACHE_SIZE_GB:-16}},
  "gzipCacheConfig": {"enable": false},
  "credentialConfig": {"mode": "file", "path": "$socket_dir/cred.json"},
  "ioEngine": 0, "workpoolSize": 8,
  "download": {"enable": false}, "p2pConfig": {"enable": false},
  "exporterConfig": {"enable": true, "uriPrefix": "/metrics", "port": 9863, "updateInterval": 1000000},
  "registryFsVersion": "v2", "serviceConfig": {"enable": false}
}
EOF_JSON
# Warm pool: LOW=0 disables it; otherwise SIZE_GB is required. All three values
# must be non-negative integers.
pool_low=${OVERLAYBD_POOL_LOW:-0}
pool_high=${OVERLAYBD_POOL_HIGH:-$pool_low}
pool_size=${OVERLAYBD_POOL_SIZE_GB:-0}
if ! { [ "$pool_low" -ge 0 ] && [ "$pool_high" -ge 0 ] && [ "$pool_size" -ge 0 ]; } 2>/dev/null; then
  echo 'pool values must be non-negative integers' >&2
  exit 1
elif [ "$pool_low" != 0 ] && [ "$pool_size" = 0 ]; then
  echo 'OVERLAYBD_POOL_SIZE_GB must be set when pool-low > 0' >&2
  exit 1
fi
exec "$tools/overlaybd-ublkd" --socket-path "$socket_dir/ublkd.sock" \
     --service-config "$socket_dir/service.json" --cache-dir "$cache" \
     --pool-low "$pool_low" \
     --pool-high "$pool_high" \
     --pool-size-gb "$pool_size"
