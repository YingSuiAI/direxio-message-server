#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
"$script_dir/validate-policy.sh" "$script_dir/agent-runtime-policy.json" >/dev/null

tmp_policy=$(mktemp "${TMPDIR:-/tmp}/dirextalk-aws-policy.XXXXXX.json")
tmp_policy_without_cfn=$(mktemp "${TMPDIR:-/tmp}/dirextalk-aws-policy-no-cfn.XXXXXX.json")
cleanup() {
  rm -f "$tmp_policy" "$tmp_policy_without_cfn"
}
trap cleanup EXIT
printf '%s\n' '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"iam:*","Resource":"*"}]}' >"$tmp_policy"
if "$script_dir/validate-policy.sh" "$tmp_policy" >/dev/null 2>&1; then
  echo "unsafe wildcard policy unexpectedly accepted" >&2
  exit 1
fi
jq '.Statement |= map(select(.Sid != "PinnedCloudFormationServiceRoleDelegation"))' \
  "$script_dir/agent-runtime-policy.json" >"$tmp_policy_without_cfn"
if "$script_dir/validate-policy.sh" "$tmp_policy_without_cfn" >/dev/null 2>&1; then
  echo "policy without the exact CloudFormation service role unexpectedly accepted" >&2
  exit 1
fi
printf 'least-privilege AWS policy checks verified\n'
