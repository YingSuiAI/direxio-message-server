---
name: dirextalk-message-server-release
description: Use when preparing, verifying, publishing, retrying, or auditing a stable Dirextalk message-server version image and GitHub Release.
---

# Dirextalk Message Server Release

Use the repository-owned release scripts; do not reconstruct the Git tag,
Docker image, GitHub Release, or `latest` update by hand.

## Prepare

1. Read `references/release-contract.md`, the target `release/vX.Y.Z.json`, and
   the current release scripts.
2. Separate inspection and preparation from external publication. Pushing
   commits, tags, images, or creating Releases requires explicit authorization.
3. Keep the canonical source version, release notes, client compatibility, and
   schema metadata aligned in one reviewed commit.
4. Treat the central `appId=1`, `channelId=server` version record as the only
   upgrade authorization. Repository release metadata does not declare source
   versions, image identities, or upgrade paths.

## Verify

Run:

```text
bash scripts/release/contract_test.sh
go test ./internal/releasecontrol ./internal/httputil ./setup ./p2p ./internal/productpolicy -count=1
go test -tags=dendrite_upgrade_tests ./cmd/dendrite-upgrade-tests -count=1
go build ./cmd/dirextalk-message-server
git diff --check
```

Then use the scripts in order:

```text
bash scripts/release/prepare.sh vX.Y.Z
bash scripts/release/verify.sh vX.Y.Z
bash scripts/release/publish.sh vX.Y.Z
```

`publish.sh` is authorized only after the tree is clean, `HEAD` equals the
reviewed `origin/main`, and `verify.sh` produced commit-bound evidence. Fix a
faulty gate in code; never bypass it.

## Stop And Report

Stop when version, notes, compatibility metadata, schema metadata, image
labels, image version output, or Git tag disagree. Resolve any conflicting tag
before retrying the release.

`latest` must move only after the version image and formal GitHub Release are
published successfully.

After publication, pull `latest` and verify its version/revision labels and
embedded binary version. Record non-secret results and any retryable partial
state.
