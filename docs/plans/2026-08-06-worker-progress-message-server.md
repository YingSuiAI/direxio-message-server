# Worker Progress Message Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose owner-scoped Agent execution list/get progress through ProductCore without creating progress messages, realtime milestones, or a second database projection.

**Architecture:** The remote Agent gRPC adapter validates the new list/get response field by field and returns bounded JSON only to the requesting owner. The existing completion relay keeps its v1 payload unchanged by continuing to use the base execution mapper, while query-only detail mapping lives in a focused file.

**Tech Stack:** Go, gRPC generated Agent client, ProductCore action registry, strict map-based JSON contract tests.

---

## File Map

- Modify `go.mod`/`go.sum` only after the Agent progress protocol commit is available.
- Modify `p2p/serviceapi/actions.go` and tests to register `agent.team.executions.list` as owner read action.
- Modify `p2p/internal/agent/module.go` and tests to route the list action to remote Agent.
- Modify `p2p/internal/agentgrpc/team_actions.go`; create `team_execution_details.go` and tests for strict list/get mapping.
- Extend shared gRPC fixtures in `team_actions_test.go`; freeze completion payload in `team_completion_test.go`.
- Regenerate `docs/product-action-contract.json`; update current documentation and API change record after tests pass.

### Task 1: Adopt The Real Agent gRPC Contract

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: Verify the current dependency lacks List**

Run: `go doc github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1.TeamPlanServiceClient`

Expected before upgrade: `ListTeamExecutionsV3` is absent.

- [ ] **Step 2: Update to the exact Agent progress commit**

Use the committed/pushed Agent module revision that contains `ListTeamExecutionsV3` and `TeamExecutionV3.progress`. Do not add local `replace` directives to the committed module.

- [ ] **Step 3: Verify generated names and compile gate**

Run:

```bash
go doc github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1.TeamPlanServiceClient
go test ./p2p/internal/agentgrpc -run '^$' -count=1
```

Expected: documentation includes List/Get; package compiles.

- [ ] **Step 4: Commit dependency adoption**

```bash
git add go.mod go.sum
git commit -m "build: adopt Agent progress contract"
```

### Task 2: Implement Owner-Bound Paged List Mapping

**Files:**
- Modify: `p2p/internal/agentgrpc/team_actions.go`
- Create: `p2p/internal/agentgrpc/team_execution_details.go`
- Create: `p2p/internal/agentgrpc/team_execution_details_test.go`
- Modify: `p2p/internal/agentgrpc/team_actions_test.go`

- [ ] **Step 1: Add failing list tests and fixture method**

Add `ListTeamExecutionsV3` to the shared `teamTestService`, capture the request, and cover valid active/history pages, empty list, page size `1..50`, page token forwarding, unknown params, unbound Owner/Execution, duplicate IDs, invalid timestamps/enums/counts, and cyclic next token.

- [ ] **Step 2: Verify tests fail**

Run: `go test ./p2p/internal/agentgrpc -run '^(TestRunnerListsOwnerBoundTeamExecutions|TestRunnerRejectsInvalidTeamExecutionListParamsBeforeRPC|TestRunnerRejectsInvalidTeamExecutionListResponse)$' -count=1`

Expected: FAIL because the action and mapper are missing.

- [ ] **Step 3: Add the list dispatch**

Accept only this input:

```go
if err := allowTeamActionParams(params, "scope", "page_size", "page_token"); err != nil {
	return nil, err
}
```

Map `scope` only from `active|history`, inject `runner.ownerID` into gRPC, default page size to 20, and reject values outside `1..50`. Return:

```go
map[string]any{
	"executions":      executions,
	"next_page_token": response.GetNextPageToken(),
}
```

Collections must be non-null empty slices. Every summary is strictly mapped in `team_execution_details.go`.

- [ ] **Step 4: Run focused tests and commit**

Run: `gofmt -w p2p/internal/agentgrpc/team_actions.go p2p/internal/agentgrpc/team_execution_details.go p2p/internal/agentgrpc/team_execution_details_test.go p2p/internal/agentgrpc/team_actions_test.go && go test ./p2p/internal/agentgrpc -run 'TeamExecutionList|ListsOwnerBound' -count=1`

Expected: PASS.

```bash
git add p2p/internal/agentgrpc/team_actions.go p2p/internal/agentgrpc/team_execution_details.go p2p/internal/agentgrpc/team_execution_details_test.go p2p/internal/agentgrpc/team_actions_test.go
git commit -m "feat: list owner Agent runs"
```

### Task 3: Add Query-Only Progress Detail And Freeze Completion

