#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd -P)
script=$script_dir/update-agent-local.sh
[ -x "$script" ]
bash -n "$script"
grep -Fq 'docker.io/dirextalk/agent:latest' "$script"
grep -Fq '/usr/local/bin/dirextalk-agent' "$script"
grep -Fq '/usr/local/bin/dirextalk-extension-runner' "$script"
grep -Fq '/usr/local/bin/dirextalk-core-runner' "$script"
grep -Fq 'DIREXTALK_AGENT_LOCAL_IMAGE_REF' "$script"
grep -Fq 'docker image tag "$target_image_id" "$image"' "$script"
grep -Fq 'docker image tag "$old_image_id" "$image"' "$script"
grep -Fq 'rollback_agent' "$script"
if grep -Eq 'attestation|RepoDigests|@sha256' "$script"; then
  echo 'superseded Agent image attestation contract remains' >&2
  exit 1
fi
printf 'Agent latest update contract verified\n'
