# Team Worker Facade Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose Agent Core `agent.team.v1` through the existing owner-authenticated ProductCore action surface and deliver deduplicated Team completion invalidations without storing Team execution history in Message Server.

**Architecture:** Message Server remains a transport and policy facade. Owner requests are schema-validated, mapped to the Agent Capability catalog over the existing mTLS gateway, and adapted into stable ProductCore DTOs. Agent completion is accepted asynchronously through the existing Agent-to-Message-Server Product Capability service, persisted as a minimal receipt plus ProductCore event, and never calls Agent synchronously.

**Tech Stack:** Go 1.26, PostgreSQL 18, gRPC/Protobuf Capability API, ProductCore HTTP/WebSocket actions, existing P2P event stream.

---

## Contract Map

| ProductCore action | Agent capability operation | Type |
|---|---|---|
| `agent.team.plans.get` | `agent.team.v1/plans_get` | read |
| `agent.team.executions.list` | `agent.team.v1/executions_list` | read |
| `agent.team.executions.get` | `agent.team.v1/executions_get` | read |
| `agent.team.executions.cancel` | `agent.team.v1/executions_cancel` | mutation |

Plan approval keeps using `agent.core.confirmations.get`, `agent.core.confirmations.confirm`, and `agent.core.confirmations.reject`. There is no Team-specific confirmation action.

Agent-to-Product completion uses `product.agent_team.v1/completion_record`. Its body contains only `event_id`, `execution_id`, `task_id`, `conversation_id`, terminal `state`, `result_message_id`, and `completed_at`. It must not contain a result body, AWS coordinates, Worker identifiers, raw errors, tool data, logs, or secrets. Because this callback occurs after the originating user grant has expired, it uses a narrowly hard-coded service-notification branch on the existing Product Capability mTLS connection; it never replays or extends a user delegation.

## File Map

- Modify `p2p/serviceapi/actions.go` and create `p2p/serviceapi/agent_team_action_schema.go` for the four owner actions.
- Modify `internal/agentgateway/catalog_requirements.go`, `catalog.go`, and `result_adapters.go` for `agent.team.v1` discovery and result validation.
- Modify `p2p/internal/agent/module.go` to route the four actions through the existing external Agent gateway.
- Create `internal/productcapability/agent_team.go` and modify `internal/productcapability/mcp.go`, `server.go`, and `handlers.go` for the asynchronous completion capability and peer-authenticated service notification.
- Modify `p2p/product_capability.go` and `p2p/storage/storage_migrations.go`; create `p2p/storage/agent_team_receipts.go` for durable dedupe and ProductCore invalidation events.
- Regenerate `docs/product-action-contract.json` and update `docs/agent-core-integration-development-contract.md`.

## Mandatory Reuse Gate

The reviewed Message Server migration source is the read-only worktree
`/Users/liyanan/Documents/Dirextalk项目监控分析/repos/dirextalk-message-server-central-agent`
at `7f3aa36d03190ee81ebb61381036123bf6c757a9`.

Port compatible validators, projection code, fixtures, and replay tests from:

- `p2p/internal/agentgrpc/team_actions.go` and
  `team_actions_test.go`: strict Plan/execution/result/artifact mapping,
  owner binding, malformed response rejection, and secret-safe errors;
- `p2p/internal/agentcompletion/relay.go` and `relay_test.go`: exact
  execution/conversation correlation, cursor replay, dedupe, restart, and
  terminal-event validation;
- `p2p/serviceapi/actions*`: existing public action names, owner-only
  registration, and transport tests; and
- Agent event-cursor storage tests: replay and monotonic persistence behavior.

Rewire those behaviors to `agentgateway` and
`product.agent_team.v1/completion_record`. Do not port the old direct
`TeamPlanService` client, Team-specific approval device/prepare/approve
actions, synchronous Agent polling relay, local Agent history, or full Team
execution storage. The new minimal receipt and ProductCore invalidation event
remain the only Message Server durable Team facts.

### Task 1: Register The Four ProductCore Actions And Strict Schemas

**Files:**
- Modify: `p2p/serviceapi/actions.go`
- Create: `p2p/serviceapi/agent_team_action_schema.go`
- Create: `p2p/serviceapi/agent_team_action_schema_test.go`
- Modify: `p2p/serviceapi/actions_test.go`

- [ ] **Step 1: Write failing action registration tests**

Add table-driven assertions that all four actions are owner-authenticated, available over HTTP and request WebSocket, have non-nil schemas, reject unknown fields, and require lowercase canonical UUIDs.

