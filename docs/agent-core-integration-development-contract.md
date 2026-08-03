# Embedded Agent Control and Execution V2 Contract

Status: authoritative target since 2026-07-31

This document is the server contract for embedded Agent control and Execution
Orchestration V2. The Message Server is the control plane. It analyzes,
compiles, authorizes, coordinates, observes and records remote work, but never
executes third-party shell, project code or third-party Skills on its own host.

The normative architecture decision is
[`adr/2026-07-31-execution-orchestration-v2.md`](adr/2026-07-31-execution-orchestration-v2.md).

## 1. Topology and ownership

- `dirextalk-message-server` is the only persistent Dirextalk application
  process and the only application image.
- PostgreSQL is the durable authority for Agent tasks, confirmations, plans,
  runs, stages, receipts, observations, artifacts and service bindings.
- Every Agent and execution record is owner-scoped. Public operations first
  authenticate the normal owner session.
- Online Agent remains the private Matrix Agent-room conversation. Native
  Agent remains `client.native_agent_stream`.
- There is no Agent Core sidecar, address, token, CA, port, gRPC fallback,
  Docker socket or local Core Runner.
- The server never publishes secret plaintext through ProductCore, realtime
  events, logs, plans, receipts, artifacts or deployment projections.

## 2. Pure V2 baseline

The former workload/GeoLibre deployment implementation was never deployed and
has no historical user data. It is deleted rather than supported as a legacy
surface:

- no V1 workload, EC2-provision or GeoLibre product action;
- no V1 workload/provider/task/confirmation execution branch;
- no V1 deployment ledger, DTO, route, page or recovery state;
- no compatibility read, observe, reconcile, retry or destroy path;
- no V1-to-V2 conversion, backfill, alias or dual write.

GeoLibre may exist only under the declarative Recipe fixtures and acceptance
tests. Core Go and Dart code must not identify GeoLibre as a product type.

The migration registry preserves upstream `main` migrations v1 through v77
unchanged. All schema introduced on this undeployed branch is represented by
one direct-final v78 migration. A fresh database therefore creates the final
Agent and Execution V2 schema in one step. The schema is registered, but
runtime capability/readiness is still independently gated: v78, types, action
registration, and documentation do not publish a capability. v78 may directly
extend an upstream-main table where the final schema requires it, but must not
split branch work into chained compatibility migrations, compatibility tables,
old-row rewrites or branch-only backfills.

## 3. Discovery and public actions

`agent.backends.get` exposes only dependencies that are ready at request time.
Existing non-deployment embedded capabilities, such as model profiles, memory,
tasks, schedules, confirmations, remote MCP and reusable AWS control, retain
their current readiness checks.

Execution capability tokens (publication gated) are:

```text
execution.v2
execution.v2.plan
execution.v2.run
execution.v2.observe
execution.v2.provision
execution.v2.bindings
execution.v2.transport.aws_ssm
```

These names are contract identifiers, not evidence that a route is live. Types,
schemas, or action registration alone never publish a capability.

Deferred transports are advertised only after their own implementation,
failure semantics and acceptance tests are ready:

```text
execution.v2.transport.ssh
execution.v2.transport.http_api
```

No Execution V2 capability or action is public merely because its types,
schema or documentation exist. Publication requires a wired route, durable
repository, deterministic coordinator, executor, capability gate and passing
acceptance tests.

The V2 action namespace is `agent.execution.v2.*`. All actions are owner-only
and use the existing WS-first ProductCore envelope. Every mutation requires a
UUID `idempotency_key`; a mutation of an existing object also requires
`expected_revision`. The server computes every authoritative digest.

Once a mutation may have been dispatched over WebSocket, the client must not
replay it over HTTP. It may only query the authoritative object or request
typed reconciliation.

Native Agent may analyze a project, create or read a plan, request or inspect a
run, and read or invoke a Service Binding. It may not confirm a gate, send raw
SSM/SSH commands, call an AWS SDK passthrough or invoke an arbitrary URL.

## 4. Durable identities and state

The canonical identities are:

- `project_id`: stable project identity;
- `analysis_id`: one immutable project analysis;
- `target_id + target_revision`: an exact execution-target snapshot;
- `plan_id + plan_revision + plan_digest`: an immutable plan revision;
- `deployment_id`: stable identity for a long-running service;
- `run_id`: one execution attempt;
- stable `stage_key` and `step_key` in a plan;
- materialized `stage_id` and `step_attempt_id` in a run.

