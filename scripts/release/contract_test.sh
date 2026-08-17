#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
prepare="$repo_root/scripts/release/prepare.sh"
verify="$repo_root/scripts/release/verify.sh"
publish="$repo_root/scripts/release/publish.sh"
lib="$repo_root/scripts/release/lib.sh"
version=v1.0.0
head_commit=1111111111111111111111111111111111111111
other_commit=2222222222222222222222222222222222222222

fail() {
  printf 'release contract test failed: %s\n' "$*" >&2
  exit 1
}

for script in "$lib" "$prepare" "$verify" "$publish"; do
  [[ -x "$script" ]] || fail "missing executable ${script#"$repo_root"/}"
done

grep -F 'dirextalk-message-server-release' "$repo_root/AGENTS.md" >/dev/null || \
  fail 'AGENTS does not route stable releases to the release Skill'
grep -F 'description: Stable vX.Y.Z version on main' "$repo_root/.github/workflows/release.yml" >/dev/null || \
  fail 'release input is not limited to the main stable version'
grep -F 'uses: docker/setup-buildx-action@v3' "$repo_root/.github/workflows/release.yml" >/dev/null || \
  fail 'release workflow does not install Docker Buildx'
if grep -Eq 'integration-publish|agent-core-v1|dirextalk/agent' "$repo_root/.github/workflows/release.yml"; then
  fail 'release workflow still contains the retired integration Agent publisher'
fi
for retired in agent-lib.sh agent-release.test.sh build-agent-v1.sh oci-lib.sh \
  prepare-agent.sh publish-agent.sh verify-agent.sh; do
  [[ ! -e "$repo_root/scripts/release/$retired" ]] || fail "retired release script remains: $retired"
done
if grep -Ein 'attestat|sbom|provenance|imagetools inspect|index digest|latest.*monotonic|race' \
  "$lib" "$prepare" "$verify" "$publish"; then
  fail 'active release scripts still contain retired publication gates'
fi
required_test_command='go test ./internal/releasecontrol ./internal/httputil ./setup ./p2p ./p2p/internal/agent ./p2p/serviceapi ./internal/productpolicy -count=1'
for test_entrypoint in "$repo_root/.github/workflows/ci.yml" "$verify"; do
  grep -F "$required_test_command" "$test_entrypoint" >/dev/null || \
    fail "${test_entrypoint#"$repo_root"/} does not run Agent implementation and service API contract tests"
done

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

install_fake_tools() {
  local fixture=$1
  cat >"$fixture/bin/git" <<EOF
#!/usr/bin/env bash
set -euo pipefail
printf 'git %s\n' "\$*" >>"\$RELEASE_TEST_LOG"
case "\${1:-} \${2:-}" in
  'status --porcelain') printf '%s' "\${FAKE_GIT_DIRTY:-}" ;;
  'branch --show-current') printf '%s\n' "\${FAKE_GIT_BRANCH:-main}" ;;
  'rev-parse HEAD') printf '%s\n' "\${FAKE_GIT_HEAD:-$head_commit}" ;;
  'rev-list -n') printf '%s\n' "\${FAKE_LOCAL_TAG_COMMIT:-\${FAKE_GIT_HEAD:-$head_commit}}" ;;
  'show -s') printf '%s\n' '2026-08-12T08:00:00+08:00' ;;
  'tag --list')
    [[ ! -f "\$RELEASE_TEST_GIT_STATE.local-tag" ]] || printf '%s\n' "$version"
    ;;
  'tag -a') : >"\$RELEASE_TEST_GIT_STATE.local-tag" ;;
  'push origin') : >"\$RELEASE_TEST_GIT_STATE.remote-tag" ;;
  'ls-remote --exit-code')
    if [[ "\${3:-}" == --tags ]]; then
      ref="\${*: -1}"
      if [[ "\$ref" == *'^{}' ]]; then
        [[ -f "\$RELEASE_TEST_GIT_STATE.remote-tag" || "\${FAKE_REMOTE_TAG_EXISTS:-0}" == 1 ]] || exit 2
        printf '%s\t%s\n' "\${FAKE_REMOTE_TAG_COMMIT:-\${FAKE_GIT_HEAD:-$head_commit}}" "\$ref"
      else
        [[ -f "\$RELEASE_TEST_GIT_STATE.remote-tag" || "\${FAKE_REMOTE_TAG_EXISTS:-0}" == 1 ]] || exit 2
        printf '%s\t%s\n' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa "\$ref"
      fi
    else
      printf '%s\trefs/heads/main\n' "\${FAKE_REMOTE_HEAD:-\${FAKE_GIT_HEAD:-$head_commit}}"
    fi
    ;;
  'ls-remote --tags')
    ref="\${*: -1}"
    if [[ "\$ref" == *'^{}' ]]; then
      [[ -f "\$RELEASE_TEST_GIT_STATE.remote-tag" || "\${FAKE_REMOTE_TAG_EXISTS:-0}" == 1 ]] || exit 2
      printf '%s\t%s\n' "\${FAKE_REMOTE_TAG_COMMIT:-\${FAKE_GIT_HEAD:-$head_commit}}" "\$ref"
    else
      [[ -f "\$RELEASE_TEST_GIT_STATE.remote-tag" || "\${FAKE_REMOTE_TAG_EXISTS:-0}" == 1 ]] || exit 2
      printf '%s\t%s\n' aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa "\$ref"
    fi
    ;;
  *) exit 2 ;;
