# Dirextalk Message Server release notes

## Unreleased

## v1.1.29

1. Complete the Agent Core MCP and Skills lifecycle with strict, separated ProductCore schemas for built-in, GitHub, skills.sh, registry, and managed Node/npm sources.
2. Add immutable single-file static-site publication behind the node-owned `/.sites` route without exposing host paths or private artifact metadata.
3. Stream durable Native Agent turns without a fixed model/tool round limit, preserve progress and terminal identity across reconnects, and keep ambiguous mutations fail-closed.
4. Enforce three concurrent local sandbox slots, bounded Node artifact quotas, script-disabled offline installs, and receipt-bound runner isolation.
5. Publish only redacted extension receipts and complete version-level network and secret grant projections for supported clients.

## v1.1.6

1. Inject one deployment-owned release catalog origin and derive the fixed Server and Agent channels from it.
2. Make Server and Agent release changes forward-only, with infrastructure failures entering maintenance and unreferenced old images removed after the new receipt commits.
3. Preserve isolated runner socket permissions and delegated cgroup controllers across first start and release changes.

## v1.1.5

1. Propagate durable Agent operation sequence cursors to Native Agent WebSocket frames so clients can resume streams with a real `after_seq` cursor.

## v1.1.4

1. Keep durable Native Agent execution running when a client detaches its WebSocket stream; only an explicit turn-stop action cancels execution.

## v1.1.3

1. Unify Message Server and Agent updates behind the single `release.v2` status, apply, ticket, and recovery contract.
2. Authorize Agent targets from the fixed central `agents` channel and enforce its minimum stable Message Server version at both the server and root-owned updater boundaries.
3. Commit Agent image provenance, runtime receipts, and the three-container topology in one rollback-safe update transaction.

## v1.1.1

1. Add Native Gemini model listing, chat, and streaming support for Gemini-native endpoints.
2. Return safe, redacted Provider and runtime errors to Native Agent clients.

## v1.1.0

1. Require Native Agent model and provider profiles to be scoped to each request.
2. Normalize versioned Provider API endpoints and raise the Anthropic output-token limit.

## v1.0.9

1. Reject direct messages involving blocked users across Matrix client and federation paths.
2. Enforce the same blocked direct-message rejection through ProductCore actions.

## v1.0.8

1. Add durable Native Agent turns that can reconnect without duplicate execution.
2. Add owner controls to list and stop durable Agent turns safely.
3. Mark unfinished Agent turns interrupted after a server restart.

## v1.0.7

1. Authorize server targets exclusively through the central version record.
2. Allow a node on any older canonical server version to install the centrally
   authorized target while retaining backup and rollback protection.
3. Simplify publication to the version image, Git tag, release notes, formal
   GitHub Release, and `latest` tag.

## v1.0.6

1. Require current Matrix `join` membership for MCP room discovery and room-scoped actions.
2. Make channel post favorites converge from Matrix reactions across all members.
3. Restore authoritative room-owner detection from Matrix creation events.
4. Keep member and avatar lists stable with exact-creator and confirmed-join-time ordering.

## v1.0.5

1. Add owner-only `release.v2.status` for safe updater readiness and progress checks.
2. Add owner-only `release.v2.apply` for centrally validated direct upgrades.
3. Keep central release records constrained to canonical versions and safe updater fields.

## v1.0.4

1. Establish a fresh stable release baseline.
2. Add metadata-only unread recovery snapshots for new devices.
3. Keep read-marker ordering server-authoritative across retries, restarts, and concurrent updates.

## v1.0.3

1. Make group joins, contact decisions, and channel approvals recoverable after retries and restarts.
2. Add durable operation leases to prevent duplicate concurrent actions.
3. Add optional recovery status fields to ProductCore HTTP and WebSocket responses.

## v1.0.2

Server schema, updater API, Product actions, and client compatibility remain
unchanged.

## v1.0.1

This security patch updates `golang.org/x/crypto` to `v0.52.0`. It does not
change the server schema, updater API, Product action contract, or supported
client-version range.

## v1.0.0

This is the first formal, immutable server release. The release version is
reported as `v1.0.0`; its source commit and build time remain separate build
metadata.

### Compatibility

- Server schema version: `1`.
- Oldest readable server schema version: `1`.
- Client compatibility is declared by each checked-in release configuration using
  an inclusive minimum and exclusive maximum version.
- The central server version record is the only authority for selecting an
  upgrade target; repository release metadata does not constrain the source
  server version.

### Forward-only updates

Server and Agent release changes never reactivate an older release. A failure
after activation starts enters maintenance and requires explicit intervention.
After health and receipt commit, the protected wrapper removes the previous
image only when no other container still references it; it never globally
prunes or force-removes shared images.

### Publishing

The source version, Docker version tag, release-notes section, Git tag, and
GitHub Release tag must all be identical. The formal GitHub Release carries the
release notes and no assets.

Run the project-local `dirextalk-message-server-release` Skill and
`scripts/release/{prepare,verify,publish}.sh`. The scripts publish and probe the
version image, create or verify the GitHub Release, and only then update
`latest`.
