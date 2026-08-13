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
    "release_version": "v1.0.0",
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
static_sites.server
workers.server
task
schedules.server
confirmation
skills.server
mcp
aws.control
voice.server
text_tools.server
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

`memory.server` covers durable Native conversation/history support and remains
the client availability gate for the adjacent automatic-memory controls. The
five public memory actions bind exactly to `agent.memory.v1/get_config`,
`update_config`, `status`, `update_fact`, and `delete_fact`. The descriptor
schemas are digest-pinned; config updates require UUID idempotency plus expected
revision, fact mutations require an exact fact UUID plus UUID idempotency, and
result projection is closed over non-secret model readiness, facts, timeline,
and observation counters.

`static_sites.server` is published only when the ready
`agent.static_sites.v1` descriptor contains both `list_releases` and
`delete_release`. Flutter gates the server-backed release inventory on this
token. Its only public ProductCore actions are `agent.static_sites.list` and
`agent.static_sites.delete`; static HTML publication remains an Agent tool
operation rather than a second Message Server action.

`workers.server` is published only when the ready `agent.worker.v1` descriptor
contains `list_workers`, `get_worker`, `destroy_worker`, `bind_domain`, and
`unbind_domain`. The matching owner actions are `agent.workers.list`,
`agent.workers.get`, `agent.workers.destroy`, `agent.workers.bind_domain`, and
`agent.workers.unbind_domain`. Worker identity and the explicit mutation
confirmation are forwarded unchanged; AWS ownership and Route 53 read-back
remain Agent responsibilities. Domains use the Worker's ordinary public IPv4
and do not require an EIP.

The model-profile sync/list contract includes
`default_tool_client_profile_id`. An empty value means no tool default; a
non-empty value must identify a conversation-kind profile in the same
authoritative profile set. This distinct role is part of the readiness proof
behind `model_profiles.server` and `model_roles.server`.

## 3. Capability protocols

Message Server to Agent uses the versioned Agent Capability gRPC service over
mTLS plus a deployment-generated service token. Agent to Message Server uses a
separate Product Capability gRPC service with its own peer identity and grant.
The private Message Server-to-Agent channel explicitly bypasses process-wide
HTTP(S) proxy settings and connects only through the capability network. Its
catalog probe budget exceeds the gRPC minimum connection window so the first
mTLS connection and authenticated catalog RPC can complete within one probe.
Both directions enforce:

- authenticated peer instance identity and account generation;
- server-derived owner identity and explicit granted scopes;
- operation ID, canonical request digest, and bounded idempotency window;
- call-chain ID, root operation ID, route, maximum depth, and loop rejection;
- request/result schema digests, durable-stream event schema digests, and advertised readiness;
- bounded deadlines, message sizes, concurrency, and sanitized errors.

For a durable operation, the bounded CallContext deadline authorizes each
admission or control RPC; it is not a total execution or observation timeout.
Message Server renews the Watch admission context and its domain-separated
control grant after Start succeeds. The live Watch is then owned by the client
WebSocket lifecycle and may outlive that admission deadline. A Watch is
detached after 30 consecutive minutes without a persisted event, on client
disconnect, or on explicit stream cancellation; detaching never cancels the
durable operation, and observation resumes from the persisted sequence cursor.

Product Capability handlers must never synchronously call Agent. When a
workflow needs asynchronous follow-up, it records an event or durable callback
request and returns. A retry must retain the original immutable operation and
owner fences; it must not cross into a same-name replacement account or peer.

## 4. Data and secrets

Message Server and Agent may use the same PostgreSQL cluster, but they use
separate database roles and schema/database ownership. Neither service role may
connect to, read, or mutate the other's private database or tables. The split
deployment uses one PostgreSQL container and volume attached to both isolated
database networks, with one network-specific hostname per application. Each
application receives only its own non-superuser role DSN; the PostgreSQL
cluster-admin credential is a third protected secret mounted only into the
database service. Agent migrations are run only by the Agent role; Message
Server migrations do not create Agent tables.

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

For each required operation, the probe verifies that input and result schema
JSON is present, each advertised schema digest matches its bytes, and pinned
Agent Core schema identities match the current contract. Durable streams also
pin their event schema and digest. Rehashing the catalog does not make an
empty, incompatible, or stale schema acceptable; schema contents are never
included in readiness errors or logs.

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

Execution V2 state, plans, runs, artifacts, typed provider ports,
reconciliation, and uncertainty handling are Agent-owned. User authorization
uses only the generic Agent CoreConfirmation actions; Execution V2 does not
publish confirmation aliases. Message Server authenticates and proxies the
published ProductCore actions. It does not expose raw SSH, SSM, AWS SDK,
arbitrary URL, shell, or Docker-socket passthrough.

Every mutation requires a UUID idempotency key. Mutations of an existing object
also require the exact expected revision. After a WebSocket mutation may have
been dispatched, Flutter does not replay it over HTTP; it reads the durable
Agent state. Provider reconciliation is an Agent background-controller concern,
not a public `runs.reconcile` operation.

Cloud Worker plans are proposed inside an Agent-owned Native conversation, not
created by an App action. Message Server forwards the complete server-authored
`related_task_ids`, `related_plan_ids`, and strict Execution V2 references in
unary and streaming history/results. A reference carries account generation
and exact task/plan/run/confirmation revision/digest linkage; Message Server
does not manufacture, weaken, or use it as authorization.

Verified Cloud Worker deliverables are read only through
`agent.execution.v2.artifacts.download`. The request is the closed
`record_kind=cloud_worker`, `artifact_id`, `offset_bytes`, and
`max_chunk_bytes` shape; every field is required and a chunk is limited to
512 KiB. The offset is bounded by the Cloud Worker 8 MiB artifact hard limit;
each successful response advances by a non-empty chunk, and the last chunk
sets EOF. The direct response carries only owner/account-generation and
artifact/execution identity, canonical base64 bytes, chunk and whole-artifact
SHA-256, total/range metadata, and EOF. Message Server revalidates that closed
shape, prepared owner authority, request identity, decoded chunk digest, and
range continuity before returning it. S3 bucket/key/version, signed URLs,
retention internals, Worker diagnostics, and provider errors are never part of
the public contract.

After Agent has frozen one terminal conversation result and independently
verified AWS cleanup, it calls the private fixed
`product.agent_execution.v1/record_completion` operation. This callback is
not advertised in the Product Capability catalog and accepts no user/model
Permission. The existing mTLS, direction token, Agent peer identity, fresh
call-chain route, and positive account generation still apply; owner identity
is injected from `Service.OwnerMXID()` rather than request JSON.

Message Server atomically stores one minimal receipt and one
`agent.execution.v2.completed` realtime invalidation. Exact event/execution and
payload replay succeeds idempotently; a different payload conflicts. The
receipt contains only event/execution/run/conversation/turn identity, terminal
state, completion time, and payload digest. It contains no result-message
identity because the central continuation owns the eventual assistant message,
and contains no result body, quote, artifact details, S3 address, AWS
resource identity, secret, or Worker diagnostics. Flutter treats the realtime
event only as an invalidation and reads authoritative history and Execution V2
state back from Agent.

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
