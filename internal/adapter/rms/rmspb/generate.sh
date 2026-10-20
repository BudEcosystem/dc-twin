#!/usr/bin/env bash
# Regenerates the RMS Go bindings from proto/ (NOTICE). Needs protoc 31.1 (with its include/
# directory) and, on PATH, protoc-gen-go v1.36.12 and protoc-gen-go-grpc v1.5.1:
#   GOBIN=$PWD/.gobin go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
#   GOBIN=$PWD/.gobin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1
# PROTOC is the protoc binary; PROTOC_INCLUDE the release's include/ directory.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../../.." && pwd)
PROTOC=${PROTOC:-protoc}
PROTOC_INCLUDE=${PROTOC_INCLUDE:-$(dirname "$(command -v "$PROTOC")")/../include}
mod=github.com/BudEcosystem/dc-twin
m1="Mrack_manager.proto=$mod/internal/adapter/rms/rmspb;rmspb"
m2="Mrack_manager_v2.proto=$mod/internal/adapter/rms/rmspb/rmsv2pb;rmsv2pb"
cd "$root"
"$PROTOC" -I "$here/proto" -I "$PROTOC_INCLUDE" \
  --go_out=. --go_opt=module=$mod --go_opt="$m1" --go_opt="$m2" \
  --go-grpc_out=. --go-grpc_opt=module=$mod --go-grpc_opt="$m1" --go-grpc_opt="$m2" \
  rack_manager.proto rack_manager_v2.proto
