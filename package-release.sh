#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

plugin_id="cpa-quota-estimator"
out_dir="${1:-dist}"
version="${PLUGIN_VERSION:-}"

if [[ -z "${version}" ]]; then
  version="$(sed -n 's/^[[:space:]]*var pluginVersion = "\([^"]*\)".*/\1/p' types.go | head -n 1)"
fi
if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "PLUGIN_VERSION must be a dotted numeric version, got: ${version:-<empty>}" >&2
  exit 1
fi

goos="$(go env GOOS)"
goarch="$(go env GOARCH)"
ext="so"
case "${goos}" in
  darwin) ext="dylib" ;;
  windows) ext="dll" ;;
esac

mkdir -p "${out_dir}"
artifact="${plugin_id}.${ext}"
zip_name="${plugin_id}_${version}_${goos}_${goarch}.zip"

CGO_ENABLED=1 go build -trimpath -buildvcs=false -buildmode=c-shared \
  -ldflags="-s -w -X main.pluginVersion=${version}" \
  -o "${artifact}" .

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}" "${artifact}" "${artifact%.${ext}}.h"' EXIT
cp "${artifact}" "${tmp_dir}/${artifact}"

if command -v zip >/dev/null 2>&1; then
  (cd "${tmp_dir}" && zip -9 -q "${OLDPWD}/${out_dir}/${zip_name}" "${artifact}")
else
  python3 - "${tmp_dir}" "${artifact}" "${out_dir}/${zip_name}" <<'PY'
import pathlib
import sys
import zipfile

source = pathlib.Path(sys.argv[1]) / sys.argv[2]
destination = pathlib.Path(sys.argv[3])
destination.parent.mkdir(parents=True, exist_ok=True)
with zipfile.ZipFile(destination, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
    archive.write(source, source.name)
PY
fi

(
  cd "${out_dir}"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${zip_name}" > checksums.txt
  else
    shasum -a 256 "${zip_name}" > checksums.txt
  fi
)

echo "Created ${out_dir}/${zip_name}"
echo "Created ${out_dir}/checksums.txt"
