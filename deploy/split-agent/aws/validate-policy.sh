#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 POLICY_JSON [CLOUDFORMATION_SERVICE_ROLE_ARN]" >&2
  exit 2
}

die() {
  echo "AWS policy gate: $*" >&2
  exit 1
}

[ "$#" -ge 1 ] && [ "$#" -le 2 ] || usage
policy=$1
[ -f "$policy" ] && [ ! -L "$policy" ] || die "policy must be a regular non-symlink file"
command -v jq >/dev/null 2>&1 || die "jq is required"
expected_cloudformation_role=${2:-}
jq -e 'type == "object" and .Version == "2012-10-17" and (.Statement | type == "array" and length > 0)' "$policy" >/dev/null || die "policy envelope is invalid"
jq -e 'all(.Statement[]; (.Effect == "Allow" or .Effect == "Deny") and (.Action != null) and (.Resource != null) and (.NotAction == null) and (.NotResource == null) and (.Principal == null))' "$policy" >/dev/null || die "policy contains an unsafe statement shape"

role_arn_re='^arn:aws:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_-]+$'
if [ -n "$expected_cloudformation_role" ] && [[ ! "$expected_cloudformation_role" =~ $role_arn_re ]]; then
  die "configured CloudFormation service role must be one exact non-wildcard role ARN"
fi

# Resource * is accepted only for AWS APIs that cannot be resource-scoped:
# caller identity and the read-only SSM inventory probe. Every mutable service
# action must be constrained to a concrete ARN in the disposable account; the
# runtime role is never an administrator or a credential issuer.
while IFS= read -r action; do
  case "$action" in
    sts:GetCallerIdentity|ssm:GetCommandInvocation|ssm:ListCommandInvocations|ssm:SendCommand|ssm:DescribeInstanceInformation|ecs:DescribeClusters|ecs:DescribeServices|ecs:DescribeTaskDefinition|ecs:ListTasks|ecs:RunTask|ecs:StopTask|ecs:UpdateService|iam:PassRole|secretsmanager:GetSecretValue)
      ;;
    *) die "action is outside the approved runtime allowlist: $action" ;;
  esac
done < <(jq -r '.Statement[] | .Action | if type == "array" then .[] else . end' "$policy")

jq -e '
  all(.Statement[];
    ((.Action | if type == "array" then . else [.] end) as $actions |
      (.Resource | if type == "array" then . else [.] end) as $resources |
      all($resources[]; . != "*" or ($actions | all(. == "sts:GetCallerIdentity" or . == "ssm:DescribeInstanceInformation"))) and
      all($actions[]; (contains("*") | not))
    )
  )
' "$policy" >/dev/null || die "wildcard resources/actions are not allowed for mutable AWS capabilities"

passrole_check=$(jq -r '
  [.Statement[]
   | select((.Action | if type == "array" then . else [.] end) | index("iam:PassRole") != null)
   | {
       service: (.Condition.StringEquals["iam:PassedToService"] // ""),
       resources: (.Resource | if type == "array" then . else [.] end)
     }]
' "$policy")
jq -e '
  all(.[];
    (.service == "ecs-tasks.amazonaws.com" or .service == "cloudformation.amazonaws.com") and
    (.resources | length > 0 and all(.[]; test("^arn:aws:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_-]+$")))
  )
' <<<"$passrole_check" >/dev/null || die "iam:PassRole must be constrained to exact ECS or CloudFormation role ARNs"

# Execution V2 always passes the configured dedicated CloudFormation role to
# AWS. Keep the policy fail-closed if that allowlisted role statement is
# absent, malformed, or does not match the configured value supplied by the
# deployment runbook.
jq -e '
  any(.[]; .service == "cloudformation.amazonaws.com")
' <<<"$passrole_check" >/dev/null || die "iam:PassRole must include an exact CloudFormation service role ARN"
if [ -n "$expected_cloudformation_role" ]; then
  jq -e --arg expected "$expected_cloudformation_role" '
    any(.[]; .service == "cloudformation.amazonaws.com" and (.resources | index($expected) != null))
  ' <<<"$passrole_check" >/dev/null || die "configured CloudFormation service role is not allowlisted by iam:PassRole"
fi

if grep -Eiq 'AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN|BEGIN .*PRIVATE KEY|AKIA[0-9A-Z]{16}' "$policy"; then
  die "policy file contains credential material"
fi

printf 'least-privilege AWS runtime policy checks passed\n'
