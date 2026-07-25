# Agent Core integration development contract

Status: approved implementation target, not current production behavior  
Approved: 2026-07-25  
Server baseline: `origin/main` at `1d2e76d03273de5cc1225144dd1625c47ee88844`  
Companion baselines:

- Flutter: `7c5cb1bc38a18f7ca11894dbac1df36fea784717`
- Agent Core: `11eed51e2a9e6431f28039a542f2424f290e6fff`

This document is the cross-repository contract authority for reconnecting the
refactored Dirextalk Agent Core to Message Server and Flutter. The existing
Online/Matrix Agent remains a separate product path and is explicitly outside
this change.

## 1. Product decisions

The client keeps two levels of selection:

1. The existing top-level manual switch remains `Online/Matrix Agent` versus
   `Ying Agent`.
2. Inside Ying Agent settings, the owner may select `embedded` or `core` only
   when Message Server reports a configured Core. If Core is not configured,
   the nested selector and all Core-only pages are hidden and Ying uses the
   embedded backend.

Core configuration does not disable the embedded backend. Core is selected by
default the first time it becomes available, but selection is an owner,
client-local preference. A Core failure never triggers an automatic embedded
retry. The owner may manually switch to embedded after seeing the failure.

Only the node owner may discover, select, configure, chat with, install into,
approve, or operate either Ying backend. Product members receive neither
actions nor UI.

The Online/Matrix Agent keeps its current Matrix room, status state, Codex
bridge, commands, history, and send path. Core must not read, write, mirror,
provision, deactivate, or reinterpret that room.

## 2. Ownership and topology

```text
Flutter owner session
  |
  +-- Online/Matrix Agent --------------------> Matrix room / bridge (unchanged)
  |
  `-- Ying Agent
        |
        +-- embedded --------------------------> Message Server Eino runtime
        |
        `-- core ------------------------------> ProductCore adapter
                                                    |
                                                    `-- TLS gRPC
                                                         Agent Core
                                                         own PostgreSQL/data