esac
EOF

  cat >"$fixture/bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'go %s\n' "$*" >>"$RELEASE_TEST_LOG"
[[ -z "${FAKE_GO_FAIL_PATTERN:-}" || "$*" != *"$FAKE_GO_FAIL_PATTERN"* ]]
EOF

  cat >"$fixture/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'docker %s\n' "$*" >>"$RELEASE_TEST_LOG"
if [[ -n "${FAKE_DOCKER_FAIL_PATTERN:-}" && "$*" == *"$FAKE_DOCKER_FAIL_PATTERN"* ]]; then
  exit 1
fi
case "${1:-} ${2:-}" in
  'image inspect')
    ref=${3:-}
    revision=$RELEASE_COMMIT
    [[ "$ref" != 'dirextalk/message-server:latest' ]] || revision=${FAKE_LATEST_PULL_REVISION:-$revision}
    [[ "$ref" == 'dirextalk/message-server:latest' ]] || revision=${FAKE_VERSION_PULL_REVISION:-$revision}
    if [[ "$*" == *'org.opencontainers.image.created'* ]]; then
      printf '%s\n' "$RELEASE_VERSION|$revision|$RELEASE_BUILD_TIME"
    else
      printf '%s\n' "$RELEASE_VERSION|$revision"
    fi
    ;;
  'compose -f') ;;
  'buildx build') : >"$RELEASE_TEST_DOCKER_STATE.version-pushed" ;;
  'buildx imagetools')
    [[ "${3:-}" == create ]] || exit 2
    : >"$RELEASE_TEST_DOCKER_STATE.latest-moved"
    ;;
  'pull --platform') printf '%s\n' "${4:-}" >"$RELEASE_TEST_DOCKER_STATE/last-pulled" ;;
  *)
    if [[ "${1:-}" == build ]]; then
      :
    elif [[ "${1:-}" == run ]]; then
      ref=
      for argument in "$@"; do
        [[ "$argument" != dirextalk/message-server:* ]] || ref=$argument
      done
      output=$RELEASE_VERSION
      if [[ "$ref" == 'dirextalk/message-server:latest' ]]; then
        output=${FAKE_LATEST_PULL_VERSION:-$output}
      else
        output=${FAKE_VERSION_PULL_VERSION:-$output}
      fi
      printf '%s\n' "$output"
    else
      exit 2
    fi
    ;;
esac
EOF

  cat >"$fixture/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'gh %s\n' "$*" >>"$RELEASE_TEST_LOG"
case "${1:-} ${2:-}" in
  'release view') [[ -f "$RELEASE_TEST_GH_STATE.release" ]] ;;
  'release create')
    [[ "${FAKE_GH_CREATE_FAIL:-0}" != 1 ]] || exit 1
    : >"$RELEASE_TEST_GH_STATE.release"
    ;;
  *) exit 2 ;;
esac
EOF
  chmod +x "$fixture/bin/"*
}

make_fixture() {
  local name=$1 fixture
  fixture=$tmp/$name
  mkdir -p "$fixture/bin" "$fixture/repo/release" "$fixture/repo/internal" \
    "$fixture/docker-state" "$fixture/gh-state" "$fixture/git-state"
  cp "$lib" "$prepare" "$verify" "$publish" "$fixture/repo/"
  printf '# Stable releases\n\n## %s\n\nStable release.\n' "$version" \
    >"$fixture/repo/release/RELEASE_NOTES.md"
  cat >"$fixture/repo/release/$version.json" <<EOF
{"version":"$version","minimum_client_version":"v1.0.0","maximum_client_version_exclusive":"v2.0.0","schema_version":2,"schema_compat_version":1}
EOF
  cat >"$fixture/repo/internal/version.go" <<EOF
const (
  SchemaVersion = 2
  SchemaCompatVersion = 1
)
version = "$version"
EOF
  printf 'module example.test/release-fixture\n' >"$fixture/repo/go.mod"
  : >"$fixture/repo/docker-compose.p2p.yml"
  : >"$fixture/commands.log"
  install_fake_tools "$fixture"
  printf '%s\n' "$fixture"
}

run_stage() {
  local fixture=$1 script=$2
  shift 2
  (
    cd "$fixture/repo"
    env PATH="$fixture/bin:$PATH" \
      RELEASE_REPO_ROOT="$fixture/repo" \
      RELEASE_OUTPUT_DIR="$fixture/out" \
      RELEASE_TEST_LOG="$fixture/commands.log" \
      RELEASE_TEST_DOCKER_STATE="$fixture/docker-state" \
      RELEASE_TEST_GH_STATE="$fixture/gh-state" \
      RELEASE_TEST_GIT_STATE="$fixture/git-state" \
      RELEASE_CONTRACT_TEST=1 \
      "$@" "$fixture/repo/$script" "$version"
  )
}

