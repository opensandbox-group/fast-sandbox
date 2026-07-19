#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
output="$repo_root/sdk/python/fast_sandbox/proto"

python3 -m grpc_tools.protoc \
  -I "$repo_root/api/proto/v1" \
  --python_out="$output" \
  --grpc_python_out="$output" \
  "$repo_root/api/proto/v1/fastpath.proto"

python3 - "$output/fastpath_pb2_grpc.py" <<'PY'
from pathlib import Path
import sys

path = Path(sys.argv[1])
text = path.read_text()
text = text.replace("import fastpath_pb2 as fastpath__pb2", "from . import fastpath_pb2 as fastpath__pb2")
path.write_text(text)
PY
