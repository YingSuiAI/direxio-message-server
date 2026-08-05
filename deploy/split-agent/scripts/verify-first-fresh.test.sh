#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd -P)
source_wrapper=$script_dir/verify-first-fresh.sh
[ -x "$source_wrapper" ] || { echo "verify-first-fresh.sh must be executable" >&2; exit 1; }

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/dirextalk-first-fresh.XXXXXX")
cleanup() {
  rm -rf -- "$tmp_dir"
}
trap cleanup EXIT

fixture=$tmp_dir/scripts
mkdir -p -- "$fixture"
cp -- "$source_wrapper" "$fixture/verify-first-fresh.sh"
chmod 755 "$fixture/verify-first-fresh.sh"

for helper in provision-local.sh start-local.sh accept-local.sh; do
  cat >"$fixture/$helper" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "${0##*/}" >>"${FIRST_FRESH_TEST_LOG:?}"
if [ -n "${FIRST_FRESH_TEST_MODE_LOG:-}" ]; then
  printf '%s|%s|%s\n' "${0##*/}" "${DIREXTALK_SPLIT_COMPOSE_MODE:-}" "${DIREXTALK_ACCEPTANCE_COMPOSE_MODE:-}" >>"$FIRST_FRESH_TEST_MODE_LOG"
fi
case "${0##*/}" in
  provision-local.sh)
    mkdir -p -- "$1"
    : >"$1/.env"
    ;;
  start-local.sh)
    [ "${FIRST_FRESH_FAIL_START:-false}" != true ] || exit 125
    ;;
esac
EOF
  chmod 755 "$fixture/$helper"
done

run_dir=$tmp_dir/run
log=$tmp_dir/log
secret=$tmp_dir/secret
: >"$secret"

set +e
FIRST_FRESH_TEST_LOG=$log "$fixture/verify-first-fresh.sh" \
  --execute-first-fresh "$run_dir" "$secret" "$secret" "$secret" "$secret" chat embed \
  >"$tmp_dir/unauthorized.stdout" 2>"$tmp_dir/unauthorized.stderr"
status=$?
set -e
[ "$status" -eq 1 ] || { echo "unauthorized path status=$status, want 1" >&2; exit 1; }
[ ! -e "$log" ] || { echo "unauthorized path invoked a helper" >&2; exit 1; }
grep -Fq 'DIREXTALK_FIRST_FRESH_AUTHORIZED=true' "$tmp_dir/unauthorized.stderr"

set +e
DIREXTALK_FIRST_FRESH_AUTHORIZED=true DIREXTALK_SPLIT_FIXTURE_MODE=true FIRST_FRESH_TEST_LOG=$log \
  "$fixture/verify-first-fresh.sh" \
  --execute-first-fresh "$run_dir" "$secret" "$secret" "$secret" "$secret" chat embed \
  >"$tmp_dir/fixture.stdout" 2>"$tmp_dir/fixture.stderr"
status=$?
set -e
[ "$status" -eq 1 ] || { echo "fixture path status=$status, want 1" >&2; exit 1; }
[ ! -e "$log" ] || { echo "fixture path invoked a helper" >&2; exit 1; }
grep -Fq 'fixture mode is forbidden' "$tmp_dir/fixture.stderr"

DIREXTALK_FIRST_FRESH_AUTHORIZED=true FIRST_FRESH_TEST_LOG=$log \
  "$fixture/verify-first-fresh.sh" \
  --execute-first-fresh "$run_dir" "$secret" "$secret" "$secret" "$secret" chat embed \
  >"$tmp_dir/success.stdout" 2>"$tmp_dir/success.stderr"
diff -u <(printf '%s\n' provision-local.sh start-local.sh accept-local.sh) "$log"
grep -Fq 'first-fresh consumer gate passed:' "$tmp_dir/success.stdout"

rm -rf -- "$run_dir"
: >"$log"
mode_log=$tmp_dir/mode.log
DIREXTALK_FIRST_FRESH_AUTHORIZED=true \
  DIREXTALK_FIRST_FRESH_COMPOSE_MODE=production \
  DIREXTALK_CORE_EXTENSION_ENABLED=true \
  DIREXTALK_CORE_WORKLOAD_ENABLED=true \
  FIRST_FRESH_TEST_LOG=$log FIRST_FRESH_TEST_MODE_LOG=$mode_log \
  "$fixture/verify-first-fresh.sh" \
  --execute-first-fresh "$run_dir" "$secret" "$secret" "$secret" "$secret" chat embed \
  >"$tmp_dir/production.stdout" 2>"$tmp_dir/production.stderr"
diff -u <(printf '%s\n' \
  'provision-local.sh|production|' \
  'start-local.sh|production|' \
  'accept-local.sh||production') "$mode_log"
grep -Fq 'first-fresh consumer gate passed:' "$tmp_dir/production.stdout"

rm -rf -- "$run_dir"
: >"$log"
set +e
DIREXTALK_FIRST_FRESH_AUTHORIZED=true FIRST_FRESH_FAIL_START=true FIRST_FRESH_TEST_LOG=$log \
  "$fixture/verify-first-fresh.sh" \
  --execute-first-fresh "$run_dir" "$secret" "$secret" "$secret" "$secret" chat embed \
  >"$tmp_dir/infrastructure.stdout" 2>"$tmp_dir/infrastructure.stderr"
status=$?
set -e
[ "$status" -eq 125 ] || { echo "infrastructure path status=$status, want 125" >&2; exit 1; }
diff -u <(printf '%s\n' provision-local.sh start-local.sh) "$log"
if grep -Fq 'first-fresh consumer gate passed:' "$tmp_dir/infrastructure.stdout"; then
  echo "infrastructure failure printed a success marker" >&2
  exit 1
fi

echo "verify-first-fresh wrapper tests passed"
