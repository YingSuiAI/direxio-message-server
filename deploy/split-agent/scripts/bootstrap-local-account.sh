#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 OUTPUT_DIR USERNAME PASSWORD_FILE" >&2
  exit 2
}

die() {
  echo "local account bootstrap: $*" >&2
  exit 1
}

[ "$#" -eq 3 ] || usage
out_input=$1
username=$2
password_file_input=$3
case "$out_input" in
  /*) out=$(readlink -m -- "$out_input") ;;
  *) out=$(readlink -m -- "$(pwd -P)/$out_input") ;;
esac
case "$password_file_input" in
  /*) password_file=$(readlink -f -- "$password_file_input" 2>/dev/null || true) ;;
  *) password_file=$(readlink -f -- "$(pwd -P)/$password_file_input" 2>/dev/null || true) ;;
esac
[ -d "$out" ] && [ ! -L "$out" ] || die "output directory must be a regular non-symlink directory"
[ -f "$out/.env" ] && [ ! -L "$out/.env" ] || die "output directory is not a provisioned split stack"
[ -f "$out/.manifest" ] && [ ! -L "$out/.manifest" ] || die "output directory has no immutable stack manifest"
[ -f "$password_file" ] && [ ! -L "$password_file" ] || die "password file must be a regular non-symlink file"
[ "$(stat -c '%a' "$password_file")" = 400 ] || die "password file must be mode 0400"
[ "$(wc -c <"$password_file")" -gt 0 ] && [ "$(wc -c <"$password_file")" -le 1024 ] || die "password file must contain a bounded non-empty password"
printf '%s\n' "$username" | grep -Eq '^[a-z0-9._=-]{1,64}$' || die "username must be a safe Matrix localpart"

read_pair() {
  local file=$1 key=$2 value count
  count=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { count++ } END { print count + 0 }' "$file")
  [ "$count" -eq 1 ] || die "$file must contain exactly one $key entry"
  value=$(awk -F= -v wanted="$key" '$0 !~ /^[[:space:]]*#/ && index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$file")
  [ -n "$value" ] || die "$file has an empty $key entry"
  printf '%s' "$value"
}

env_file=$out/.env
manifest=$out/.manifest
stack_name=$(read_pair "$manifest" stack_name)
env_stack=$(read_pair "$env_file" DIREXTALK_SPLIT_STACK_NAME)
[ "$stack_name" = "$env_stack" ] || die ".env stack identity differs from the manifest"
printf '%s\n' "$stack_name" | grep -Eq '^d-[a-z2-7]{26}$' || die "stack identity is not a generated immutable namespace"

command -v docker >/dev/null 2>&1 || die "docker is required"
compose_file=$(cd "$(dirname "$0")/.." && pwd -P)/compose.yaml
local_override=$(cd "$(dirname "$0")/.." && pwd -P)/compose.local.yaml
password_mount=/run/bootstrap/password
output_file=$(mktemp "${TMPDIR:-/tmp}/dirextalk-account-bootstrap.XXXXXX")
chmod 600 "$output_file"
cleanup() {
  rm -f "$output_file"
}
trap cleanup EXIT

compose_args=(
  --project-name "$stack_name"
  --env-file "$env_file"
  -f "$compose_file"
  -f "$local_override"
  run --rm --no-deps
  -v "$password_file:$password_mount:ro"
  --entrypoint /usr/bin/create-account
  message-server
  --config /etc/dirextalk-message-server/message-server.yaml
  --url http://message-server:8008
  --username "$username"
  --passwordfile "$password_mount"
)
init_args=(
  --project-name "$stack_name"
  --env-file "$env_file"
  -f "$compose_file"
  -f "$local_override"
  run --rm --no-deps
  message-server-init
)
if DIREXTALK_LOCAL_BOOTSTRAP_ENABLED=true docker compose "${init_args[@]}" >"$output_file" 2>&1; then
  :
else
  status=$?
  die "local bootstrap initialization failed (status $status); no command output or secret was emitted"
fi
restart_args=(
  --project-name "$stack_name"
  --env-file "$env_file"
  -f "$compose_file"
  -f "$local_override"
  restart message-server
)
if docker compose "${restart_args[@]}" >"$output_file" 2>&1; then
  :
else
  status=$?
  die "message-server restart for local registration failed (status $status)"
fi
healthy=false
for _ in $(seq 1 30); do
  if docker compose --project-name "$stack_name" --env-file "$env_file" \
    -f "$compose_file" -f "$local_override" exec -T message-server \
    wget -q -O - http://127.0.0.1:8008/_p2p/health >/dev/null 2>&1; then
    healthy=true
    break
  fi
  sleep 1
done
[ "$healthy" = true ] || die "message-server did not become healthy after local registration enablement"
if docker compose "${compose_args[@]}" >"$output_file" 2>&1; then
  printf 'local account bootstrap completed for %s (credentials stayed in protected files)\n' "$username"
else
  status=$?
  die "create-account failed (status $status); no command output or access token was emitted"
fi
