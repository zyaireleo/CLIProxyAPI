#!/usr/bin/env bash
set -euo pipefail

if [[ "${CGO_ENABLED:-1}" != 1 ]]; then
  echo 'Plugin candidates require CGO_ENABLED=1' >&2
  exit 2
fi
if [[ "${GOOS:-linux}" != linux || "${GOARCH:-amd64}" != amd64 || "${GOAMD64:-v1}" != v1 ]]; then
  echo 'Plugin candidates require linux/amd64 with GOAMD64=v1' >&2
  exit 2
fi

output_dir="${1:?usage: build-plugin-candidate.sh OUTPUT_DIR}"
repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"
export CGO_ENABLED=1 GOOS=linux GOARCH=amd64 GOAMD64=v1 GOTOOLCHAIN=local

if [[ "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" != linux/amd64 ]]; then
  echo 'Build plugin candidates in a Linux amd64 environment' >&2
  exit 2
fi
if [[ "$(go env GOVERSION)" != go1.27.0 ]]; then
  echo 'Plugin candidate toolchain must be go1.27.0' >&2
  exit 2
fi
if ! git diff --quiet HEAD --; then
  echo 'Commit tracked source changes before building a candidate' >&2
  exit 2
fi

mkdir -p "$output_dir"
output_dir="$(cd "$output_dir" && pwd)"
binary="$output_dir/cli-proxy-api"
if [[ -e "$binary" || -e "$output_dir/artifact.json" ]]; then
  echo 'Refusing to overwrite an existing candidate artifact' >&2
  exit 2
fi

revision="$(git rev-parse HEAD)"
release="cpa-plugin-${revision:0:12}"
built_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
go build -trimpath -buildvcs=false \
  -ldflags="-s -w -X main.Version=$release -X main.Commit=$revision -X main.BuildDate=$built_at" \
  -o "$binary" ./cmd/server
go run ./deploy/verify-plugin-artifact --max-glibc 2.35 "$binary" > "$output_dir/artifact.json"
printf 'revision=%s\nrelease=%s\nbuilt_at=%s\n' "$revision" "$release" "$built_at" > "$output_dir/SOURCE"
git ls-tree -r HEAD internal/registry > "$output_dir/MODEL_TREE"
sha256sum "$binary" > "$output_dir/SHA256SUMS"
echo "Verified plugin candidate: $binary"
