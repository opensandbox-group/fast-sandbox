#!/usr/bin/env bash
set -Eeuo pipefail

pool_name="${1:?pool name is required}"
sandbox_name="${2:?sandbox name is required}"
data_plane="${3:-}"

echo "Terminal 1 (keep it running):"
echo "  make quickstart-forward"
echo
echo "Terminal 2 (copy/paste in order):"
echo "  bin/fastctl --endpoint localhost:9090 --proxy-endpoint http://localhost:18080 \\"
echo "    run ${sandbox_name} --image docker.io/library/alpine:latest \\"
echo "    --pool ${pool_name} -- /bin/sleep 3600"
echo
echo "  kubectl wait --for=jsonpath='{.status.dataPlaneState}'=Ready \\"
echo "    sandbox/${sandbox_name} --timeout=60s"
echo
echo "  bin/fastctl --endpoint localhost:9090 get ${sandbox_name}"
echo
echo "  bin/fastctl --endpoint localhost:9090 diagnostics sandbox ${sandbox_name}"

if [[ "${data_plane}" == "execd" ]]; then
  echo
  echo "  bin/fastctl --endpoint localhost:9090 --proxy-endpoint http://localhost:18080 \\"
  echo "    opensandbox exec ${sandbox_name} -- sh -lc 'printf \"hello from execd\\n\" > /tmp/execd.txt && cat /tmp/execd.txt'"
  echo
  echo "  printf 'hello from host\\n' > /tmp/fast-sandbox-quickstart.txt"
  echo "  bin/fastctl --endpoint localhost:9090 --proxy-endpoint http://localhost:18080 \\"
  echo "    opensandbox cp /tmp/fast-sandbox-quickstart.txt ${sandbox_name}:/tmp/from-host.txt"
  echo "  bin/fastctl --endpoint localhost:9090 --proxy-endpoint http://localhost:18080 \\"
  echo "    opensandbox files stat ${sandbox_name} /tmp/from-host.txt"
  echo "  bin/fastctl --endpoint localhost:9090 --proxy-endpoint http://localhost:18080 \\"
  echo "    opensandbox files read ${sandbox_name} /tmp/from-host.txt"
  echo "  bin/fastctl --endpoint localhost:9090 --proxy-endpoint http://localhost:18080 \\"
  echo "    opensandbox cp ${sandbox_name}:/tmp/execd.txt /tmp/execd-downloaded.txt"
else
  echo
  echo "This Pool uses infraProfile=minimal; OpenSandbox exec/file commands are intentionally unavailable."
fi

echo
echo "  bin/fastctl --endpoint localhost:9090 delete ${sandbox_name}"
