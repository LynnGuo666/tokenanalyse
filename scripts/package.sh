#!/usr/bin/env bash
set -euo pipefail

VERSION="${1:-0.1.0}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"
mkdir -p "$DIST"

TARGETS=(
  "linux amd64"
  "linux arm64"
  "darwin amd64"
  "darwin arm64"
  "windows amd64"
  "windows arm64"
)

for target in "${TARGETS[@]}"; do
  read -r GOOS GOARCH <<<"$target"
  NAME="tokenanalyse_${VERSION}_${GOOS}_${GOARCH}"
  BIN="$DIST/${NAME}"
  if [[ "$GOOS" == "windows" ]]; then
    BIN="${BIN}.exe"
  fi

  echo "Building $GOOS/$GOARCH"
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o "$BIN" ./cmd/tokenanalyse

  STAGE="$DIST/.stage-${NAME}"
  rm -rf "$STAGE"
  mkdir -p "$STAGE"
  cp "$BIN" "$STAGE/"
  cp "$ROOT/README.md" "$ROOT/LICENSE" "$ROOT/NOTICE" "$STAGE/"
  cp -R "$ROOT/deploy" "$STAGE/"
  if [[ "$GOOS" == "windows" ]]; then
    python3 - "$STAGE" "$DIST/${NAME}.zip" <<'PY'
import pathlib
import sys
import zipfile

root = pathlib.Path(sys.argv[1])
out = pathlib.Path(sys.argv[2])
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as archive:
    for path in root.rglob("*"):
        if path.is_file():
            archive.write(path, path.relative_to(root))
PY
  else
    tar -C "$STAGE" -czf "$DIST/${NAME}.tar.gz" .
  fi
  rm -rf "$STAGE" "$BIN"
done

echo "Packages written to $DIST"
