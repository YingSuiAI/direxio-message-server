# ADR: Execution Orchestration V2

Date: 2026-07-31  
Status: Accepted contract; implementation and release gated

## Context

The embedded Agent control plane owns durable tasks, confirmations, AWS
providers, workloads, and deployment reads. V1 is deliberately typed and
server-owned, but its create path is not the long-term boundary for remote
execution. We need a contract that can compose declarative planning with a
server-owned deterministic coordinator and remote target transports without
turning the Message Server into a shell runner or a generic cloud proxy.

This ADR is authoritative for the V2 orchestration boundary. It does not make
any V2 route, storage, executor, capability, or action live by documentation
alone.

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

### 4. V1 and V2 cutover

V1 create remains available during V2 implementation and rollout. It may be
retired only after the V2 acceptance gates in this ADR pass in the target
deployment and the replacement path is enabled. V1 read, observe, reconcile,
and destroy behavior remains retained for compatibility and operational
recovery; V2 acceptance must not remove those legacy facts.

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
- AWS SSM long-running service acceptance passes before publishing any
  `execution.v2.*` capability/action.
- V1 create retirement is a post-acceptance release decision; legacy read,
  observe, reconcile, and destroy remain covered by compatibility tests.
- Deferred transports and GeoLibre fixture/recipe paths have no live action or
  capability claim.

## Consequences

Planning remains extensible without granting the Message Server arbitrary code
execution. Provider execution can evolve in a remote control plane while the
server retains owner authorization, immutable intent, policy, and evidence
boundaries. The cost is an explicit typed schema and durable uncertainty path
for every mutating stage, plus a staged rollout that keeps V1 create until V2
is proven.