fixture=$(make_fixture dirty)
if run_stage "$fixture" prepare.sh FAKE_GIT_DIRTY=' M internal/version.go'; then
  fail 'prepare accepted a dirty tree'
fi

fixture=$(make_fixture branch)
if run_stage "$fixture" prepare.sh FAKE_GIT_BRANCH=feature; then
  fail 'prepare accepted a non-main branch'
fi

fixture=$(make_fixture remote)
if run_stage "$fixture" prepare.sh FAKE_REMOTE_HEAD=$other_commit; then
  fail 'prepare accepted a main commit not pushed to origin'
fi

fixture=$(make_fixture version)
sed -i 's/version = "v1.0.0"/version = "v9.9.9"/' "$fixture/repo/internal/version.go"
if run_stage "$fixture" prepare.sh; then
  fail 'prepare accepted a mismatched source version'
fi

fixture=$(make_fixture success)
run_stage "$fixture" prepare.sh
run_stage "$fixture" verify.sh
run_stage "$fixture" publish.sh
grep -F 'docker buildx build --pull --platform linux/amd64 --push' "$fixture/commands.log" >/dev/null || \
  fail 'version image was not pushed with Buildx'
grep -F 'docker pull --platform linux/amd64 dirextalk/message-server:v1.0.0' "$fixture/commands.log" >/dev/null || \
  fail 'published version image was not pulled back'
grep -F 'gh release create v1.0.0' "$fixture/commands.log" >/dev/null || \
  fail 'matching GitHub Release was not created'
grep -F 'docker buildx imagetools create --tag dirextalk/message-server:latest dirextalk/message-server:v1.0.0' "$fixture/commands.log" >/dev/null || \
  fail 'latest was not updated from the version tag'
grep -F 'docker pull --platform linux/amd64 dirextalk/message-server:latest' "$fixture/commands.log" >/dev/null || \
  fail 'latest was not pulled back'

version_pull=$(grep -nF 'docker pull --platform linux/amd64 dirextalk/message-server:v1.0.0' "$fixture/commands.log" | cut -d: -f1)
release_create=$(grep -nF 'gh release create v1.0.0' "$fixture/commands.log" | cut -d: -f1)
latest_move=$(grep -nF 'docker buildx imagetools create --tag dirextalk/message-server:latest' "$fixture/commands.log" | cut -d: -f1)
latest_pull=$(grep -nF 'docker pull --platform linux/amd64 dirextalk/message-server:latest' "$fixture/commands.log" | cut -d: -f1)
(( version_pull < release_create && release_create < latest_move && latest_move < latest_pull )) || \
  fail 'publication order is not version probe -> GitHub Release -> latest -> latest probe'

fixture=$(make_fixture version-probe)
run_stage "$fixture" prepare.sh
run_stage "$fixture" verify.sh
if run_stage "$fixture" publish.sh FAKE_VERSION_PULL_VERSION=v9.9.9; then
  fail 'publish accepted a pulled version image with a mismatched binary version'
fi
[[ ! -f "$fixture/gh-state.release" ]] || \
  fail 'release or latest moved after the version image probe failed'
if grep -F 'docker buildx imagetools create --tag dirextalk/message-server:latest' "$fixture/commands.log" >/dev/null; then
  fail 'latest moved after the version image probe failed'
fi

fixture=$(make_fixture github-failure)
run_stage "$fixture" prepare.sh
run_stage "$fixture" verify.sh
if run_stage "$fixture" publish.sh FAKE_GH_CREATE_FAIL=1; then
  fail 'publish succeeded after GitHub Release creation failed'
fi
if grep -F 'docker buildx imagetools create --tag dirextalk/message-server:latest' "$fixture/commands.log" >/dev/null; then
  fail 'latest moved before GitHub Release creation succeeded'
fi

fixture=$(make_fixture latest-probe)
run_stage "$fixture" prepare.sh
run_stage "$fixture" verify.sh
if run_stage "$fixture" publish.sh FAKE_LATEST_PULL_REVISION=$other_commit; then
  fail 'publish accepted latest with a mismatched revision label'
fi
grep -F 'docker buildx imagetools create --tag dirextalk/message-server:latest' "$fixture/commands.log" >/dev/null || \
  fail 'latest probe did not run after latest moved'

fixture=$(make_fixture tag-conflict)
run_stage "$fixture" prepare.sh
run_stage "$fixture" verify.sh
if run_stage "$fixture" publish.sh FAKE_REMOTE_TAG_COMMIT=$other_commit; then
  fail 'publish accepted a remote release tag bound to another commit'
fi

printf 'release contract tests passed\n'