V2 does not use `workload_id`, `provision_id` or another V1 identifier.

Plans move from `draft` to `ready`, then optionally to `expired` or
`superseded`. Runs use:

```text
pending -> waiting_user -> queued -> running
        -> succeeded | failed | uncertain | canceled | rejected | expired
```

Stages use:

```text
blocked -> waiting_user -> queued -> running
        -> succeeded | failed | uncertain | skipped | canceled | rejected
        | expired
```

All plan, stage and step snapshots are strict, canonical, size-bounded JSON.
Unknown fields, trailing values, invalid normalization, row/snapshot mismatch
or digest mismatch fail closed.

## 5. Planning Skills, Recipes and typed steps

Built-in Planning Skills are trusted declarative planning inputs. They may
read allowlisted project facts and produce analysis or plan fragments. They
may not execute code, call a provider, read arbitrary local files, fetch
undeclared network content or mutate state.

Recipes are versioned declarative YAML. Their manifest pins the content
digest, schemas, allowed step kinds, target capabilities, network declarations
and secret purposes. Each plan pins the selected one to three Skill/Recipe
versions and digests.

The first supported typed steps are:

```text
target.inspect
compute.provision
compute.destroy
source.fetch
artifact.upload
package.ensure
file.put
container.apply
systemd.apply
script.run
http.probe
tcp.probe
artifact.collect
cleanup
```

`script.run` references an immutable content-addressed artifact. Inline shell
is invalid. The frozen step includes interpreter, argv, cwd, non-secret
environment, secret references, privilege, network grants, accepted exit
codes, timeout, output limit, redaction and postcondition.

Artifact metadata is durable in PostgreSQL. The first content backend is a
SHA-256-addressed atomic file store. It defaults to
`/var/dirextalk-message-server/agent/artifacts` inside the existing Message
Server data volume; `P2P_AGENT_ARTIFACT_DIR` is only an optional override.
Artifact paths are never trusted as authority and secret plaintext is
forbidden.

## 6. Compiler, policy and coordinator boundaries

- The planner produces candidate analysis and declarative fragments without
  credentials or side effects.
- The compiler normalizes typed steps, validates the stage DAG, resolves
  immutable pins and computes plan, stage and artifact-set digests.
- Policy authenticates the owner and rechecks exact target, credential,
  secret, quote, risk, expiry, revision and idempotency facts.
- The deterministic coordinator materializes one task per stage, executes
  steps in order through a typed remote transport, and persists receipts and
  readback after every step.

Workers use PostgreSQL row locks, CAS revisions, attempt numbers, lease epochs,
holders and expiries. An expired lease may be taken over without letting the
old holder commit. Domain terminal state, generic task terminal state, task
event, confirmation reservation release and concurrency decrement commit
atomically.

The same target cannot run conflicting mutations concurrently. Target
mutation leases are generic and contain no project identity.

Rollback is a separate run. It selects only frozen rollback steps for stages
proved applied by the source run, orders them by the inverse dependency DAG
and uses a fresh high-risk confirmation. It never silently executes rollback
or substitutes forward steps.

This rollback mechanism is not enabled for the initial generic container
Recipe. That Recipe is initial-deploy-only and contains no destructive cleanup
or replacement path. Upgrade, repair, destroy and rollback are rejected for it
until the product has a typed versioned or blue-green rollback strategy.

## 7. Confirmation contract

R0/R1 read-only or low-risk stages may queue automatically. R2 resource
purchase, secret access, sudo, remote mutation and repository write require
confirmation. R3 public network, DNS, TLS, migration and production cutover
use independent confirmations. R4 destroy, data deletion and destructive
rollback use an independent high-risk confirmation.

The authoritative binding covers:

```text
owner_id
plan_id / plan_revision / plan_digest
deployment_id
run_id / run_revision
stage_id / stage_digest
target_id / target_revision / target_digest
execution_digest
artifact_set_digest
network_digest
secret_grant_digest
policy_digest
cost_quote_digest
rollback_digest
preview_digest
risk_level / gate_type / expires_at
```

The server creates a safe `ConfirmationPreview`. It may expose identifiers,
titles, typed step names, target summary, risk, cost, rollback summary and the
normalized network grants selected for that stage, but not argv, environment
values, secret references, provider payloads or raw scripts. `network_digest`
binds both the exact target network policy and those selected grants. Flutter
renders this allowlisted projection and verifies only identity, linkage,
revision, binding digest, expiry and account scope.

