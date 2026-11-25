#!/usr/bin/env bash
set -euo pipefail

# Build from a downstream module. Go only honors replacements in its main module.
root=$(cd "$(dirname "$0")/.." && pwd)
sdk=github.com/dtelecom/server-sdk-go
mkdir "${1:?usage: prepare-consumer.sh NEW_DIRECTORY}"
cd "$1"
export GOWORK=off
go mod init example.com/sdk-consumer
go mod edit -require "$sdk@v0.0.0" -replace "$sdk=$root"
# Use the same public imports a Go participant and its Cloud backend use.
cp "$root/cloud/integration_test.go" consumer_test.go
go mod tidy
go test -mod=readonly -count=1 "$sdk" "$sdk/cloud"
go build -mod=readonly -o room-management "$sdk/examples/room-management"
go test -mod=readonly -tags integration -c -o live-room.test .
protocol_replacement=$(go list -m -f '{{if .Replace}}{{.Replace.Path}}{{end}}' github.com/livekit/protocol)
if [[ -n "$protocol_replacement" ]]; then
  echo 'The downstream consumer unexpectedly replaced github.com/livekit/protocol.' >&2
  exit 1
fi
