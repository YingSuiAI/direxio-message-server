#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd -P)
script=$script_dir/update-message-server-local.sh
[ -x "$script" ]
bash -n "$script"
grep -Fq 'docker.io/dirextalk/message-server:latest' "$script"
grep -Fq '/usr/bin/dirextalk-message-server --version' "$script"
if grep -Eq 'attestation|RepoDigests|@sha256' "$script"; then
  echo 'superseded message-server image attestation contract remains' >&2
  exit 1
fi
printf 'message-server latest update contract verified\n'
