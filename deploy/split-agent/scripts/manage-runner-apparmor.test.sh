#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
script=$script_dir/manage-runner-apparmor.sh
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT
mkdir -p "$tmp_dir/bin" "$tmp_dir/etc/apparmor.d" "$tmp_dir/sys"
: >"$tmp_dir/sys/profiles"

cat >"$tmp_dir/bin/apparmor_parser" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DIREXTALK_FAKE_STATE/parser.log"
[ "${DIREXTALK_FAKE_PARSER_FAIL:-false}" != true ] || exit 42
case "$1" in
  --replace)
    printf 'dirextalk-runner-userns (unconfined)\n' >"$DIREXTALK_APPARMOR_LOADED_PROFILES"
    ;;
  --remove)
    : >"$DIREXTALK_APPARMOR_LOADED_PROFILES"
    ;;
  *) exit 43 ;;
esac
EOF

cat >"$tmp_dir/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[ "${DIREXTALK_FAKE_DOCKER_FAIL:-false}" != true ] || exit 44
case "$1 $2 $3" in
  'ps --all --quiet') printf '%s\n' "${DIREXTALK_FAKE_CONTAINER_ID:-}" ;;
  inspect\ --format\ *) printf '%s\n' "${DIREXTALK_FAKE_CONTAINER_PROFILE:-docker-default}" ;;
  *) exit 45 ;;
esac
EOF
chmod +x "$tmp_dir/bin/apparmor_parser" "$tmp_dir/bin/docker"

run_manager() {
  env \
    PATH="$tmp_dir/bin:/usr/bin:/bin" \
    DIREXTALK_SPLIT_TEST_MODE=true \
    DIREXTALK_APPARMOR_TARGET_DIR="$tmp_dir/etc/apparmor.d" \
    DIREXTALK_APPARMOR_LOADED_PROFILES="$tmp_dir/sys/profiles" \
    DIREXTALK_FAKE_STATE="$tmp_dir" \
    "$script" "$@"
}

run_manager install >"$tmp_dir/install.stdout"
grep -Fqx 'profile=dirextalk-runner-userns' "$tmp_dir/install.stdout"
cmp -s "$script_dir/../apparmor.d/dirextalk-runner-userns" "$tmp_dir/etc/apparmor.d/dirextalk-runner-userns"
[ "$(stat -c '%a' "$tmp_dir/etc/apparmor.d/dirextalk-runner-userns")" = 644 ]
grep -Fqx 'dirextalk-runner-userns (unconfined)' "$tmp_dir/sys/profiles"
run_manager verify >"$tmp_dir/verify.stdout"
grep -Fqx 'verified_profile=dirextalk-runner-userns' "$tmp_dir/verify.stdout"

cp "$tmp_dir/etc/apparmor.d/dirextalk-runner-userns" "$tmp_dir/tampered"
printf '\n# tampered\n' >>"$tmp_dir/etc/apparmor.d/dirextalk-runner-userns"
if run_manager install >"$tmp_dir/tamper.stdout" 2>"$tmp_dir/tamper.stderr"; then
  echo 'tampered installed profile was accepted' >&2
  exit 1
fi
grep -Fq 'installed same-name profile differs' "$tmp_dir/tamper.stderr"
mv "$tmp_dir/tampered" "$tmp_dir/etc/apparmor.d/dirextalk-runner-userns"

if DIREXTALK_FAKE_CONTAINER_ID=runner-1 DIREXTALK_FAKE_CONTAINER_PROFILE=dirextalk-runner-userns \
  run_manager remove >"$tmp_dir/in-use.stdout" 2>"$tmp_dir/in-use.stderr"; then
  echo 'in-use profile was removed' >&2
  exit 1
else
  status=$?
fi
[ "$status" -eq 3 ]
grep -Fq 'expected negative: profile is still referenced' "$tmp_dir/in-use.stderr"
[ -f "$tmp_dir/etc/apparmor.d/dirextalk-runner-userns" ]

if DIREXTALK_FAKE_DOCKER_FAIL=true run_manager remove >"$tmp_dir/docker-fail.stdout" 2>"$tmp_dir/docker-fail.stderr"; then
  echo 'Docker infrastructure failure was accepted' >&2
  exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
grep -Fq 'Docker container inventory failed' "$tmp_dir/docker-fail.stderr"

run_manager remove >"$tmp_dir/remove.stdout"
grep -Fqx 'removed_profile=dirextalk-runner-userns' "$tmp_dir/remove.stdout"
[ ! -e "$tmp_dir/etc/apparmor.d/dirextalk-runner-userns" ]
[ ! -s "$tmp_dir/sys/profiles" ]

if run_manager remove >"$tmp_dir/absent.stdout" 2>"$tmp_dir/absent.stderr"; then
  echo 'already absent profile did not return expected negative' >&2
  exit 1
else
  status=$?
fi
[ "$status" -eq 3 ]
grep -Fq 'expected negative: profile is already absent' "$tmp_dir/absent.stderr"

if DIREXTALK_FAKE_PARSER_FAIL=true run_manager install >"$tmp_dir/parser-fail.stdout" 2>"$tmp_dir/parser-fail.stderr"; then
  echo 'parser infrastructure failure was accepted' >&2
  exit 1
else
  status=$?
fi
[ "$status" -eq 1 ]
grep -Fq 'profile parser/load failed' "$tmp_dir/parser-fail.stderr"

echo 'runner AppArmor management tests passed'
