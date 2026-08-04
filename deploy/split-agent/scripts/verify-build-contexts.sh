#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 AGENT_ROOT MESSAGE_SERVER_ROOT CAPABILITY_API_ROOT" >&2
  exit 2
}

die() {
  echo "build-context gate: $*" >&2
  exit 1
}

[ "$#" -eq 3 ] || usage

required_patterns=(
  '**/*.pem'
  '**/*.key'
  '**/*.token'
  '**/*.secret'
  '**/database-url'
  '**/database_url'
  '**/database-url.txt'
  '**/database_url.txt'
  '**/postgres-password'
  '**/postgres_password'
  '**/service-token'
  '**/service_token'
)

check_root() {
  local label=$1 root=$2 ignore pattern file basename
  [ -d "$root" ] && [ ! -L "$root" ] || die "$label build root must be a regular directory"
  ignore=$root/.dockerignore
  [ -f "$ignore" ] && [ ! -L "$ignore" ] || die "$label build root is missing a regular .dockerignore"
  for pattern in "${required_patterns[@]}"; do
    grep -Fqx "$pattern" "$ignore" || die "$label .dockerignore is missing $pattern"
  done

  while IFS= read -r -d '' file; do
    basename=${file##*/}
    case "$basename" in
      .env|.env.*|*.pem|*.key|*.crt|*.p12|*.pfx|*.token|*.secret|*-token|*-secret|database-url|database_url|database-url.txt|database_url.txt|postgres-password|postgres_password|*_test.go|*.log|*.out|docker-compose*.yml)
        continue
        ;;
    esac
    if grep -Iq . "$file" && grep -Eq 'BEGIN [A-Z ]*PRIVATE KEY|postgres(ql)?://[^[:space:]]+:[^[:space:]@]{16,}@' "$file"; then
      die "$label build context contains an unignored private key or credential URI: $file"
    fi
  done < <(find "$root" -type f \
    ! -path "$root/.git/*" \
    ! -path "$root/.run/*" \
    ! -path "$root/.codex/*" \
    ! -path "$root/deploy/*" \
    -print0)
}

check_root Agent "$1"
check_root message-server "$2"
check_root capability-api "$3"
printf 'build-context secret exclusion checks passed\n'
