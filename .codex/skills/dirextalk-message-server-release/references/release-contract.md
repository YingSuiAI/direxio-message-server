# Stable release contract

The formal version is canonical `vX.Y.Z`. A release uses one reviewed Git
commit, its RFC3339 commit timestamp, one canonical Docker version tag, one
annotated Git tag, and one matching formal GitHub Release.

## Required repository metadata

- `internal/version.go`: target default version and schema constants.
- `release/RELEASE_NOTES.md`: matching `## vX.Y.Z` section.
- `release/vX.Y.Z.json`: target version, client compatibility bounds, and
  schema compatibility metadata.

Release metadata never names a predecessor, upgrade path, image identity, or
offline evidence. A centrally published `appId=1`, `channelId=server` version is
the complete authorization for a node to request that canonical target.

## Verification

The release gate runs the affected Go packages, the retained-data migration
suite, the production build, Compose validation, a binary version probe, and a
running-container `GET /_p2p/health` probe whose `version` must equal the
release target.
The built image labels bind the requested version, reviewed commit, and commit
timestamp. Verification evidence is canonical JSON bound to those values.

No release manifest, release index, checksum, predecessor asset, or offline
attestation is generated, downloaded, uploaded, or consulted.

## Publication order

1. Pass repository tests, build the local version image, and probe its metadata,
   embedded binary version, and running health endpoint version.
2. Push `dirextalk/message-server:vX.Y.Z`, pull it back, and probe its version
   and revision labels, embedded binary version, and running health endpoint
   version again.
3. Create or reuse the same-version Git tag and matching formal GitHub Release.
4. Only after the formal Release succeeds, update
   `dirextalk/message-server:latest` from the version tag, pull `latest`, and
   probe its version/revision labels, embedded binary version, and running
   health endpoint version.

The sibling Agent repository owns Agent publication and its three-binary
version probes. Message Server release automation never publishes Agent images.

The scripts require a clean `main` whose `HEAD` equals `origin/main`. The final
same-version tag must resolve to that reviewed commit.