```

Flutter never receives a Core endpoint, CA private material, service token, AWS
credential, extension secret, or runner credential. Message Server authenticates
the existing owner session first and then calls its one deployment-bound Core
instance.

Message Server and Agent Core must not share PostgreSQL, a data root, runtime
files, extension sockets, Docker sockets, execution history, model secrets, or
AWS credentials.

## 3. Core configuration and health

The adapter is explicitly configured by protected server deployment inputs:

- `P2P_AGENT_CORE_ENABLED`
- `P2P_AGENT_CORE_ADDRESS`
- `P2P_AGENT_CORE_SERVER_NAME`
- `P2P_AGENT_CORE_CA_FILE`
- `P2P_AGENT_CORE_TOKEN_FILE`
- `P2P_AGENT_CORE_EXPECTED_INSTANCE_ID`
- bounded connect, unary, stream-idle, and probe timeouts

`configured` means the adapter is enabled and all required values are
syntactically complete. An enabled but incomplete configuration is a startup
error. A complete configuration whose peer is temporarily unreachable does not
prevent Message Server startup.

The adapter reports one of:

- `not_configured`
- `ready`
- `unavailable`
- `incompatible`

`ready` requires a recent authenticated TLS 1.3 probe, the expected SNI and CA,
the deployment token, the expected `instance_id`, `api_version = v1`, and the
minimum Core capabilities. A public health response by itself is insufficient.
Unknown API versions or missing required capabilities fail closed as
`incompatible`.

The owner query `agent.backends.get` returns a sanitized projection:

```json
{
  "embedded": {
    "available": true,
    "capabilities": ["chat", "models.query", "bundled_tools", "bundled_skills"]
  },
  "core": {
    "configured": true,
    "status": "ready",
    "instance_id": "redacted-safe-id",
    "api_version": "v1",
    "capabilities": ["agent.info", "conversation", "model.profile", "task"],
    "supported_model_providers": [
      "openai_compatible",
      "anthropic",
      "gemini"
    ]
  }
}
```

The response never contains target addresses, certificate paths, tokens, error
chains, credential fingerprints that identify an account, or raw upstream
messages. Capability names are the intersection of:

1. enabled capabilities reported by Core;
2. capabilities understood by this Message Server build; and
3. capabilities allowed by local product policy.

Capability tokens are case-sensitive and have one cross-repository meaning:

| Token | Minimum Core evidence | Product/UI surface |
| --- | --- | --- |
| `agent.info` | authenticated instance and capability discovery | Core status |
| `model.profile` | model profile CRUD and atomic sync | shared model page |
| `conversation` | durable conversation and turn services | Core chat |
| `conversation.extensions` | production chat resolver and Runner dispatch | MCP/Skill selection in chat |
| `task` | durable task CRUD/events | Tasks |
| `schedule` | schedule service | Schedules |
| `confirmation` | durable confirm/reject/consume | Approvals |
| `mcp` | MCP lifecycle service | MCP management |
| `skill` | Skill lifecycle service | Skill management |
| `knowledge` | Knowledge service | Knowledge |
| `aws.control` | typed AWS credential/CloudFormation service | AWS and CloudFormation |
| `workload.core_runner` | ready isolated Workload Runner | Core-host workloads |
| `workload.aws_ssm` | typed SSM workload provider | EC2 service installation |
| `workload.aws_ecs` | typed ECS workload provider | OCI services |

Core is usable for Ying chat only when `agent.info`, `model.profile`, and
`conversation` are enabled. Optional tokens hide only their dependent pages;
they do not make basic Core chat incompatible.

Capability tokens are boolean. For Core API `v1`, Message Server projects the
fixed model-provider identifiers `openai_compatible`, `anthropic`, and `gemini`
as `supported_model_providers`; they are not inferred from optional capability
tokens.

## 4. ProductCore adapter surface

Core operations use explicit owner-only ProductCore actions under
`agent.core.*`. Existing embedded `agent.*` actions keep their wire meaning;
the new client chooses the appropriate adapter and does not ask the server to
guess a backend.

The first implementation exposes these action families:

| Family | ProductCore actions |
| --- | --- |
| Discovery | `agent.backends.get`, `agent.core.status.get` |
| Models | `agent.core.model_profiles.sync`, `.list`, `.get`, `.delete` |
| Conversations | `agent.core.conversations.create`, `.get`, `.list`, `.delete`, `.chat` |
| Tasks | `agent.core.tasks.get`, `.list`, `.cancel`, `.retry`, `.events` |
| Confirmations | `agent.core.confirmations.get`, `.list`, `.confirm`, `.reject` |
| MCP | `agent.core.mcp.discover`, `.get`, `.list`, `.install`, `.update`, `.remove`, `.execute` |
| Skills | `agent.core.skills.discover`, `.get`, `.list`, `.install`, `.update`, `.remove`, `.execute` |
| Workloads | `agent.core.workloads.plan`, `.get`, `.list`, `.quote`, `.apply`, `.destroy` |
| AWS | `agent.core.aws.credentials.create`, `.update`, `.delete`, `.list`, `.test`; existing typed plan/change reads needed by workloads |

Exact request and response fields mirror the versioned Core Protobuf after
normalizing timestamps, enums, optional values, and pagination into the
ProductCore JSON conventions. The generated
`docs/product-action-contract.json`, action registry, public DTO tests,
`AsClient`, `HttpAsClient`, realtime transport, and Flutter test doubles change
together.

Core gRPC errors map to stable ProductCore results:

| gRPC condition | ProductCore HTTP status | Stable code |
| --- | ---: | --- |
| invalid argument | 400 | `agent_core_invalid_argument` |
| unauthenticated / permission denied | 502 | `agent_core_trust_failed` |
| not found | 404 | `agent_core_not_found` |
| aborted / revision conflict | 409 | `agent_core_conflict` |
| failed precondition | 409 | `agent_core_precondition_failed` |
| deadline / unavailable | 503 | `agent_core_unavailable` |
| incompatible protocol/capability | 502 | `agent_core_incompatible` |
| unknown upstream failure | 502 | `agent_core_upstream_failed` |

Responses and logs use sanitized summaries; they do not forward arbitrary gRPC,
model-provider, command, extension, AWS, or database error text.

## 5. Durable conversation and streaming contract

Core chat is a first-class owner realtime stream, separate from both Matrix and
the embedded Native Agent stream:

- client frame: `client.agent_core_stream`
- cancel frame: `client.agent_core_stream.cancel`
- server frames:
  `server.agent_core_stream.accepted`,
  `server.agent_core_stream.event`,
  `server.agent_core_stream.error`, and
  `server.agent_core_stream.cancelled`

The Agent Protobuf adds a durable turn surface:

- `ConversationService.StartTurn`
- `ConversationService.GetTurn`
- `ConversationService.WatchTurnEvents`
- `ConversationService.CancelTurn`

`StartTurn` commits a request digest and an immutable association among the
client idempotency UUID, Core turn ID, conversation ID, model-profile revision,
message, extension selections, and expected conversation revision before it
returns accepted. Execution is lease-driven and no longer owned by the caller's
gRPC context. `GetTurn` returns the current immutable terminal result when one
exists. `WatchTurnEvents(after_sequence)` replays durable Core events. A replay
gap returns explicit earliest/latest bounds rather than silently skipping data.

`CancelTurn` is an idempotent, revision-aware request. An already committed
terminal result wins a cancel race; otherwise cancellation becomes the single
terminal outcome only after execution and descendant cleanup are fenced.
Terminal state cannot be rewritten.

The client frame has:

```json
{
  "type": "client.agent_core_stream",
  "turn_id": "uuid",
  "request_digest": "64 lowercase hexadecimal characters",
  "after_sequence": 0,
  "params": {
    "conversation_id": "uuid-or-empty",
    "expected_revision": 0,
    "client_model_profile_id": "stable-client-id",
    "message": "text",
    "extensions": []
  }
}
```

`request_digest` is required on every attach and is the SHA-256 digest of the
canonical JSON encoding of the immutable initial `params` object (UTF-8,
lexicographically sorted object keys). The canonical object includes the
full initial message, client model profile, conversation, expected revision,
and extension selections, but excludes the transport-only
`request_digest`, `after_seq`, and `after_sequence` fields. A first reservation
requires a non-empty message and an exact digest match. A replay with an
already-bound Core turn may omit the message and profile and reattach using
only the stored digest; a replay whose Core turn is not yet bound must resupply
the original message so the server never submits an empty semantic Start.
Missing/invalid digests use `agent_core_digest_required` or
`agent_core_invalid_argument`; a digest that differs from the ledger uses
`agent_core_digest_mismatch`, and an ambiguous pre-Core replay uses
`agent_core_reconciliation_required`.

Accepted/event/error/cancelled frames always contain `turn_id`; events contain
the Core event `seq`, a stable `event` token, and sanitized `data`. Accepted
also returns `core_turn_id`, `conversation_id`, `status`, and `latest_seq`.
Errors include only a stable `code`, `retryable`, and replay bounds when
applicable.

The adapter validates every upstream event before forwarding it: the event
turn ID must match, replay sequences must be strictly greater than the
requested cursor and monotonic, replay bounds must be ordered, and event kinds
must use the Core vocabulary. Malformed upstream responses become
`agent_core_stream_failed` and are never rendered as ordinary events.

Message Server durably stores the client/Core turn mapping, request digest,
last forwarded Core sequence, and terminal projection. The client-supplied
`turn_id` is reused as the Core idempotency key and never retried with a new
key after an ambiguous dispatch.

Detaching a websocket does not cancel accepted work. Explicit cancel calls the
durable Core cancel operation and follows the turn until a terminal result.
After Message Server process loss, the adapter uses `GetTurn`/`WatchTurnEvents`
and its ledger to reconcile; it must never submit the prompt again merely
because the stream outcome is unknown.

Core owns Core conversation IDs, messages, revisions, tool summaries, and task
links. Flutter may cache projections and drafts but does not mirror Core
messages into a Matrix timeline or the embedded local history store.

## 6. One model configuration page

Flutter remains the editing source for the one visible model-profile page and
keeps API keys only in its secure credential store. Non-secret profile fields
remain in its existing account-scoped local store.

When the owner:

- selects Core;
- saves a model profile while Core is selected; or
- retries a previously failed Core synchronization,

Flutter calls `agent.core.model_profiles.sync`. The request includes stable
client profile IDs, complete non-secret settings, the selected default profile,
and only the API keys required to create or rotate changed profiles.

Core adds an atomic `ModelProfileService.Sync` RPC. Flutter filters the shared
page and sends only entries whose provider is in
`supported_model_providers`. The request contains one batch idempotency UUID,
`default_client_profile_id`, and compatible profile entries with
`client_profile_id`, optional expected Core revision, complete settings, and an
optional write-only key. An Embedded-only profile never enters the RPC, and the
default reference must belong to the submitted compatible set; otherwise
Flutter blocks before dispatch. Missing Core profiles are preserved; deletion
is never implied by sync. Validation, revision checking, profile writes, and
default selection occur in one PostgreSQL transaction. Any entry failure
changes nothing. A same-key/same-digest replay returns the original sanitized
result; the same key with different content conflicts.

Message Server streams the request over authenticated TLS to Core and never
persists or logs keys. Core persists the protected model credential and returns
profile IDs, revisions, `api_key_configured`, and a sanitized sync result.
Ordinary list/get responses never return keys.

Automatic sync is revision-aware and all-or-report: per-profile failures are
returned explicitly and prevent chat from silently selecting a different
profile. Deleting a local profile does not silently destroy a Core profile;
the owner-facing delete operation is explicit. The current single-device owner
session avoids concurrent writers, but stale revisions still fail visibly.

Embedded chat continues to receive the selected request-scoped profile and key
from the same page. Core and embedded never copy conversation history or
runtime configuration between each other.

The shared page retains every provider supported by Embedded. Core sync accepts
only the Core-compatible subset reported by capability metadata. The initial
Core subset is OpenAI-compatible, Anthropic, and Gemini; `openai` maps to
`OPENAI_COMPATIBLE`. Selecting Core with an incompatible default profile blocks
send and asks the owner to choose/configure a compatible profile. It never
rewrites or deletes the Embedded profile.

## 7. Embedded Ying security boundary

The embedded Eino path remains available as a basic fallback, but this version
enforces the boundary on the server, not only in Flutter.

Embedded may use:

- model chat and streaming;
- Dirextalk tools compiled into this Message Server release;
- Skills whose immutable content is shipped in this repository/release;
- ordinary memory/config behavior required by those bundled components.

Embedded may not:

- invoke `runtime__shell`;
- install or run arbitrary binaries, packages, commands, or runtime CLIs;
- add/update/remove third-party MCP servers or Skills;
- load MCP/Skill definitions from mutable native data directories or URLs;
- use arbitrary stdio/HTTP MCP configured at runtime; or
- recover a denied operation through shell, an external MCP, or a legacy plugin
  path.

Legacy native installation/runtime actions remain registered only when
compatibility requires it and return a stable unsupported/forbidden result.
They are absent from advertised capabilities and hidden by current Flutter.
Bundled tools and Skills are selected from a build-owned allowlist; deployment
configuration may disable entries but cannot add new entries.

The standard external `POST /mcp` endpoint is a separate inbound service and is
not an Embedded runtime capability. Its existing bearer and room-policy
contract remains unchanged. Embedded cannot call it to recover a denied tool.
Online/Matrix Agent account/session/config actions are also not changed.

## 8. Core extensions and arbitrary workloads

All third-party MCP/Skill installation and execution moves to Agent Core.
Core may install arbitrary supported sources and prioritize functionality over
the embedded server's production-hardening limits. It still must:

- bind source/version/content, network grants, secret grants, and requested
  permissions to a durable plan digest;
- request owner confirmation before installation, update, removal, arbitrary
  command execution, or new external exposure;
- execute local code only through its separate Runner;
- keep secrets write-only and redact output; and
- report uncertain execution as uncertain rather than replaying it.

The durable `ConfirmationService` is not required for ordinary owner data-plane
mutations such as conversation create/chat/delete, model-profile sync, task
cancel/retry, or AWS credential record CRUD. Those operations still require the
authenticated owner, revisions/idempotency, and an ordinary destructive UI
prompt where applicable.

Durable owner confirmation is required for extension install/update/remove,
arbitrary commands, workload apply/destroy, external exposure, and every AWS
resource mutation. Installation confirmation makes the exact pinned MCP/Skill
discoverable and selectable; it does not pre-approve later side effects.

Only a code-shipped, contract-tested tool classified as read-only may execute
without a per-call confirmation. Every third-party tool, unknown/unclassified
tool, Skill-triggered command, or tool declared to create/update/send/delete,
use a secret, access a new network target, expose a service, or incur cost
requires a new confirmation binding its exact tool/version/digest, argument
summary, target revision, and grants. The durable turn enters
`confirmation_required` without executing the call and resumes the same turn
only after that confirmation is consumed. Rejection/expiry terminalizes that
tool step without substituting another backend.

There is no interactive terminal surface. Arbitrary Core-host installation is
represented as a durable workload plan:

```text
plan -> quote/impact -> owner confirmation -> Task -> events -> terminal result
```

The Runner may execute arbitrary commands inside its isolated task/workload
environment. Long-running services run as separately identified workloads
rather than becoming children of the Core API process.

## 9. AWS deployment and scheduling

AWS is owner-provided: the owner supplies credentials or an assumable role and
AWS bills that account directly. Message Server never stores AWS credentials;
it proxies write-only values to Core.

Core supports:

- arbitrary CloudFormation through its existing typed plan/change flow;
- EC2 provisioning followed by arbitrary installation through AWS Systems
  Manager Run Command;
- arbitrary OCI image deployment to ECS;
- website deployment through either SSM-managed EC2 or ECS; and
- deployment of other Agent services by the same workload abstraction.

SSH is not opened or managed by this feature. SSM instances must use an
explicit instance profile and pass managed-instance readiness before install.
ECS inputs pin an image digest at confirmation time. Commands, templates,
parameters, regions, accounts, network exposure, secret references, expected
cost, and target revisions are part of the confirmation digest.

Every action that creates, updates, destroys, exposes, installs, executes, or
can mutate AWS or incur spend requires a fresh owner confirmation. AWS
credential record create/update/delete is owner-authenticated, revision-aware,
and write-only but does not itself consume a durable confirmation because it
does not call AWS. Credential use does not bypass the resource-operation
confirmation. Scheduled work may prepare a plan and produce a pending
confirmation, but cannot spend or mutate AWS unattended. Rejection or expiry
requires a new plan/confirmation. Credential reads never return secret values.

Account deletion does not destroy AWS resources. Cloud destruction remains an
explicit, separately confirmed workload operation.

## 10. Two-Compose development topology

Local integration uses two independent Compose projects:

```text
message project                         agent project
----------------                         -------------
message-server-init                      agent-migrate
message-server                           agent-core
message-postgres                         agent-postgres
                                          extension-runner
                                          optional workload runner
             \                           /
              shared private integration network
