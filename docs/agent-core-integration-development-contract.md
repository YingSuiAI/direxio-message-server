# External Agent Core Integration Contract

Status: current split-service contract

This document defines the Message Server side of the Native Agent integration.
The Agent implementation, storage schema, and versioned Capability API are
owned by `dirextalk-agent` and `dirextalk-capability-api`. Current code,
generated ProductCore action metadata, Protobuf descriptors, and focused tests
override narrative documentation.

## 1. Topology and ownership

- Flutter connects only to `dirextalk-message-server` with the normal owner
  `access_token`. It never receives an Agent service address, service token,
  client certificate, database credential, or direct upload endpoint.
- `dirextalk-message-server` owns ProductCore authentication, the public
  `agent.*` action envelope, Native Agent WebSocket stream frames, product
  policy, Matrix state, and product capabilities.
- `dirextalk-agent` is the only Native Agent runtime. It owns conversations,
  model profiles and provider credentials, knowledge and long-term memory,
  tasks, schedules, confirmations, Skills/MCP installations, Execution V2,
  AWS configuration, and Agent runtime data.
- Online Agent remains the private Matrix `agent_room_id` conversation. Native
  Agent remains the ProductCore `agent.*` / `client.native_agent_stream`
  surface. The two transports do not share history or online-state inference.
- Message Server contains no in-process Native Agent runner, Agent database
  schema, model provider, knowledge index, scheduler, extension runner, or
  Execution V2 coordinator.

## 2. Public client contract

The existing owner-authenticated ProductCore action names and WebSocket frames
remain the Flutter contract. Message Server maps those actions to the external
Agent Capability gRPC catalog and never exposes a second client-facing API.

`agent.backends.get` keeps the current response envelope:

```json
{
  "core": {
    "available": true,
    "configured": true,
    "status": "ready",
    "instance_id": "...",
    "api_version": "...",
    "capabilities": [],
    "supported_model_providers": []
  },
  "embedded": {
    "available": false,
    "configured": false,
    "status": "disabled",
    "capabilities": [],
    "supported_model_providers": []
  }
}
```

`core` is the only executable Native Agent backend. `embedded` is a disabled
diagnostic value and is never selected, retried, or used as a fallback.
Completed discovery with an unavailable Core is an explicit error/retry state,
not a loading state.

The Core `capabilities` array is generated from the Agent registry descriptors
that passed publication readiness. Client-facing tokens are projections of
real registered operations, not configuration guesses. The current tokens are:

```text
agent.info
config
conversation
model.profile
model_profiles.server
model_roles.server
knowledge
memory.server
task
schedules.server
confirmation
skills.server
mcp
aws.control
voice.server
execution.v2
execution.v2.plan
execution.v2.run
execution.v2.observe
execution.v2.provision
execution.v2.bindings
execution.v2.secrets
execution.v2.transport.aws_ssm
execution.v2.transport.http_api
```

A token is omitted when its backing descriptor or required operation is not
published. In particular, a Product Capability bridge alone does not publish
`skills.server` or `mcp`. Registration, schema presence, or documentation alone
never makes an action live.

## 3. Capability protocols

Message Server to Agent uses the versioned Agent Capability gRPC service over
mTLS plus a deployment-generated service token. Agent to Message Server uses a
separate Product Capability gRPC service with its own peer identity and grant.
Both directions enforce:

- authenticated peer instance identity and account generation;
- server-derived owner identity and explicit granted scopes;
- operation ID, canonical request digest, and bounded idempotency window;
- call-chain ID, root operation ID, route, maximum depth, and loop rejection;
- request/result schema digests and advertised readiness;
- bounded deadlines, message sizes, concurrency, and sanitized errors.

Product Capability handlers must never synchronously call Agent. When a
workflow needs asynchronous follow-up, it records an event or durable callback
request and returns. A retry must retain the original immutable operation and
owner fences; it must not cross into a same-name replacement account or peer.

## 4. Data and secrets

Message Server and Agent may use the same PostgreSQL cluster, but they use
separate database roles and schema/database ownership. Neither service role may
read or mutate the other's private tables. Agent migrations are run only by the
Agent migration role; Message Server migrations do not create Agent tables.

This project uses a fresh-state baseline. There is no embedded-Agent row
upgrade, dual write, fallback store, compatibility adapter, or historical
database import. Account deletion may purge both services after revalidating
the owner and account-generation fence.

Provider keys, AWS credentials, service tokens, private keys, and database
passwords never appear in ProductCore responses, logs, errors, operation
digests, command arguments, or Git. Flutter writes provider credentials once
through the model-profile action, verifies redacted readback, clears its local
credential snapshot, and subsequently sends only exact profile/revision pins.

## 5. Readiness and failure semantics

Message Server publishes Native Agent actions only after a bounded catalog
probe proves every required action and schema. A successful proof is a short
lease. The probe loop renews before expiry with enough margin for both the
probe interval and timeout.

A failed proactive renewal may retain the previous proof only until that same
account generation's lease expires. Initial failure, expired lease, peer or
account-generation change, schema mismatch, authentication failure, or loop
detection fails closed. Unrelated Matrix/ProductCore traffic remains served,
while Native Agent calls return an explicit unavailable response.

The health path must not oscillate merely because a healthy lease was allowed
to expire before its next scheduled probe. Tests use a fake clock to cross
multiple lease boundaries and cover renewal failure, expiry, recovery, stop,
and account replacement.

## 6. Execution V2 boundary

Execution V2 state, plans, confirmations, receipts, artifacts, typed provider
ports, reconciliation, and uncertainty handling are Agent-owned. Message Server
authenticates and proxies the published ProductCore actions. It does not expose
raw SSH, SSM, AWS SDK, arbitrary URL, shell, or Docker-socket passthrough.

Every mutation requires a UUID idempotency key. Mutations of an existing object
also require the exact expected revision. After a WebSocket mutation may have
been dispatched, Flutter does not replay it over HTTP; it queries the durable
Agent state or invokes the typed reconciliation operation.

## 7. Required verification

A releasable revision must pass:

- Agent registry/discovery tests proving only registered operations produce
  client capability tokens;
- Message Server catalog, mTLS/token, owner/generation, digest, loop, deadline,
  and proxy adapter tests;
- Flutter tests for `core=ready` with `embedded=disabled`, Core unavailable,
  transport timeout, retry, model-profile synchronization, memory, schedules,
  voice, and Native Agent stream send/resume;
- database-role isolation and fresh migration tests;
- a real split-stack test spanning more than two catalog TTLs without a health
  gap, followed by model profile, conversation, knowledge/memory, and schedule
  readback through the Flutter-facing Message Server contract.

Release artifacts are versioned together by immutable image digests and a
matching Flutter build. Rollback restores a compatible Agent image and Agent
data snapshot together; Message Server never attempts to interpret Agent rows.
