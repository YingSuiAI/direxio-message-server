# ADR: Execution Orchestration V2

Date: 2026-07-31
Status: Accepted contract; implementation and release gated

## Context

The embedded Agent control plane owns durable tasks, confirmations, AWS
credentials and Execution V2 planning/runtime state. The unreleased V1
workload/provision surface has been removed. The active boundary composes
declarative planning with a server-owned deterministic coordinator and remote
target transports without turning the Message Server into a shell runner or a
generic cloud proxy.

This ADR is authoritative for the V2 orchestration boundary. It does not make
any V2 route, storage, executor, capability, or action live by documentation
alone.

The direct-final v78 migration registers the final V2 schema. Runtime
capability/readiness remains a separate gate; schema registration, action
registration, and documentation never publish an execution capability by
themselves.

## Decision

### 1. Planning is declarative and side-effect free

The server may ship built-in declarative Planning Skills/Recipes. They are
trusted, versioned inputs to planning only and may produce immutable plan
fragments. A fragment is canonicalized, secret-free, owner-scoped, and bound
to its content digest and referenced credential/configuration revisions. A
Planning Skill/Recipe cannot execute a command, call a provider, fetch
third-party code, or mutate state.

GeoLibre is a fixture/recipe used to exercise this boundary. It is not a
product target, public action, provider guarantee, or capability contract.

### 2. Responsibilities are split by boundary

- **Planner** validates a declarative request and invokes only built-in
  Planning Skills/Recipes to produce candidate fragments. It has no provider
  credentials and performs no side effect.
- **Compiler** merges and canonicalizes fragments into a typed stage graph,
  rejects unsupported or conflicting fragments, and emits the immutable plan
  digest. It does not dispatch or retry a mutation.
- **Policy** authenticates the owner and evaluates capability, target,
  credential/configuration revision, risk, expiry, quota, and idempotency
  constraints. It cannot weaken a frozen plan or turn an untyped payload into
  a provider command.
- **Coordinator** is the server-owned deterministic control-plane component.
  It resolves a frozen plan/stage from durable state, dispatches only its typed
  steps to a remote target through a versioned transport, and persists typed
  receipt, progress, readback, or error evidence. It never executes project
  code or third-party shell locally and has no fallback local runner.

The typed transport carries an operation/stage identity, plan and input
digests, bounded typed parameters, immutable artifact references, idempotency
markers, and provider revision references. It never accepts raw SSH, SSM, AWS,
HTTP, DNS, or TLS passthrough fields; `script.run` can only reference a
digest-pinned script artifact already covered by the stage confirmation.

### 3. Confirmation and uncertain outcomes are explicit

The compiled plan and each mutating stage are frozen before confirmation. A
confirmation records the exact plan digest, stage digest, policy/revision
facts, expiry, and idempotency key. Changing any of those facts requires a new
plan and confirmation. This is an execution.v2 control-plane confirmation; it
is not Native Agent chat confirmation and Native Agent confirmation must never
authorize a V2 mutation.

After a mutating dispatch is accepted by the coordinator, a lost or ambiguous
response is recorded as `uncertain`. The server must not replay the mutation,
switch transports, fall back to local execution, or consume a second
confirmation. Recovery is read-only typed reconciliation or an explicitly
authorized destroy/recovery operation. A successful receipt, terminal failure,
or reconciliation evidence must remain bound to the original plan/stage
digests.

### 4. Fresh-install V2 cutover

V1 was never deployed and has no historical user or deployment data. The
implementation therefore has no V1 compatibility, migration, read, observe,
reconcile, destroy, or adapter obligation. V2 is the only deployment and
execution contract. V1 workload, GeoLibre, deployment-ledger, public action,
Flutter DTO/state, and database objects are removed rather than projected or
silently converted.

The PostgreSQL registry keeps the migrations already present on upstream
`main` unchanged. All branch-only migrations after that baseline are replaced
by one fresh-install migration that creates the final Agent and Execution V2
schema directly. It contains no V1 backfill, `source_kind` bridge, compatibility
alias, old-row rewrite, or staged V1-to-V2 upgrade path. GeoLibre remains only
an explicitly selected V2 recipe fixture and never creates a project-specific
schema or public action.

The Message Server continues to reject local third-party shell/code/skill
execution, raw SSM/SSH/AWS passthrough, and any mutation replay or fallback
after ambiguous dispatch. A server-owned coordinator does not relax those
rules.

### 5. Capability and rollout discipline

`execution.v2.*` capability tokens and actions are unpublished until all of
the following are implemented and enabled together:

1. the authenticated server route and request/response contract;
2. durable storage, idempotency, digests, confirmation, and uncertainty
   records;
3. the typed coordinator transport and server executor;
4. focused contract, restart, concurrency, policy, and ambiguous-dispatch
   tests; and
5. an explicit deployment enablement decision.

Discovery must omit an unready capability, and documentation must label an
unimplemented phase as planned or deferred rather than live.

### 6. First production slice and deferred work

The first production slice is AWS SSM long-running services through the typed
coordinator, including frozen plans/stages, bounded progress, typed readback,
and uncertain/reconciliation handling. SSH, generic HTTP, DNS, TLS, and the
Coding Worker remain deferred; they are not implied by the V2 contract and
must not be advertised until separately implemented and accepted.

The initial EC2 profile enforces no public inbound traffic but does permit
public IPv4 TCP/443 egress to any destination. The plan and confirmation must
therefore represent that boundary as the explicit wildcard grant
`scheme=https, host=*, port=443, scope=external`. A registry hostname or path
is not an enforced network restriction and must not be shown or digested as
one. OCI images remain independently pinned to an immutable SHA-256 digest.
Both package bootstrap and image pull stages declare this broad grant.

The first generic container Recipe supports initial deployment only. It has
no cleanup rollback and will fail if a container with the stable service name
already exists under a different execution-spec digest; it never deletes that
container to perform an in-place replacement. Upgrade, repair, destroy and
rollback operations are not selected or exposed for this Recipe until a
versioned or blue-green deployment/rollback model is implemented and accepted.

## Acceptance gates

- Planner/Recipe output is deterministic, immutable, digest-bound, and has no
  provider or secret side effects.
- Compiler and policy tests reject conflicts, stale revisions, malformed
  typed stages, and raw passthrough fields.
- Coordinator transport tests cover success, typed progress/readback,
  timeout, lost response, and `uncertain` without mutation replay or local
  fallback.
- Frozen plan/stage confirmation is owner-scoped, revision-checked,
  idempotent, expiry-aware, and independent of Native Agent confirmation.
- Confirmation previews disclose the actual selected network grants and bind
  both those grants and the target network policy into `network_digest`.
- AWS SSM long-running service acceptance passes before publishing any
  `execution.v2.*` capability/action.
- Fresh installation creates only the final V2 deployment/execution schema;
  no branch-only V1 migration ID, table, backfill, action, DTO, or
  compatibility test remains.
- Deferred transports and GeoLibre fixture/recipe paths have no live action or
  capability claim.

## Consequences

Planning remains extensible without granting the Message Server arbitrary code
execution. Provider execution can evolve in a remote control plane while the
server retains owner authorization, immutable intent, policy, and evidence
boundaries. The cost is an explicit typed schema and durable uncertainty path
for every mutating stage. Because there is no deployed V1 state, the branch
uses a clean V2 cutover instead of maintaining parallel orchestration models.