```

They use distinct databases, volumes, data roots, service users, and project
names. Only a private, explicitly created integration network is shared.
Agent gRPC is not published to the host in the normal profile. TLS certificate,
CA, and service token are generated into ignored local secret files and mounted
read-only. Neither project mounts the other project's data or Docker socket.

The local acceptance order is:

1. Agent database migration and authenticated Core readiness.
2. Message Server startup with Core disabled.
3. Message Server startup with Core configured and authenticated.
4. Capability and nested-selector verification.
5. Real model profile synchronization and streamed conversation.
6. MCP and Skill install, confirmation, invocation, cancellation, and cleanup.
7. Core-host workload confirmation and execution.
8. Fake-AWS SSM/ECS/CloudFormation lifecycle.
9. Explicitly authorized real-AWS create/read-back/destroy audit.

## 11. Verification and release gates

Required automated evidence:

- Agent protobuf/buf checks, unit tests, PostgreSQL integration tests, runner
  isolation tests, fake AWS tests, and production-composition tests.
- Server generated action-contract tests, owner-auth tests, Core fake-server
  tests, bad CA/token/instance/capability tests, stream detach/cancel/reconcile
  tests, and embedded deny-list/allowlist tests.
- Flutter transport, provider, secure model sync, switch visibility, widget,
  stream resume, confirmation, MCP/Skill, workload, and error-state tests;
  `flutter analyze --no-pub`.
- `docker compose config`, image builds, health checks, secret-leak scan, and
  the full two-project smoke test.

Real service acceptance must record immutable image revisions, Core instance
identity, AWS account/region, exact confirmed digest, resources created,
independent read-back, explicit destruction, and a zero-resource residue audit.
It requires separately supplied deployment targets, credentials, budget, and
owner confirmations; no repository value is treated as deployment authority.

## 12. Implementation order

1. Freeze and test Core protobuf additions plus chat extension wiring.
2. Implement Core workload/SSM/ECS execution and confirmations.
3. Enforce the embedded allowlist and block native installation/runtime APIs.
4. Implement the Message Server Core client, capability probe, actions, and
   realtime stream.
5. Implement the one-page Flutter model sync and nested backend selector.
6. Add Core extension/task/confirmation/workload UI behind capabilities.
7. Build the two Compose projects and complete local end-to-end acceptance.
8. Perform the explicitly authorized real deployment acceptance.

No old `adam/agent-one` code is merged wholesale. A reference change is reused
only after it is shown to implement this contract against the current three
baselines.
