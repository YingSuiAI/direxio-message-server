# Dirextalk Message Server release notes

## Unreleased

## v1.1.64

1. Advertise explicit standard MCP tool annotations so clients can distinguish idempotent reads from non-idempotent message and comment writes without relying on tool names or protocol defaults.
2. Adopt `dirextalk-capability-api` v1.1.0 generated Agent data-plane v2 types and shared conformance vectors as the cross-service session response contract.
3. Preserve established Matrix and ProductCore public error messages while tightening internal error handling and full-repository static analysis.

## v1.1.63

1. Return the RFC3339 UTC `server_time` in owner Agent session tickets so clients can derive ticket lifetime from a monotonic clock without trusting the device wall clock.
2. Build the Message Server with Go 1.26.6.

## v1.1.62

1. Remove the retired ProductCore-to-Agent MCP gateway test surface and require current direct Agent HTTP settings during Agent updates instead of migrating legacy gateway configuration.

## v1.1.52

1. Refresh the Agent config material volume after removing the retired AWS key, and rematerialize the original config during rollback.

## v1.1.51

1. Remove the retired `core_aws_enabled` key transactionally during existing-node Agent upgrades and restore the original config if the update fails.

## v1.1.50

1. Remove the superseded Generic Execution V2 product surface and retain only the eight current Cloud Worker read/cancel/artifact operations.
2. Remove deploy-time Agent AWS/SSM configuration and policy artifacts so App-uploaded credentials are the only Worker binding.

## v1.1.49

1. Synchronize the current Execution V2 Cloud Worker result schema digests with the Agent capability catalog.

## v1.1.48

1. Align Cloud Worker plan and run projections with the simplified Agent-owned SSH execution shape.
2. Keep one live proposal quote through confirmation and remove the retired runtime progress projection.

## v1.1.47

1. Remove the legacy node-bound Cloud Worker deployment wiring so Workers use the Agent-owned dynamic SSH execution path.
2. Expose Worker compute capacity, storage, hourly price, and authorized total from the live quote through ProductCore.
3. Recover interrupted Agent update receipts and allow watchdog retries to resume safely after restart.

## v1.1.46

1. Simplify Cloud Worker plan and run projections for reusable, persistent Workers while keeping run and execution identities independent.
2. Replace Cloud Worker confirmation and conversation-reference digest linkage with exact identity, revision, live-quote, and canonical Worker UUID fields.

## v1.1.45

1. Move ProductCore queries and mutations to HTTP and deliver durable product events through owner-authenticated SSE with cursor resume and reset recovery.
2. Move Native Agent text turns to one HTTP admission followed by durable SSE observation, including reconnect without repeating the mutation.
3. Preserve focused-room push suppression through server-expiring Matrix account data after removing the client WebSocket session store.

## v1.1.43

1. Expose owner actions to list, inspect, and destroy persistent AWS Workers.
2. Add optional Route 53 domain binding and removal for Worker services using their ordinary public IPv4 address.

## v1.1.42

1. Accept additive internal memory fields while projecting only the stable public memory contract.
2. Project Agent task identities into the public task envelope so task reads no longer return HTTP 502.

## v1.1.41

1. Keep memory-fact replay transport metadata outside the closed public mutation receipt so successful edits no longer return HTTP 502.
2. Restrict Native Agent room-tool type inputs to the room kinds accepted by ProductCore.

## v1.1.40

1. Accept the Agent's ordinary built-in MCP installation source in owner action schemas, request validation, and task result projection.

## v1.1.39

1. Remove the retired Cloud Worker completion result-message identity from the current Agent callback and ProductCore invalidation.
2. Drop the unused receipt column while keeping central continuation as the assistant-message authority.

## v1.1.38

1. Expose the current Agent static-site release inventory and exact delete action to owner clients.
2. Publish Native Agent room readiness from the live capability catalog while respecting the enabled setting.

## v1.1.37

1. Return the current Agent model-profile identity fields required by the client readback contract.
2. Remove superseded Agent response aliases and keep memory fact mutations on the current capability contract.

## v1.1.36

1. Align the Skills and MCP read-action catalog pins with the current Agent schemas.
2. Restore the Skills management page against Agent v1.0.78.

## v1.1.35

1. Publish exact structured-memory fact update and delete actions.
2. Remove superseded Agent action aliases and legacy Knowledge-memory CRUD.
3. Pin the ProductCore gateway to the current Agent capability schemas.

## v1.1.34

1. Keep the updater receipt as the Agent runtime version authority and treat the central Agent version only as a newer-version comparison target.
2. Configure Agent static-site responses with the node's public HTTPS origin.

## v1.1.33

1. Consume the Message Server and Agent `latest` release channels directly in fresh production deployments.
2. Verify expected source revisions and real binary versions before startup and again in healthy running containers.
3. Remove the superseded application digest and image-attestation deployment path while retaining digest pins for infrastructure images.
