#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

output_dir="dist/linux"
binary_name="grok_switch"
binary_path="${output_dir}/${binary_name}"

go test ./...
mkdir -p "${output_dir}"
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "${binary_path}" .
sha256sum "${binary_path}" > "${binary_path}.sha256"

printf 'Built %s\n' "${binary_path}"
printf 'SHA-256 %s\n' "${binary_path}.sha256"