Any change to a command, commit, artifact, target, secret revision, port,
quote, policy or rollback content creates a new immutable plan/stage revision
and invalidates the old confirmation.

## 8. AWS and remote execution

AWS credentials are encrypted, owner-scoped and revisioned. Read APIs are
strictly redacted. CloudFormation and AWS readback remain typed.

The first production transport is AWS SSM against a pinned
`aws_ec2_instance` target. New compute defaults to no public inbound access and
SSM management. Business ingress is a separate R3 stage.

The initial CloudFormation security group permits public IPv4 TCP/443 egress
to any destination. It does not enforce DNS names, TLS SNI, registry hosts or
URL paths. Plans using this profile must therefore declare the canonical broad
grant `https://*:443` (external scope) for each stage that uses it, including
`package.ensure` and `container.apply`. Exact OCI image content remains
SHA-256-pinned, but that content pin must not be presented as a network-policy
restriction.

The SSM transport provides typed inspect, dispatch, poll, reconcile, cancel
and artifact collection operations. It records the provider operation and SSM
Command ID as soon as they are known. A timeout, disconnect or lost response
after possible dispatch becomes `uncertain`. The coordinator performs
readback only; it never resends blindly or falls back to SSH, HTTP or local
execution.

Host CPU, memory, disk, EC2 state, SSM state and service probes are collected
through controlled read-only inspection. AWS/infrastructure views contain
infrastructure and health summaries only; project versions, images, commands,
DNS, TLS and migrations belong to the deployment/run event surface.

SSH and HTTP API transports remain absent until they provide durable operation
identity, idempotency, readback and their own capability gates.

## 9. Service Binding and remote jobs

A successful service deployment may create a machine-readable
`ServiceBinding` with pinned endpoint, protocol, authentication secret
references, operation schemas, health probe and usage/runbook artifacts.
`service_bindings.invoke` may call only a schema-pinned HTTP operation.
SSM/SSH administration always requires a new plan and run.

Remote coding is a later `purpose=job` use of the same plan/run/stage
framework. Local capability inspection may decide that the Message Server host
is too small, but it never authorizes local project execution. Repository
write, target retention and destruction remain separately confirmed stages.

## 10. Secrets and remote MCP

The version-1 AES-256-GCM keyring defaults to
`/var/dirextalk-message-server/agent/secret-keyring.json` inside the existing
Message Server data volume. `P2P_AGENT_SECRET_KEYRING_FILE` is only an
optional override.
Each encrypted row pins envelope/AAD version, key ID, nonce and ciphertext.
Canonical length-prefixed AAD binds secret domain, owner, entity,
revision/version, purpose/reference and binding digest.

Writes use the active key. Reads require the exact row key. Unknown keys,
authentication failure, wrong AAD or unsupported versions fail the dependent
Agent capability closed while ordinary Matrix/ProductCore service remains
available.

The undeployed branch needs no secret-schema compatibility migration. A fresh
database receives the final encrypted columns from v78. Secret administration
uses the same image and never accepts key material as a command-line argument.

Remote MCP accepts pinned HTTPS Streamable HTTP only. stdio, local MCP,
subprocesses and third-party Skill execution are rejected before side effects.
Tool schemas, grants, installation version and canonical input are digest
bound. Ambiguous mutation results become `uncertain` and are not replayed.

The public `POST /mcp` remains an independent Dirextalk MCP server with its
existing agent-token, Origin and room-permission enforcement.

## 11. Required verification and enablement

Before any Execution V2 capability is enabled:

- fresh PostgreSQL v1–v78 migration and direct-final schema assertions;
- strict snapshot, digest, owner/revision and idempotency tests;
- DAG, confirmation, replay, lease takeover, restart and atomic terminal tests;
- artifact path, symlink, content digest, size and secret-redaction tests;
- typed CloudFormation/SSM mocks with response-loss fault injection;
- GeoLibre and an unrelated container Recipe through the same generic path;
- Flutter account/client/generation fences and generic confirmation tests;
- full Go/Flutter analysis, test, build and diff checks;
- real AWS account acceptance before production enablement.

No legacy V1 count, drain, cutover or rollback procedure exists. Rollback of an
undeployed V2 release restores the matching PostgreSQL, artifact store,
keyring and application version as one consistent set.
