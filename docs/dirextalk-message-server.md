# Dirextalk Message Server Image

This backend image packages the merged Matrix and P2P AS service.

## Image

Docker Hub repository:

```bash
dirextalk/message-server:latest
```

Release tags:

```bash
dirextalk/message-server:latest
dirextalk/message-server:vX.Y.Z
```

Production releases use the repository-owned release workflow. From a clean,
pushed `main` whose source version, release config, and release notes all name
the same target, run:

```bash
bash scripts/release/prepare.sh vX.Y.Z
bash scripts/release/verify.sh vX.Y.Z
bash scripts/release/publish.sh vX.Y.Z
```

The scripts run tests and image probes, publish the version tag and formal
GitHub Release, and only then update `latest`. They do not publish updater
metadata assets or constrain which older server version may install the
centrally authorized target.

For a local-only source rebuild, leave `MESSAGE_SERVER_IMAGE` unset. The
Compose files keep the backend service tagged as `dirextalk/message-server:latest`
and may build it from the checked-out source:

```bash
unset MESSAGE_SERVER_IMAGE
docker compose -f docker-compose.p2p.yml up -d --build message-server
```

For a deployed image, set `MESSAGE_SERVER_IMAGE` to the complete immutable
registry digest. Pull the digest explicitly, then use `--no-build`; never use
`--build` on this path:

```bash
export MESSAGE_SERVER_IMAGE=dirextalk/message-server@sha256:<64-hex-digest>
docker compose -f docker-compose.p2p.yml pull message-server-init message-server
docker compose -f docker-compose.p2p.yml up -d --no-build message-server
```

The dual-node regression stack uses the same image selector. Pull all six
Message Server services before starting the three nodes without a build:

```bash
export MESSAGE_SERVER_IMAGE=dirextalk/message-server@sha256:<64-hex-digest>
docker compose -f docker-compose.p2p-dual.yml pull \
  dendrite-a-init dendrite-b-init dendrite-c-init dendrite-a dendrite-b dendrite-c
docker compose -f docker-compose.p2p-dual.yml up -d --no-build dendrite-a dendrite-b dendrite-c
```

Before pushing a new `latest`, run the multi-node regression against the rebuilt image.

PowerShell:

```powershell
$env:P2P_DUAL_PUBLIC_HOST = if ($env:P2P_DUAL_PUBLIC_HOST) { $env:P2P_DUAL_PUBLIC_HOST } else { "host.docker.internal" }
docker compose -f docker-compose.p2p-dual.yml up -d --force-recreate dendrite-a dendrite-b dendrite-c
python scripts/p2p-three-node-regression.py
```

Bash:

```bash
export P2P_DUAL_PUBLIC_HOST="${P2P_DUAL_PUBLIC_HOST:-host.docker.internal}"
docker compose -f docker-compose.p2p-dual.yml up -d --force-recreate dendrite-a dendrite-b dendrite-c
python3 scripts/p2p-three-node-regression.py
```

## Runtime Notes

- PostgreSQL is expected to run as `postgres:18`.
- The default compose path for local credentials is `/var/dirextalk-message-server/p2p/bootstrap.json`.
- The credentials file contains `password`, unified `access_token`, `agent_token`, and `device_id`.
- If `P2P_PORTAL_PASSWORD` is not set, a new portal initializes with an 8 digit numeric password and writes it to the credentials file.
- Remote public channel lookup requires the client request to include `remote_node_base_url`, for example `https://dendrite-b:8448/_p2p`.
- `P2P_REMOTE_NODE_INSECURE_SKIP_TLS_VERIFY=true` is intended only for trusted local self-signed test nodes.
