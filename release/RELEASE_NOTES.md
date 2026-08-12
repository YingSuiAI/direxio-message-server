# Dirextalk Message Server release notes

## Unreleased

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