**Files:**
- Modify: `p2p/internal/agentgrpc/team_execution_details.go`
- Modify: `p2p/internal/agentgrpc/team_execution_details_test.go`
- Modify: `p2p/internal/agentgrpc/team_actions.go`
- Modify: `p2p/internal/agentgrpc/team_completion_test.go`

- [ ] **Step 1: Add failing strict progress tests**

Cover all closed stages/health/failure enums, maximum 8 roles, maximum 50 timeline entries per role, unique role IDs, monotonic sequence/timestamps, role runtime bindings, execution updated-time bounds, and forbidden internal fields. Require `team.execution.completed` payload to remain byte-shape compatible without a `progress` key.

- [ ] **Step 2: Verify tests fail**

Run: `go test ./p2p/internal/agentgrpc -run '^(TestRunnerGetsBoundedTeamExecutionProgress|TestRunnerRejectsInvalidTeamExecutionProgress|TestRunnerStreamsReportBoundTeamCompletion)$' -count=1`

Expected: FAIL until query-only detail mapping exists.

- [ ] **Step 3: Keep base and detail mappers separate**

Keep `mapTeamExecution` unchanged for completion. In `getTeamExecution`, append the validated progress map only after base mapping:

```go
execution, err := runner.mapTeamExecution(response.GetExecution())
if err != nil { return nil, err }
progress, err := mapTeamExecutionProgress(response.GetExecution().GetProgress())
if err != nil { return nil, invalidTeamExecutionResponse() }
execution["progress"] = progress
return execution, nil
```

The progress mapper emits only schema/stage/health/counts/timestamps, role metadata, fixed failure enums, and bounded public timeline entries.

- [ ] **Step 4: Run completion and adapter regressions, then commit**

Run:

```bash
go test ./p2p/internal/agentgrpc -run 'TeamExecutionProgress|TeamCompletion' -count=1
go test ./p2p/internal/agentcompletion -count=1
```

Expected: PASS; completion payload remains v1-compatible.

```bash
git add p2p/internal/agentgrpc/team_execution_details.go p2p/internal/agentgrpc/team_execution_details_test.go p2p/internal/agentgrpc/team_actions.go p2p/internal/agentgrpc/team_completion_test.go
git commit -m "feat: map bounded Agent run details"
```

### Task 4: Register The Public Action And Complete Verification

**Files:**
- Modify: `p2p/serviceapi/actions.go`
- Modify: `p2p/serviceapi/actions_test.go`
- Modify: `p2p/internal/agent/module.go`
- Modify: `p2p/internal/agent/module_test.go`
- Regenerate: `docs/product-action-contract.json`
- Modify: `docs/current-project-documentation.md`
- Modify: `docs/api-interface-change-record.md`

- [ ] **Step 1: Add failing registry/routing tests**

Require `agent.team.executions.list` to be `owner + http_and_ws_request`, included in remote Agent handlers, and routed by the existing `agent.team.*` runner. Update expected action count from 171 to 172 only in the same change.

- [ ] **Step 2: Verify tests fail**

Run: `go test ./p2p/serviceapi ./p2p/internal/agent -run 'Action|Team' -count=1`

Expected: FAIL because the public action is not registered.

- [ ] **Step 3: Register action and regenerate contract**

Add `AgentTeamExecutionsListAction = "agent.team.executions.list"`, its owner metadata, and the remote handler. Then run: `go run ./cmd/dirextalk-action-contract > docs/product-action-contract.json`

- [ ] **Step 4: Run focused and broad checks**

Run each command separately:

```bash
go test ./p2p/serviceapi ./p2p/internal/agent ./p2p/internal/agentgrpc ./p2p/internal/agentcompletion -count=1
go test ./p2p -run '^(TestActionRegistryCoversPublicAndAgentActions|TestActionMetadataCoversRegistryAndDerivesClassifications)$' -count=1
go vet ./p2p/internal/agentgrpc ./p2p/internal/agentcompletion
go build ./cmd/dirextalk-message-server
git diff --check
```

Expected: PASS. No database migration, Matrix message, ProductCore realtime event, or MCP tool is added.

- [ ] **Step 5: Update docs with observed evidence and commit**

Record list/get, owner scope, strict exclusions, unchanged completion event, and exact checks.

```bash
git add p2p/serviceapi/actions.go p2p/serviceapi/actions_test.go p2p/internal/agent/module.go p2p/internal/agent/module_test.go docs/product-action-contract.json docs/current-project-documentation.md docs/api-interface-change-record.md
git commit -m "feat: expose Agent runs query action"
```
