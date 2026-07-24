#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Usage: $0 /absolute/management-ui/path" >&2
}

if [[ $# -ne 1 || "$1" != /* ]]; then
  usage
  exit 2
fi

target="$1"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
manifest="${script_dir}/release.env"

if [[ ! -f "${manifest}" ]]; then
  echo "Release manifest not found: ${manifest}" >&2
  exit 1
fi

# shellcheck source=release.env
source "${manifest}"

: "${BRAVO_VERSION:?BRAVO_VERSION is missing from release.env}"
: "${CLIPROXYAPI_VERSION:?CLIPROXYAPI_VERSION is missing from release.env}"
: "${CLIPROXYAPI_IMAGE:?CLIPROXYAPI_IMAGE is missing from release.env}"
: "${RELEASE_PLATFORM:?RELEASE_PLATFORM is missing from release.env}"
: "${WEBUI_REPOSITORY:?WEBUI_REPOSITORY is missing from release.env}"
: "${WEBUI_COMMIT:?WEBUI_COMMIT is missing from release.env}"
: "${BUN_IMAGE:?BUN_IMAGE is missing from release.env}"

if [[ -e "${target}" ]]; then
  echo "Refusing to overwrite existing path: ${target}" >&2
  exit 1
fi

git clone --no-checkout "${WEBUI_REPOSITORY}" "${target}"
git -C "${target}" switch --detach "${WEBUI_COMMIT}"

actual_commit="$(git -C "${target}" rev-parse HEAD)"
if [[ "${actual_commit}" != "${WEBUI_COMMIT}" ]]; then
  echo "Management UI checkout mismatch: ${actual_commit}" >&2
  exit 1
fi

echo "Management UI ${actual_commit} checked out at ${target}"
