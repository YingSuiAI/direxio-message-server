#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# This is the only wrapper that claims a first-fresh consumer gate.  It is
# intentionally opt-in because provisioning and startup create Docker state
# and may build images.  The fixture-only verify-topology.sh must not call it.

usage() {
  cat >&2 <<'EOF'
usage:
  DIREXTALK_FIRST_FRESH_AUTHORIZED=true verify-first-fresh.sh \
    --execute-first-fresh OUTPUT_DIR OPENROUTER_KEY_FILE EMBEDDING_KEY_FILE \
    TAVILY_KEY_FILE PORTAL_PASSWORD_FILE CHAT_MODEL EMBEDDING_MODEL
EOF
  exit 2
}

die() {
  echo "split-stack first-fresh gate: $*" >&2
  exit 1
}

[ "$#" -eq 8 ] || usage
[ "$1" = --execute-first-fresh ] || usage
[ "${DIREXTALK_FIRST_FRESH_AUTHORIZED:-false}" = true ] || die "set DIREXTALK_FIRST_FRESH_AUTHORIZED=true for the mutating first-fresh gate"
[ "${DIREXTALK_SPLIT_FIXTURE_MODE:-false}" != true ] || die "fixture mode is forbidden for the first-fresh gate"
compose_mode=${DIREXTALK_FIRST_FRESH_COMPOSE_MODE:-local}
case "$compose_mode" in
  local|production) ;;
  *) die "DIREXTALK_FIRST_FRESH_COMPOSE_MODE must be local or production" ;;
esac
if [ "$compose_mode" = production ]; then
  [ "${DIREXTALK_CORE_EXTENSION_ENABLED:-false}" = true ] || die "production first-fresh requires DIREXTALK_CORE_EXTENSION_ENABLED=true"
  [ "${DIREXTALK_CORE_WORKLOAD_ENABLED:-false}" = true ] || die "production first-fresh requires DIREXTALK_CORE_WORKLOAD_ENABLED=true"
fi

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
provisioner=$script_dir/provision-local.sh
starter=$script_dir/start-local.sh
consumer=$script_dir/accept-local.sh
for helper in "$provisioner" "$starter" "$consumer"; do
  [ -x "$helper" ] || die "required first-fresh helper is missing or not executable: $helper"
done

output_input=$2
openrouter_key=$3
embedding_key=$4
tavily_key=$5
portal_password=$6
chat_model=$7
embedding_model=$8

case "$output_input" in
  /*) output_dir=$(readlink -m -- "$output_input") ;;
  *) output_dir=$(readlink -m -- "$(pwd -P)/$output_input") ;;
esac
[ "$output_dir" != / ] || die "refusing to use / as a fresh output directory"
[ ! -e "$output_dir" ] && [ ! -L "$output_dir" ] || die "OUTPUT_DIR must not exist; first-fresh requires a new namespace"

for source in "$openrouter_key" "$embedding_key" "$tavily_key" "$portal_password"; do
  case "$source" in
    /*) ;;
    *) die "secret source paths must be absolute" ;;
  esac
done
[ -n "$chat_model" ] || die "CHAT_MODEL must not be empty"
[ -n "$embedding_model" ] || die "EMBEDDING_MODEL must not be empty"

DIREXTALK_SPLIT_COMPOSE_MODE="$compose_mode" \
  "$provisioner" "$output_dir" "$openrouter_key" "$embedding_key" "$tavily_key" "$portal_password"
DIREXTALK_SPLIT_COMPOSE_MODE="$compose_mode" \
  "$starter" "$output_dir/.env"
DIREXTALK_ACCEPTANCE_COMPOSE_MODE="$compose_mode" \
  "$consumer" "$output_dir" "$chat_model" "$embedding_model"

# This marker is emitted only after the real provision -> Compose startup ->
# public-interface acceptance consumer path has completed successfully.  The
# wrapper deliberately leaves cleanup to a separately authorized command.
printf 'first-fresh consumer gate passed: %s\n' "$output_dir"