```go
for _, action := range []string{
	"agent.team.plans.get",
	"agent.team.executions.list",
	"agent.team.executions.get",
	"agent.team.executions.cancel",
} {
	spec, ok := ActionSpecFor(action)
	if !ok || spec.Auth != ActionAuthOwner || spec.Transport != ActionTransportHTTPAndWS || spec.Schema == nil {
		t.Fatalf("invalid Team action spec for %s: %#v", action, spec)
	}
}
```

Run:

```bash
GOWORK=off go test ./p2p/serviceapi -run 'TestTeam'
```

Expected: FAIL because the actions and schema helpers do not exist.

- [ ] **Step 2: Implement exact request schemas**

Create schemas with `additionalProperties: false` semantics matching the existing schema builder:

```text
plans.get:       plan_id
executions.list: page_size?, page_token?, states?
executions.get:  execution_id
executions.cancel: execution_id, expected_revision
```

Limit `page_size` to `1..100`; limit `states` to the public Team execution states published by Agent; require `expected_revision >= 1` for cancel.

- [ ] **Step 3: Register the actions beside existing Core Task actions**

Keep the action order stable and do not add plan creation or direct start actions. Plan preparation remains initiated by Agent chat and returns a generic Core confirmation.

- [ ] **Step 4: Run focused tests**

```bash
GOWORK=off go test ./p2p/serviceapi -run 'TestTeam|TestActionSpecs'
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add p2p/serviceapi
git commit -m "feat: define ProductCore Team actions"
```

### Task 2: Bind `agent.team.v1` In The External Agent Gateway

**Files:**
- Modify: `internal/agentgateway/catalog_requirements.go`
- Modify: `internal/agentgateway/catalog.go`
- Modify: `internal/agentgateway/runner.go`
- Modify: `internal/agentgateway/result_adapters.go`
- Modify: `internal/agentgateway/catalog_test.go`
- Modify: `internal/agentgateway/runner_test.go`
- Modify: `internal/agentgateway/result_adapters_test.go`

- [ ] **Step 1: Write failing binding and readiness tests**

Assert the exact mapping and that Team client capability tokens are omitted unless all four operations are present with canonical input/result schemas and matching digests.

```go
want := map[string]string{
	"agent.team.plans.get": "plans_get",
	"agent.team.executions.list": "executions_list",
	"agent.team.executions.get": "executions_get",
	"agent.team.executions.cancel": "executions_cancel",
}
for action, operation := range want {
	binding, ok := actionBindingFor(action)
	if !ok || binding.capabilityID != "agent.team.v1" || binding.operation != operation {
		t.Fatalf("unexpected binding for %s: %#v", action, binding)
	}
}
```

Run:

```bash
GOWORK=off go test ./internal/agentgateway -run 'TestTeam'
```

Expected: FAIL because no Team binding exists.

- [ ] **Step 2: Add the catalog requirements**

Pin `agent.team.v1` semantic/protocol version and all four operation schemas. Project the client tokens `team`, `team.plan`, and `team.execution` only from a currently valid catalog lease. Do not infer readiness from configuration or from `agent.execution.v2`.

- [ ] **Step 3: Add request and result adapters**

Forward server-derived owner and account generation through the existing grant path. Preserve `confirmation_id`, `task_id`, `plan`, `execution`, `items`, and `next_page_token`; strip unknown internal top-level values and reject secret-shaped fields recursively.

- [ ] **Step 4: Verify fail-closed behavior**

Tests must cover missing operation, stale digest, malformed result, catalog lease expiry, account-generation replacement, and an upstream error containing a fake secret. Public errors must remain structured and sanitized.

Run:

```bash
GOWORK=off go test ./internal/agentgateway
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agentgateway
git commit -m "feat: map Team capability through Agent gateway"
```

### Task 3: Route Team Actions Through The Existing Agent Module

**Files:**
- Modify: `p2p/internal/agent/module.go`
- Modify: `p2p/internal/agent/module_test.go`
- Modify: `p2p/action_registry_test.go`

- [ ] **Step 1: Write failing routing tests**

Use a fake `Runner` to assert each Team action is registered exactly once, validated before invocation, forwarded under the same action name, and returns the adapted result. Also assert an unknown `agent.team.*` action is not registered.

Run:

```bash
GOWORK=off go test ./p2p/internal/agent ./p2p -run 'Test.*Team'
```

Expected: FAIL because the module omits Team actions.

- [ ] **Step 2: Add the actions to `externalNativeActions`**

