# AWS acceptance lane (least privilege, explicit opt-in)

This directory is a reviewable policy and runbook only. It never invokes the
AWS CLI, creates an account resource, or accepts ambient credentials. The
Compose harness is not an AWS authorization boundary.

## Required controls

1. Use a disposable AWS account and a dedicated region. Record the account ID,
   region, exact EC2 instance ID or ECS cluster/service/task definition, and the
   source/image revision in the acceptance ticket.
2. Use a short-lived STS session obtained through an already-approved deployer
   role. Do not use root credentials, long-lived access keys, instance metadata
   credentials, or credentials copied into `.env`, YAML, Docker build contexts,
   logs, or command arguments.
3. Attach the smallest policy that matches the exact target ARNs. Start from
   [`agent-runtime-policy.json`](./agent-runtime-policy.json), replace its
   disposable example ARNs with the real exact ARNs, then run:

   ```sh
   deploy/split-agent/aws/validate-policy.sh \
     deploy/split-agent/aws/agent-runtime-policy.json \
     arn:aws:iam::123456789012:role/dirextalk-acceptance-cfn
   ```

   `Resource: "*"` is reserved for `sts:GetCallerIdentity` (and any AWS API
   that AWS documents as requiring a resource wildcard, only after review).
   No wildcard action, `iam:*`, user/key creation, organization/account
   mutation, or unconstrained `iam:PassRole` is permitted. The validator
   accepts ECS task-role delegation only with its exact service condition and
   requires a separate exact CloudFormation service-role delegation constrained by
   `iam:PassedToService=cloudformation.amazonaws.com`; omitting the latter
   fails closed for Execution V2. The optional second argument must exactly
   match the non-secret `core_aws_cloudformation_service_role_arn` configured
   in the Agent, so a caller-role fallback can never be used.
4. Store the AWS access key, secret, and optional session token through the
   Agent's durable AWS credential workflow. The generated Agent config contains
   only a UUID credential reference and immutable target/readiness facts. The
   runtime resolves the current verified revision from the Agent database and
   performs STS/account/target readback before advertising an AWS capability.
5. Provision with `DIREXTALK_CORE_AWS_ENABLED=true` only after the policy and
   target are approved. The SSM readiness variables are non-secret identity
   facts:

   ```sh
   DIREXTALK_CORE_AWS_ENABLED=true \
   DIREXTALK_CORE_AWS_SSM_CREDENTIAL_REFERENCE=11111111-1111-4111-8111-111111111111 \
   DIREXTALK_CORE_AWS_SSM_REGION=us-east-1 \
   DIREXTALK_CORE_AWS_SSM_ACCOUNT_ID=123456789012 \
   DIREXTALK_CORE_AWS_SSM_INSTANCE_ID=i-0123456789abcdef0 \
   deploy/split-agent/scripts/provision-local.sh /absolute/path/.run/split
   ```

   The UUID must be the exact durable credential row; it is not a secret and
   must never be replaced with key material. ECS readiness is intentionally
   opt-in through the Agent's typed config/API after its exact cluster,
   service, subnet, security-group, task-family, and role ARNs are approved.

## Acceptance sequence

1. Run the local split topology and model/Knowledge/memory checks with AWS
   disabled. Verify the Agent advertises no AWS workload route.
2. Apply the reviewed policy to the disposable runtime role out of band, then
   insert/verify the credential reference through the authenticated Agent
   workflow. Confirm STS account and target identity readback in the Agent
   health/capability projection before sending a workload operation.
3. Exercise one bounded SSM operation against the pinned instance. Require an
   explicit user confirmation, inspect operation events and actual state, then
   exercise the exact destroy/reconcile path. Treat `uncertain` or missing
   readback as a failed acceptance, never as success.
4. If Execution V2 is enabled, exercise one fixed CloudFormation reservation
   with the same configured `core_aws_cloudformation_service_role_arn`. Verify
   the change-set request contains that exact RoleARN and that every returned
   stack/resource identifier is read back before accepting the operation. A
   missing or mismatched role ARN must be rejected before any AWS call.
5. Revoke the STS session, remove the disposable role/policy and target
   resources, and run the exact split-stack cleanup script. Keep the manifest,
   policy digest, image attestation, and verification output as audit evidence;
   do not delete the manifest before the review closes.

No AWS call is considered tested by a Compose render or a unit fixture. A live
AWS claim requires the disposable-account evidence above and the actual Agent
consumer path.
