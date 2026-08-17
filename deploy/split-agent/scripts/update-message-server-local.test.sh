#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd -P)
script=$script_dir/update-message-server-local.sh
[ -x "$script" ]
bash -n "$script"
if grep -Eq -- '-v[[:space:]]+index=' "$script"; then
  echo 'awk builtin index is used as a variable name' >&2
  exit 1
fi
grep -Fq 'target_image="docker.io/dirextalk/message-server:$target_version"' "$script"
grep -Fq '/usr/bin/dirextalk-message-server --version' "$script"
grep -Fq 'DIREXTALK_MESSAGE_SERVER_LOCAL_IMAGE_REF' "$script"
grep -Fq 'docker image tag "$target_image_id" "$target_image"' "$script"
grep -Fq 'DIREXTALK_MESSAGE_SERVER_IMAGE="$image"' "$script"
grep -Fq 'rollback_message_server' "$script"
if grep -Eq 'attestation|RepoDigests|@sha256' "$script"; then
  echo 'superseded message-server image attestation contract remains' >&2
  exit 1
fi
printf 'message-server version-tag update contract verified\n'