Use the existing `m.invoke(action)` path. Do not add a local Team coordinator, local result store, direct Agent address, or fallback runtime.

- [ ] **Step 3: Verify owner-only routing and sanitized errors**

```bash
GOWORK=off go test ./p2p/internal/agent ./p2p/serviceapi ./internal/agentgateway
```

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add p2p/internal/agent p2p/action_registry_test.go
git commit -m "feat: route Team actions to Agent Core"
```

### Task 4: Add The Asynchronous Product Completion Capability

**Files:**
- Create: `internal/productcapability/agent_team.go`
- Create: `internal/productcapability/agent_team_test.go`
- Modify: `internal/productcapability/mcp.go`
- Modify: `internal/productcapability/mcp_test.go`
- Modify: `internal/productcapability/server.go`
- Modify: `internal/productcapability/handlers.go`
- Modify: `internal/productcapability/delegation_test.go`
- Modify: `setup/monolith.go`

- [ ] **Step 1: Write failing descriptor tests**

Require one mutation operation named `completion_record` on `product.agent_team.v1`, audience Native Agent, scope `product:agent_team:complete`, permanent idempotency, canonical schemas, and no result body beyond the accepted event identifier. Prove this one operation accepts the authenticated Agent service path without a live user `PermissionContext`, while every other Product operation still rejects that path.

```go
operation, ok := registry.Operation("product.agent_team.v1", "completion_record")
if !ok || operation.GetOperationType() != capv1.OperationType_OPERATION_TYPE_MUTATION {
	t.Fatalf("Team completion capability is unavailable: %#v", operation)
}
```

Run:

```bash
GOWORK=off go test ./internal/productcapability -run 'TestAgentTeam'
```

Expected: FAIL because the descriptor is absent.

- [ ] **Step 2: Define the canonical completion schema**

Accept only:

```json
{
  "completed_at": "2026-08-06T10:00:00Z",
  "conversation_id": "00000000-0000-0000-0000-000000000000",
  "event_id": "00000000-0000-0000-0000-000000000000",
  "execution_id": "00000000-0000-0000-0000-000000000000",
  "result_message_id": "00000000-0000-0000-0000-000000000000",
  "state": "succeeded",
  "task_id": "00000000-0000-0000-0000-000000000000"
}
```

Terminal `state` is one of `succeeded`, `failed`, or `cancelled`. All IDs are canonical lowercase UUIDs; `completed_at` is RFC3339 UTC. Reject extra fields recursively before invoking ProductCore.

- [ ] **Step 3: Register it in the checked production registry**

Bind it to a fixed internal ProductCore action `agent.team.completion.record`, not an arbitrary caller-provided action. Registration failure must abort Product Capability server startup rather than publish a partial catalog.

- [ ] **Step 4: Implement the narrow service-notification authentication branch**

Add `ExpectedOwnerID` to Product Capability configuration and populate it from `service.OwnerMXID()` during monolith composition. For `StartOperation` only when capability and operation are exactly `product.agent_team.v1/completion_record`:

- require the existing interceptor to authenticate mTLS client CN, direction token, Agent instance ID, account generation, readiness, and valid `agent -> product` call route;
- reject a caller-supplied owner, scope, action, or non-empty user `PermissionContext`;
- derive owner and generation from immutable server configuration;
- validate the exact canonical input schema and request digest;
- use `event_id` as the durable operation ID and preserve the existing operation ledger replay/conflict rules;
- invoke only the fixed completion handler.

Keep this branch in a dedicated helper such as `startServiceNotification`; generic `Query`, `StartOperation`, delegation exchange, and all control RPCs continue to require current signed grants.

- [ ] **Step 5: Test protocol fences**

Cover wrong/missing client certificate, token, peer instance, owner configuration, account generation, route, capability, operation, operation ID, canonical request digest, changed replay payload, and payload limits. Also prove an expired captured user grant cannot authorize completion and the service path cannot invoke contacts/messages or any arbitrary action.

Run:

```bash
GOWORK=off go test ./internal/productcapability
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/productcapability setup/monolith.go
git commit -m "feat: accept durable Agent Team completions"
```

### Task 5: Persist Minimal Completion Receipts And Emit Invalidation Events

**Files:**
- Modify: `p2p/storage/storage_migrations.go`
- Create: `p2p/storage/agent_team_receipts.go`
- Create: `p2p/storage/agent_team_receipts_test.go`
- Modify: `p2p/product_capability.go`
- Create: `p2p/product_capability_agent_team_test.go`

- [ ] **Step 1: Write failing store tests**

Require a transaction that inserts one receipt and one `p2p_events` row. Replaying the same `event_id` returns the original receipt without allocating another event sequence; reusing an `event_id` with a different canonical payload returns a conflict.

The new table contains only:

```sql
event_id TEXT PRIMARY KEY,
owner_id TEXT NOT NULL,
account_generation BIGINT NOT NULL,
execution_id TEXT NOT NULL,
task_id TEXT NOT NULL,
conversation_id TEXT NOT NULL,
result_message_id TEXT NOT NULL,
terminal_state TEXT NOT NULL,
payload_digest BYTEA NOT NULL,
completed_at TIMESTAMPTZ NOT NULL,
created_at TIMESTAMPTZ NOT NULL
```

Run:

```bash
GOWORK=off go test ./p2p/storage -run 'TestAgentTeamReceipt'
```

Expected: FAIL because the store is absent.

- [ ] **Step 2: Add the ordered ProductCore migration**

Add the table and indexes through `storage_migrations.go`. Do not create Agent Plan, execution, Worker, resource, progress, result, or conversation tables.

- [ ] **Step 3: Implement transactional dedupe and invalidation**

Emit one P2P event:

```json
{
  "type": "agent.team.execution.completed",
  "execution_id": "...",
  "conversation_id": "...",
  "result_message_id": "...",
  "state": "succeeded",
  "completed_at": "..."
}
```

Use `dedupe_key = "agent-team-completion:" + event_id`. The event is a cache invalidation hint; it contains no final answer. Flutter must read the Agent-owned conversation and execution projection after receiving it.

- [ ] **Step 4: Bind the internal ProductCore action**

`InvokeProductCapability` recognizes only the fixed internal action, derives owner from `Service.OwnerMXID()` and account generation from the authenticated Product Capability context, calls the receipt store, and returns only `{"event_id":"...","accepted":true}`. It must never trust identity from params or invoke the Agent gateway.

- [ ] **Step 5: Test concurrent replay and restart durability**

Run:

```bash
GOWORK=off go test -race ./p2p/storage ./p2p -run 'Test.*AgentTeam'
```

Expected: PASS with one receipt and one event under concurrent identical delivery.

- [ ] **Step 6: Commit**

```bash
git add p2p/storage p2p/product_capability.go p2p/product_capability_agent_team_test.go
git commit -m "feat: publish Team completion invalidations"
```

### Task 6: Regenerate Contracts And Verify The Message Server Boundary

**Files:**
- Modify: `docs/product-action-contract.json`
- Modify: `docs/agent-core-integration-development-contract.md`
- Modify: `docs/current-project-documentation.md`
- Modify: `p2p/serviceapi/action_contract_test.go`

- [ ] **Step 1: Regenerate the public ProductCore action contract**

Run:

```bash
GOWORK=off go run ./cmd/dirextalk-action-contract > /tmp/product-action-contract.json
cp /tmp/product-action-contract.json docs/product-action-contract.json
```

Expected: the generated artifact includes exactly the four public Team actions and excludes `agent.team.completion.record` because that action is internal-only.

- [ ] **Step 2: Update ownership documentation**

Document `agent.team.v1`, the `team`, `team.plan`, and `team.execution` discovery tokens, generic Core confirmation reuse, asynchronous `product.agent_team.v1`, the narrowly fixed service-notification authentication exception, and the minimal-receipt rule. State explicitly that the exception cannot call any other Product capability and does not weaken user-delegated operations.

- [ ] **Step 3: Run focused and full verification**

```bash
GOWORK=off go test ./internal/agentgateway ./internal/productcapability ./p2p/internal/agent ./p2p/serviceapi ./p2p/storage ./p2p
GOWORK=off go test ./...
GOWORK=off go vet ./...
git diff --check
```

Expected: PASS. The repository must contain no new model/AWS key handling, Team execution history, Worker status history, or direct App-to-Agent endpoint.

- [ ] **Step 4: Commit**

```bash
git add docs p2p/serviceapi/action_contract_test.go
git commit -m "docs: publish Team facade contract"
```

## Message Server Release Gate

Deploy only with an Agent image that publishes the pinned `agent.team.v1` descriptor and with matching bidirectional Capability identities. Acceptance must prove: owner request routing, generic confirmation approval, terminal completion replay dedupe, realtime invalidation, Agent-owned conversation readback, account-generation fencing, and zero Team execution history stored in Message Server.
