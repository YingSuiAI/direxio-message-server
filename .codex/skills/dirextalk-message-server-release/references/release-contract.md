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
suite, the production build, Compose validation, and an image version probe.
The built image labels must bind the requested version, reviewed commit, and
commit timestamp. Verification evidence is canonical JSON bound to those same
values. Publication uses buildx to push a provenance- and SBOM-bearing OCI
index with exactly one real platform, `linux/amd64`; buildx attestation
descriptors with platform `unknown/unknown` do not count as runtime platforms.

No release manifest, release index, checksum, predecessor asset, or offline
attestation is generated, downloaded, uploaded, or consulted.

## Publication order

1. Pass repository tests, build the local version image, and probe its metadata
   and embedded version.
2. Push `dirextalk/message-server:vX.Y.Z` as an OCI index, inspect and record
   its canonical digest and exact `linux/amd64` platform set, pull that platform
   back, and probe its labels, revision, timestamp, and embedded version again.
3. Create or verify the annotated Git tag and matching formal GitHub Release
   using the checked-in title and release notes. The Release has no assets.
4. Only after the formal Release succeeds, create
   `dirextalk/message-server:latest` from the immutable version-index digest.
   Require both tags to resolve to the same OCI index digest, then pull the
   `linux/amd64` platform and probe its metadata and embedded version.

The repository-owned Agent release scripts use the same buildx OCI index,
platform, digest, label, and pull-back rules. They additionally require all
three Agent binaries to report the requested version. Their `latest` tag moves
only after the version index and all three binary probes pass.

The scripts require a clean `main` whose `HEAD` equals `origin/main`. An
existing version tag must already resolve to the same reviewed commit; the
scripts check the remote tag before moving either image tag and never move a tag
that belongs to another commit. An existing formal Release must exactly match
the checked-in title and notes and contain no assets.

An explicitly authorized same-version replacement preserves the old tag,
Release, and image evidence outside the repository, deletes both the old formal
Release and remote tag, and then runs the normal scripts from the new reviewed
commit. The script does not require a version bump after that external cleanup.
