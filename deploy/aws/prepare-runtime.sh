#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "Usage: $0 /absolute/runtime/path" >&2
}

if [[ $# -ne 1 || "$1" != /* ]]; then
  usage
  exit 2
fi

runtime_root="$1"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
manifest="${script_dir}/release.env"
config_target="${runtime_root}/config.yaml"
compose_target="${runtime_root}/docker-compose.yml"
secrets_target="${runtime_root}/secrets.env"
runtime_env_target="${runtime_root}/.env"

if [[ ! -f "${manifest}" ]]; then
  echo "Release manifest not found: ${manifest}" >&2
  exit 1
fi

# shellcheck source=release.env
source "${manifest}"
: "${BRAVO_VERSION:?BRAVO_VERSION is missing from release.env}"
: "${CLIPROXYAPI_VERSION:?CLIPROXYAPI_VERSION is missing from release.env}"
: "${CLIPROXYAPI_IMAGE:?CLIPROXYAPI_IMAGE is missing from release.env}"

if [[ -e "${runtime_root}" && ! -d "${runtime_root}" ]]; then
  echo "Runtime path exists and is not a directory: ${runtime_root}" >&2
  exit 1
fi

if [[ -d "${runtime_root}" ]]; then
  existing_entry="$(find "${runtime_root}" -mindepth 1 -maxdepth 1 -print -quit)"
  if [[ -n "${existing_entry}" ]]; then
    echo "Refusing to initialize non-empty runtime: ${runtime_root}" >&2
    exit 1
  fi
fi

command -v openssl >/dev/null 2>&1 || {
  echo "openssl is required" >&2
  exit 1
}

umask 077
install -d -m 0700 \
  "${runtime_root}" \
  "${runtime_root}/auths" \
  "${runtime_root}/bravo-data" \
  "${runtime_root}/logs"

management_key="$(openssl rand -hex 32)"
ordinary_api_key="$(openssl rand -hex 32)"

config_contents="$(<"${script_dir}/config.yaml.example")"
config_contents="${config_contents//__MANAGEMENT_KEY__/${management_key}}"
config_contents="${config_contents//__ORDINARY_API_KEY__/${ordinary_api_key}}"
printf '%s\n' "${config_contents}" >"${config_target}"
chmod 0600 "${config_target}"

install -m 0600 "${script_dir}/docker-compose.yml" "${compose_target}"
{
  printf 'BRAVO_VERSION=%s\n' "${BRAVO_VERSION}"
  printf 'CLIPROXYAPI_VERSION=%s\n' "${CLIPROXYAPI_VERSION}"
  printf 'CLIPROXYAPI_IMAGE=%s\n' "${CLIPROXYAPI_IMAGE}"
} >"${runtime_env_target}"
chmod 0600 "${runtime_env_target}"
{
  printf 'MANAGEMENT_KEY=%s\n' "${management_key}"
  printf 'ORDINARY_API_KEY=%s\n' "${ordinary_api_key}"
} >"${secrets_target}"
chmod 0600 "${secrets_target}"

echo "Fresh runtime prepared at ${runtime_root}"
echo "Operator secrets were written to ${secrets_target}"
echo "The secrets file is not passed into the container."
