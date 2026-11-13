#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST_DIR="${ROOT_DIR}/dist"
mkdir -p "${DIST_DIR}"

TARGETS=(
  "linux/amd64/focusgate-linux-amd64"
  "linux/arm64/focusgate-linux-arm64"
  "darwin/amd64/focusgate-darwin-amd64"
  "darwin/arm64/focusgate-darwin-arm64"
  "windows/amd64/focusgate-windows-amd64.exe"
)

export CGO_ENABLED=0

cd "${ROOT_DIR}"
for target in "${TARGETS[@]}"; do
  IFS="/" read -r GOOS GOARCH OUT <<< "${target}"
  echo "Building ${GOOS}/${GOARCH} -> ${DIST_DIR}/${OUT}"
  GOOS="${GOOS}" GOARCH="${GOARCH}" go build -trimpath -ldflags="-s -w" -o "${DIST_DIR}/${OUT}" ./cmd/focusgate
done

echo "Build complete. Artifacts in ${DIST_DIR}:"
ls -lh "${DIST_DIR}"
