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
  "android/arm64/focusgate-android-arm64"
)

cd "${ROOT_DIR}"
export CGO_ENABLED=0

VERSION="${VERSION:-$(git describe --tags --exact-match 2>/dev/null || echo "1.0.0")}"
if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+ && ! "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]]; then
  VERSION="1.0.0"
fi
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo "dev")}"
BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
LDFLAGS="-s -w -X focusgate/internal/version.Version=${VERSION} -X focusgate/internal/version.Commit=${COMMIT} -X focusgate/internal/version.Date=${BUILD_DATE}"

for target in "${TARGETS[@]}"; do
  IFS="/" read -r GOOS GOARCH OUT <<< "${target}"
  echo "Building ${GOOS}/${GOARCH} -> ${DIST_DIR}/${OUT}"
  GOOS="${GOOS}" GOARCH="${GOARCH}" go build -trimpath -ldflags="${LDFLAGS}" -o "${DIST_DIR}/${OUT}" ./cmd/focusgate
done

echo "Build complete. Artifacts in ${DIST_DIR}:"
ls -lh "${DIST_DIR}"
