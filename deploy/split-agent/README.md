# Split Agent deployment harness

This directory is the fresh-data deployment boundary for the split architecture:

- Flutter and every public client connect through the canonical public base
  URL. In edge-terminated production, message-server has only internal HTTP
  `:8008` plus a loopback host binding; Caddy owns public HTTPS. Explicit
  direct-TLS modes additionally enable internal `:8448` and its loopback host
  binding (the local acceptance example uses 18008/18448).
- message-server owns Matrix/ProductCore, the public action envelope, the
  external Native Agent facade, and Product Capability on private port 50053.
- dirextalk-agent owns Native Agent Core on private port 9443 and Agent
  Capability on private port 50052.
- Agent and message-server share one PostgreSQL container, cluster, and data
  volume while using distinct non-superuser roles, owned databases, protected
  DSNs, and private database networks. PostgreSQL alone joins both database
  networks under network-specific aliases; neither application joins the
  other's database network or can connect to the other's database. A separate
  protected cluster-admin credential is mounted only into PostgreSQL. Account
  deletion may wipe either application database; this fresh-state harness does
  not attempt a historical migration.
- Native Agent Core is the only service that owns model, Knowledge, memory,
  task, schedule, extension, and workload state. The extension and Core
  workload runners are separate isolated containers, and the default stack
  starts all three Agent runtime containers from one image.
- Agent Knowledge vectors live in the Agent-owned PostgreSQL database through
  pgvector. Agent egress is used only for configured provider HTTPS calls.
- A dedicated non-internal `message_public` bridge is attached only to
  message-server because rootless Docker needs one non-internal bridge for
  host-port forwarding. Agent, database, and capability networks stay
  internal; no other service joins this edge network.
- The canonical production mode is `edge-terminated`: message-server listens
  only on HTTP `:8008`, publishes that listener only on host loopback, and is
  reachable from Caddy only through the dedicated `message_public` bridge.
  Caddy alone owns public ports 80/443 and ACME state; the canonical client URL
  remains `https://<DIREXTALK_MESSAGE_SERVER_NAME>`. No certificate or private
  key is provisioned into message-server in this mode. `local` and `external`
  direct TLS remain explicit test/operator modes through
  `compose.direct-tls.yaml`; their healthcheck probes both internal listeners.
- AWS is deliberately disabled in the baseline. No file here creates or changes
  AWS resources.

The two gRPC directions use the neutral Capability API with mTLS, one exact
direction token per direction, instance/generation metadata, and Ed25519
grants. `message-server-init` creates the private CA and issues the initial
certificates exactly once for each fresh stack. Its CA signing key is kept in
an init-only volume, the grant private key is mounted only in message-server,
and Agent receives only the issued material it requires.

## Fresh local run

Provision a new output directory for every run. Do not reuse a directory or
Compose volume namespace. The provisioner performs a read-only Docker inspect
gate over every derived network/volume and the Compose project label; an
existing resource is a hard failure, even when the output directory is new.
Because the default Compose shape always starts the Agent, extension-runner,
and Core-runner containers, complete the root-owned host preparation in the
section below first and load its generated env file before provisioning.
Each run receives a fresh random account generation shared by message-server,
Agent Capability, and Product Capability peer metadata:

    cd /home/adam/dirextalk/dirextalk-message-server
    export DIREXTALK_MESSAGE_HTTP_BIND=${DIREXTALK_MESSAGE_HTTP_BIND:-18008}
    export DIREXTALK_MESSAGE_HTTPS_BIND=${DIREXTALK_MESSAGE_HTTPS_BIND:-18448}
    # First run the root-owned host preparation shown below, then load its env:
    # sudo /usr/local/libexec/dirextalk/split-agent/scripts/prepare-runner-cgroups.sh "$STACK" > /tmp/dirextalk-runner-cgroups.env
    # set -a; . /tmp/dirextalk-runner-cgroups.env; set +a
    deploy/split-agent/scripts/provision-local.sh \
      /absolute/path/.run/split-20260804 \
      /absolute/path/openrouter.key \
      /absolute/path/embedding.key \
      /absolute/path/tavily.key \
      /absolute/path/portal-password

The second, third, and fourth arguments are provider-key source files. A
missing argument creates a mode 0400 empty placeholder; replace that file
before model, Knowledge, or Web Search acceptance. The optional fifth argument
is a portal-password source: it must be a current-user-owned regular
non-symlink mode 0400 file containing exactly one 8-digit line. When omitted,
provisioning generates an 8-digit decimal initial password from
cryptographically secure random bytes. In either case the resulting mode 0400
`message-portal-password` file is where the provisioner writes the initial
password; the value never enters
argv, environment, `.env`, logs, or stdout. The provisioner also creates
disposable certs/tokens, two isolated PostgreSQL URLs, UUIDs, the non-secret Agent YAML,
and a path-only `.env`.

For a canonical production first provision behind Caddy, set
`DIREXTALK_SPLIT_COMPOSE_MODE=production`,
`DIREXTALK_MESSAGE_TLS_MODE=edge-terminated`, and
`DIREXTALK_MESSAGE_SERVER_NAME`. Do not set either message TLS certificate
source variable. Provisioning records `https://<server-name>` as
`DIREXTALK_MESSAGE_CLIENT_BASE_URL`, creates protected empty certificate/key
placeholders for identity fencing, and fails if direct certificate material is
supplied. Caddy must use `reverse_proxy message-server:8008` on the named
public bridge.

For an explicit direct-TLS provision, set
`DIREXTALK_MESSAGE_TLS_MODE=external`,
`DIREXTALK_MESSAGE_SERVER_NAME`,
`DIREXTALK_MESSAGE_TLS_CERT_SOURCE_FILE`, and
`DIREXTALK_MESSAGE_TLS_KEY_SOURCE_FILE` in the environment. Both TLS source
files must be current-user-owned regular non-symlinks with mode 0400. The
provisioner copies them through descriptor-bound reads, validates the host and
certificate/key pair, and records only the output paths and file identities in
`.env`/`.manifest`; source paths and key material are not emitted. External
clients use `https://<DIREXTALK_MESSAGE_SERVER_NAME>`. Local mode keeps the
client URL at `http://localhost:<DIREXTALK_MESSAGE_HTTP_BIND>` and creates
empty protected TLS placeholders for the local initializer.

Start the baseline stack through the protected environment helper:

    deploy/split-agent/scripts/start-local.sh \
      /absolute/path/.run/split-20260804/.env

The helper revalidates the mode-0400 `.env` and manifest identities, their
owner, instance/generation bindings, and every declared Docker resource. It
refuses occupied host ports or any existing project container, network, or
volume before building, then rechecks them immediately before startup. Before
any build or Docker create, it also requires both configured `/cgroup` sources
to be existing delegated cgroup-v2 subtrees owned by their corresponding
runner UIDs and verifies the Docker Engine uses the systemd cgroup driver. It
builds the Agent image first from the
sibling Agent repository, builds message-server second, starts the complete
Agent/message-server dependency graph with `--no-build`, and waits for Agent,
both isolated runners, and message-server health. It never cleans an existing
or partially started stack; use the separately authorized cleanup command
below.

Each provisioned stack has a 128-bit generated namespace (`d-` plus 26
lower-case Base32 characters) and a mode 0400 `.manifest` binding every
network/volume name, instance identity, and account generation. Keep the
manifest for audit. To remove only this exact stack, run
`deploy/split-agent/scripts/cleanup-local.sh /absolute/path/.run/split`; add
`--purge` only when the fresh databases and volumes may be discarded.

The root host release operator may pause or resume only the three Agent runtime
containers through the receipt-bound wrappers after installing them at a
root-owned fixed path. A host updater integration must own both that path and
the provisioned `OUTPUT_DIR`; neither value may come from a public request:

    deploy/split-agent/scripts/stop-agent-local.sh /absolute/path/.run/split
    deploy/split-agent/scripts/restart-agent-local.sh /absolute/path/.run/split

These wrappers consume the complete `.cleanup-receipt` IDs for `agent`,
`extension-runner`, and `core-runner` only. Stop order is Agent then both
runners; restart order is both runners healthy then Agent healthy. A mixed
known state is recovered through an exact stop/start sequence, while missing,
replaced, unknown, or uninspectable identities fail closed. Exit status `0`
means the requested mutation completed, `3` means stop found the runtime
already stopped, and `1` means an infrastructure or identity check failed.
Restart always performs a real stop/start boundary, including for a healthy
runtime. The wrappers never invoke Compose, stop `message-server`, Postgres,
or remove containers/resources. They are host-side updater/release
operations. An updater consumer must invoke them as direct argv, treat stop
status `3` as an expected negative state, and fail on other non-zero statuses.
Flutter and other public clients must connect to message-server and must never
call these scripts or Docker directly.

The same root-owned integration point can apply a formal Agent version. After
authenticating central release discovery, the updater persists the exact Agent
plan and invokes the wrapper with its canonical target and minimum
message-server versions as direct argv:

    deploy/split-agent/scripts/update-agent-local.sh \
      /absolute/path/.run/split v1.0.1 v1.1.1

The wrapper accepts exactly `OUTPUT_DIR target_version minimum_server_version`;
both version values must be canonical stable `vX.Y.Z`. It performs no channel
directory, metadata file, path, or environment discovery. The minimum comes
directly from the authenticated updater command and is compared to
the OCI version label of the exact receipt-bound running message-server
container; it never compares a Flutter or other client version. An unmet
minimum, a non-newer Agent target, or a non-production stack is an expected
negative result (exit `3`) before pull or mutation. Exit `1` is an identity,
Docker, migration, health, or control-commit infrastructure failure. A successful
update pulls the fixed
`docker.io/dirextalk/agent:<version>` repository, resolves it to an immutable
digest, migrates Agent storage, recreates both runners and then Agent from that
one digest, then atomically updates the Agent digest and source revision in the
protected image attestation together with its manifest, environment, cleanup
receipt, and three container IDs. The same transaction runs the production
image smoke and isolated three-container topology gates before publishing the
new protected controls. A fixed owner-bound lock serializes the complete
transaction, and file-plus-directory fsync makes journal and control state
durable across process or host failure. Updates are one-way: a post-mutation failure does not
start the previous image or restore old controls. It preserves the last
receipt, publishes no replacement success receipt, exits non-zero, and writes
a mode-0400 `.agent-update-failure.*` record with the failed phase and immutable
target identity for operator audit and explicit recovery. After committing the
new controls and a `cleanup-pending` receipt, the wrapper revalidates the three
healthy target container identities, removes all fixed-repository references
to the previous image without `--force`, and only then publishes the complete
receipt.
If any non-owned, unbound, or running container or foreign repository alias
still uses the old image ID, the wrapper retains that image ID after removing
only its fixed `dirextalk/agent` repository references; it never removes the
foreign alias or performs a global prune.

This wrapper is the host execution adapter only. The current message-server
`release.v2` actions delegate exclusively to the
root-owned updater Unix contract, and Flutter never calls this wrapper or Agent
directly. The updater remains responsible for binding this argv invocation to
the same authenticated receipt used for status; public requests cannot supply
these wrapper arguments directly.

Formal production message-server updates use the sibling receipt-bound host
adapter with exactly `OUTPUT_DIR target_version`:

    deploy/split-agent/scripts/update-message-server-local.sh \
      /absolute/path/.run/split v1.0.1

The target is canonical stable `vX.Y.Z`; repository, image, Compose files,
project, and service are code-owned and are never argv inputs. Status `0`
means only message-server was recreated healthy, `3` is an expected
non-production or non-newer result, and `1` is an identity, Docker, health, or
control/journal infrastructure failure. The adapter atomically binds the resolved image digest and
OCI revision through the protected image attestation, manifest, environment,
cleanup receipt, and new container ID. Updates are one-way. A fixed root-owned
journal and lock preserve an interrupted or failed mutation for explicit
operator recovery; a later invocation refuses to resume it automatically and
never starts the previous image. Journal and control renames and the final
journal unlink are file-plus-directory fsynced. After committing the new controls and a
`cleanup-pending` receipt, the wrapper revalidates the healthy target container,
removes all fixed-repository references to the previous image without
`--force`, and only then publishes the complete receipt. Images used by an
unbound or other container, or carrying a foreign alias, retain their image ID
and foreign references after fixed `dirextalk/message-server` references are
removed. Agent and database
containers are never recreated, and no wrapper performs a global prune.

Formal Agent images are built from the sibling `dirextalk-agent` repository.
The first channel version is `v1.0.0`:

    scripts/release/prepare-agent.sh v1.0.0
    scripts/release/verify-agent.sh v1.0.0
    scripts/release/publish-agent.sh v1.0.0

Preparation requires the committed HEAD of Agent branch
`adam/agent-core-v1-integration`. The only permitted worktree entry is the
protected untracked `.codex-final-overlay.Containerfile`; verification records
that exact revision and exports the committed Git tree to a temporary build
context, so that local overlay can never enter the released image.
Verification builds `dirextalk/agent:v1.0.0` from
`deploy/container/agent.Containerfile`, checks its source/version OCI labels,
and executes all three bundled binaries. Publication pushes only after that
commit-bound evidence, verifies the version tag, then moves
`dirextalk/agent:latest` to the same image and verifies both tags resolve to the
same immutable Docker Hub digest used by the deployment channel.

The message-server portal owner is initialized by the protected
`portal.bootstrap` action, not by the ordinary Matrix `create-account` binary.
Provisioning generates a separate mode 0400 portal-password file and Compose
consumes it through `P2P_PORTAL_PASSWORD_FILE`; no password is placed in an
environment value. After the stack is healthy, export the credentials file
without printing tokens:

    mkdir -m 700 /absolute/path/.run/portal
    deploy/split-agent/scripts/export-portal-bootstrap.sh \
      /absolute/path/.run/split /absolute/path/.run/portal/bootstrap.json
    jq '{action:"portal.bootstrap",params:{password:.password}}' \
      /absolute/path/.run/portal/bootstrap.json > /absolute/path/.run/portal/request.json
    chmod 400 /absolute/path/.run/portal/request.json
    curl --fail --silent --show-error \
      -H 'Content-Type: application/json' \
      --data-binary @/absolute/path/.run/portal/request.json \
      "http://127.0.0.1:${DIREXTALK_MESSAGE_HTTP_BIND}/_p2p/query" \
      > /absolute/path/.run/portal/session.json
    chmod 400 /absolute/path/.run/portal/session.json
    jq -e '.access_token and .user_id and .agent_room_id' \
      /absolute/path/.run/portal/session.json >/dev/null

The response file is the owner-session input for authenticated acceptance and
is never echoed. `bootstrap-local-account.sh` exists only for creating a
separate non-owner Matrix test account; it must not establish the portal owner
session.

The generated message-server template contains only the literal
`__DIREXTALK_DB_DSN__` marker. `generate-config` never receives the protected
DSN as an argv value. At runtime the mounted entrypoint helper reads the
`message_database_url` secret file and renders the substituted config into the
private `/tmp` tmpfs before `exec`; the persisted config volume never stores
the database password or DSN. The helper uses only shell builtins for the
substitution, so the DSN is not passed to `sed`, `awk`, or another child
process's argv/environment.

Build and render without starting the stack:

Each consumer resolves the public capability-api v1.0.3 module. The Agent and
both bundled runner containers consume one image built from the sibling Agent
context; message-server builds from this repository with its local Dockerfile.
No capability-api build context or temporary `go.mod` replace is used:

    deploy/split-agent/scripts/build-local.sh \
      /absolute/path/.run/split-20260804/.env \
      agent message-server

    docker compose \
      --env-file /absolute/path/.run/split-20260804/.env \
      -f deploy/split-agent/compose.yaml \
      -f deploy/split-agent/compose.local.yaml \
      build

    docker compose \
      --env-file /absolute/path/.run/split-20260804/.env \
      -f deploy/split-agent/compose.yaml \
      -f deploy/split-agent/compose.local.yaml \
      config --quiet

The runner services have explicit entrypoints for bundled binaries and resolve
the exact same local Agent image; no runner-specific image or local tag exists.
Run the root-owned host-preparation sequence below first, then source its
generated `KEY=VALUE` output. Do not construct cgroup paths from `STACK`:
provisioning must consume the exact `ControlGroup` roots returned by the
helper. With that environment loaded, enable both runtime flags:

    export STACK=d-$(head -c 16 /dev/urandom | base32 | tr '[:upper:]' '[:lower:]' | tr -d '=[:space:]')
    # After the host-preparation sequence below:
    # set -a; . /tmp/dirextalk-runner-cgroups.env; set +a
    DIREXTALK_CORE_EXTENSION_ENABLED=true \
    DIREXTALK_CORE_WORKLOAD_ENABLED=true \
    DIREXTALK_SPLIT_STACK_NAME="$STACK" \
    deploy/split-agent/scripts/provision-local.sh \
      /absolute/path/.run/split-20260804-runners \
      /absolute/path/openrouter.key \
      /absolute/path/embedding.key

Then render and start the complete split runtime:

    docker compose \
      --env-file /absolute/path/.run/split-20260804-runners/.env \
      -f deploy/split-agent/compose.yaml \
      -f deploy/split-agent/compose.local.yaml \
      config --quiet

    docker compose \
      --env-file /absolute/path/.run/split-20260804-runners/.env \
      -f deploy/split-agent/compose.yaml \
      -f deploy/split-agent/compose.local.yaml \
      up -d

The runner services use `network_mode: none`, fixed image-ABI UIDs `65531`
(extension) and `65530` (Core workload), a private Unix-domain socket volume,
and only a caller-supplied delegated cgroup-v2 subtree bind-mounted at
`/cgroup`. They receive no Docker socket, PostgreSQL network, database URL,
Agent secret, or other host filesystem mount. Socket init jobs must complete
before either runner becomes healthy; Core's own health/readiness gate must
pass before clients are accepted. The provisioner always emits these exact UIDs
in `.env` and `agent-config.yaml`; setting
`DIREXTALK_CORE_EXTENSION_RUNNER_UID` or
`DIREXTALK_CORE_WORKLOAD_RUNNER_UID` to any other value is rejected. These
variables are metadata for the bundled image ABI, not build-time customization
knobs.

### Delegating cgroup-v2 on the host (Ubuntu 24.04+, systemd 254+)

The production runner contract requires Ubuntu 24.04 or newer, systemd 254 or
newer, a unified cgroup-v2 mount, and a rootful Docker daemon configured with
`CgroupDriver=systemd`. Per-user systemd delegation is not supported. The
repository-owned helper installs the fixed users/groups and the
two persistent template units, then starts the exact stack instances. It never
stops or replaces a same-name unit and refuses an existing file whose contents
or ownership differ.

    export STACK=d-$(head -c 16 /dev/urandom | base32 | tr '[:upper:]' '[:lower:]' | tr -d '=[:space:]')
    sudo install -d -o root -g root -m 0755 /usr/local/libexec/dirextalk/split-agent/scripts
    sudo install -d -o root -g root -m 0755 /usr/local/libexec/dirextalk/split-agent/systemd
    sudo install -d -o root -g root -m 0755 /usr/local/libexec/dirextalk/split-agent/sysusers.d
    sudo install -o root -g root -m 0755 deploy/split-agent/scripts/prepare-runner-cgroups.sh /usr/local/libexec/dirextalk/split-agent/scripts/prepare-runner-cgroups.sh
    sudo install -o root -g root -m 0644 deploy/split-agent/systemd/*.service /usr/local/libexec/dirextalk/split-agent/systemd/
    sudo install -o root -g root -m 0644 deploy/split-agent/sysusers.d/dirextalk-split-agent.conf /usr/local/libexec/dirextalk/split-agent/sysusers.d/
    sudo /usr/local/libexec/dirextalk/split-agent/scripts/prepare-runner-cgroups.sh "$STACK" > /tmp/dirextalk-runner-cgroups.env
    chmod 400 /tmp/dirextalk-runner-cgroups.env
    # Inspect the generated env file, then load it for provisioning.
    set -a; . /tmp/dirextalk-runner-cgroups.env; set +a
    export DIREXTALK_SPLIT_STACK_NAME="$STACK"
    export DIREXTALK_CORE_EXTENSION_ENABLED=true
    export DIREXTALK_CORE_WORKLOAD_ENABLED=true

After loading that root-owned preparation receipt, the explicit first-fresh
consumer wrapper runs the real provision -> Compose start -> acceptance path.
It rejects fixture mode, requires a brand-new output directory, and leaves the
accepted stack running for inspection; cleanup remains a separate authorized
step:

    DIREXTALK_FIRST_FRESH_AUTHORIZED=true \
      deploy/split-agent/scripts/verify-first-fresh.sh \
      --execute-first-fresh /absolute/path/to/new-run \
      /absolute/path/openrouter.key \
      /absolute/path/embedding.key \
      /absolute/path/tavily.key \
      /absolute/path/portal-password \
      openai/gpt-4o-mini openai/text-embedding-3-small

The same consumer wrapper is the production Docker Hub path. Set
`DIREXTALK_FIRST_FRESH_COMPOSE_MODE=production`, provide the two application
digests, the protected image-attestation source, and the public server name.
Production provisioning records that mode in both `.env` and `.manifest`.
`start-local.sh` then renders `compose.yaml` with
`compose.production.yaml`, verifies the protected TLS and attestation
identities, pulls every digest-pinned public image, runs the three-binary Agent
smoke gate, and starts with `--no-build --pull never`. The production override
binds only the host updater socket directory and control-token file into
message-server; local mode does not consume those host paths.
Neither the Agent nor message-server Git checkout is required on the target
host:

    DIREXTALK_FIRST_FRESH_AUTHORIZED=true \
    DIREXTALK_FIRST_FRESH_COMPOSE_MODE=production \
    DIREXTALK_CORE_EXTENSION_ENABLED=true \
    DIREXTALK_CORE_WORKLOAD_ENABLED=true \
    DIREXTALK_MESSAGE_SERVER_IMAGE_IMMUTABLE=docker.io/dirextalk/message-server@sha256:<digest> \
    DIREXTALK_AGENT_IMAGE_IMMUTABLE=docker.io/dirextalk/agent@sha256:<digest> \
    DIREXTALK_IMAGE_ATTESTATION_SOURCE_FILE=/absolute/path/image-attestation \
    DIREXTALK_MESSAGE_TLS_MODE=edge-terminated \
    DIREXTALK_MESSAGE_SERVER_NAME=s1.dirextalk.ai \
      deploy/split-agent/scripts/verify-first-fresh.sh \
      --execute-first-fresh /absolute/path/to/new-run \
      /absolute/path/openrouter.key \
      /absolute/path/embedding.key \
      /absolute/path/tavily.key \
      /absolute/path/portal-password \
      openai/gpt-4o-mini openai/text-embedding-3-small

If a provisioned first-fresh run fails before `start-local.sh` creates its
cleanup receipt, use the provision-only failure wrapper against that existing
run directory:

    deploy/split-agent/scripts/cleanup-provision-failure.sh \
      /absolute/path/to/failed-run

This path is only for the old/failed provision state with no cleanup receipt.
It revalidates the manifest, `.env`, host machine-id, local Docker Engine,
runner units, fragments, and control groups, and refuses mutation unless every
planned container, network, and volume is absent. It disables only the exact
stack runner units; the mode-0700 run directory and protected failure log are
left in place for audit. Use `cleanup-local.sh` for a run that has a normal
cleanup receipt.

The helper must be executed from this root-owned release path; it rejects a
user-owned checkout, symlinked asset, or group/world-writable parent before
touching users, units, or cgroups. The helper prints only `KEY=VALUE` lines. In addition to the two canonical
`/cgroup` roots, the output binds each exact systemd instance, template
`FragmentPath` and SHA-256, parent slice, and `ControlGroup`; `provision-local`
and `start-local` must preserve those bindings in the stack manifest. The
extension subtree is owned by UID/GID `65531` and the Core runner subtree by
UID/GID `65530`. Both roots must expose `cpu`, `memory`, and `pids`, and their
`cgroup.subtree_control` and `cgroup.procs` files must be writable by the
corresponding runner identity. Never bind `/sys/fs/cgroup` itself or a generic
system/user/global slice.

Only the local override builds sibling Agent sources, directly through the
Agent repository's Dockerfiles. Both consumer module graphs use the public
capability-api v1.0.3 release with no replace. The production compose file has no build section and
requires every image reference to end in `@sha256:<64 lowercase hex>`. The
provisioner writes `registry.invalid/...@sha256:<64 hex>` placeholders for
unpublished local application images; the local override replaces them with
`*_IMAGE_LOCAL` tags. Replace all placeholder immutable refs with release
digests before any production `up`. Production images must resolve the
released v1.0.3 module with no replace directive; use registry digests for
every non-local deployment.

For a fresh baseline stack, use the guarded startup sequence after the
API/runtime acceptance gate is green:

    deploy/split-agent/scripts/start-local.sh \
      /absolute/path/.run/split-20260804/.env

The message-server health endpoint is the only host-facing service check (use
the same host-port variables written to `.env`):

    curl --fail "http://127.0.0.1:${DIREXTALK_MESSAGE_HTTP_BIND}/_p2p/health"

No Agent Core, Agent Capability, Product Capability, or PostgreSQL port is
published to the host.

message-server uses a read-only rootfs, drops all Linux capabilities except
the narrowly required `DAC_READ_SEARCH` read-only bind-secret access, and sets
`no-new-privileges`; its explicit named data/config volumes and `/tmp` tmpfs
are the only writable paths. It remains UID 0 solely because file-based
Compose secrets are root-readable and the startup shell must consume the
protected DSN/capability material before exec. The application enforces HTTPS
authority/SNI and OpenRouter host validation; Compose does not provide an
HTTPS egress allowlist or firewall, so network-level destination restriction
must be supplied by the host/cloud boundary if required.

The shared PostgreSQL container also keeps the provisioned administrator,
message-server, and Agent password sources at mode 0400. Standalone Docker
Compose does not apply secret `uid`, `gid`, or `mode` metadata, so a root
startup wrapper copies exactly those three files into a private tmpfs before
the official image drops privileges. The tmpfs directory remains root-owned
and non-writable to the official `postgres` user; only the mode-0400 copies are
owned by that user. Both the official `POSTGRES_PASSWORD_FILE` consumer and
the fresh-state database initializer read only the materialized paths.

## Secret and identity map

The provisioner writes these protected files outside Git:

- Capability CA, server/client certificate pairs, directional and voice-relay
  tokens, and Ed25519 grant keys: created in protected named volumes by
  `message-server-init`; they are not provisioner files and never appear in
  `.env`;
- postgres-admin-password, mounted only into the shared PostgreSQL service;
- agent-postgres-password, message-postgres-password, and the corresponding
  role- and database-specific URL files. Applications receive only their own
  URL and never receive the cluster-admin or peer application credential;
- core-secret-master-key: a fresh raw 32-byte mode-0400 Agent master key. The
  Agent keyring uses it for authenticated encryption of Agent-owned durable
  secret snapshots (AWS and any enabled model, execution, extension, or chat
  snapshot stores). It is
  mounted only into the Agent secret-init path, copied into the Agent-owned
  secret volume, and referenced by `core_secret_master_key_file`; message-server,
  runners, `.env`, argv, and images never receive the key bytes. The manifest
  binds its device/inode/UID so cleanup refuses a replacement file;
- openrouter-api-key, embedding-api-key, and tavily-api-key: protected host
  files used by the acceptance helper. They are intentionally not mounted into
  the Agent process because Core v1 stores model and Web Search credentials
  through authenticated typed APIs; no runtime component reads these files
  directly.

Agent secret-init copies only Agent runtime credentials into a UID-owned named
volume with mode 0400. The acceptance helper reads the three provider key files
from the protected host directory and sends them once through authenticated
typed APIs; no provider key value is placed in Compose YAML, Agent mounts, or
logs.

## Model, embedding, Knowledge, and memory acceptance

The deployment config enables Knowledge in the fresh Agent PostgreSQL database
with pgvector and a generated embedding profile UUID. The default vector
dimension is 1536; set
`DIREXTALK_CORE_KNOWLEDGE_VECTOR_DIMENSION` when provisioning a new stack if
the selected OpenRouter-compatible embedding model returns another fixed
dimension (1–2000). Dimension is immutable for the fresh Agent database and the
acceptance helper verifies the configured value while performing a real
embedding/index/search cycle. Model profiles are mutable
Agent-owned database records, so provision the OpenRouter chat and embedding
profiles through the authenticated Core/Capability API using the protected key
files. The embedding profile request must use the exact generated
core_knowledge_embedding_profile_id from agent-config.yaml; the chat profile
may use a separate UUID. Then run:

1. live provider model catalog before save, model profile sync/list/get, and a
   second model catalog read through the stored client profile (responses must
   be non-empty, report configured credentials, and omit key bytes);
2. one chat request through message-server using the exact persisted profile
   ID/revision/credential-version triple, followed by an identical
   idempotency retry, plus a live Tavily-backed `web_search` tool turn whose
   persisted successful tool result is verified independently;
3. conversation create/list/get with persisted message-history readback;
4. Knowledge source upload, index task completion, semantic search, restart,
   and delete;
5. long-term memory create, update/re-index/search, then automatic recall of a
   unique stored marker in the first turn of a fresh conversation after Agent
   restart, followed by delete.

Interactive Native Agent chat currently auto-recalls only long-term Memory; it
does not automatically inject ordinary uploaded Knowledge. The Knowledge gate
therefore proves only the real upload/index/status/search path and its returned
source marker; it is not evidence of chat-time Knowledge grounding or automatic
RAG.

Use the same embedding API key for OpenRouter-compatible embeddings when the
provider permits it; keep the two protected host files separate so rotation
does not require a YAML change.

### Disposable local end-to-end acceptance

After building and starting a fresh local stack, run the public-interface
acceptance lane (never pass key values as arguments):

    deploy/split-agent/scripts/accept-local.sh \
      /absolute/path/.run/split-20260804 \
      openai/gpt-oss-20b:free \
      openai/text-embedding-3-small

The model IDs can also be supplied with
`DIREXTALK_ACCEPTANCE_CHAT_MODEL` and
`DIREXTALK_ACCEPTANCE_EMBEDDING_MODEL`. The helper reads the protected
`openrouter-api-key`, `embedding-api-key`, `tavily-api-key`, and
portal-password files, and all request/session/response/log files are mode
0400 under a mode-0700 temporary workspace. It talks only to message-server
`/_p2p/query` and `/_p2p/health`; it never calls an Agent listener
directly and never prints a token.

The lane verifies both health listeners, Compose port/network isolation,
OpenRouter live/stored-profile model catalogs, chat and embedding profile sync,
redacted configured profile list/get readback, stored Tavily configuration/test
across Agent restart, a real Native Agent chat that persists a successful
Tavily `web_search` tool result, automatic Knowledge embedding binding, native
chat plus identical-turn replay, conversation create/list/get and message
history, Product contacts and
prepared-message exact-once replay, forged-owner rejection, Knowledge upload
and automatic indexing/search, long-term memory update/re-index/search and
fresh-conversation automatic recall
after Agent restart, source/memory deletion, database-role/table/pgvector
isolation, and a key/log/config canary. By default it also performs the
final `portal.account.delete` and verifies the sealed Agent, deprovision ledger,
business-table purge, including Agent-owned vector rows; post-delete checks use
only Compose exec. There is intentionally no
`agent.knowledge.index` workaround: a binding mismatch is a hard contract
failure.

This HTTP acceptance lane does not claim Native Agent WebSocket streaming,
reconnect/resume, or cancellation coverage. Those behaviors require a later
dedicated gate using real `client.native_agent_stream` and
`server.native_agent_stream.*` frames; an HTTP `agent.chat` response cannot
stand in for that protocol evidence.

For the final persistent-account acceptance, set
`DIREXTALK_ACCEPTANCE_ACCOUNT_DELETE=false`. This keeps the configured model
profiles, Tavily configuration, and conversation records while still deleting
the temporary Knowledge source and long-term memory created by the lane. The
account-delete, post-delete health/rejection, database purge, and
deprovision-ledger assertions are then
skipped. `DIREXTALK_ACCEPTANCE_ACCOUNT_DELETE` defaults to `true`; setting
`DIREXTALK_ACCEPTANCE_CLEANUP_AFTER=true` while account deletion is disabled is
rejected, so a persistent acceptance cannot accidentally purge its namespace.

Set `DIREXTALK_ACCEPTANCE_CLEANUP_AFTER=true` to purge the exact disposable
namespace after a successful run. The helper does not create AWS resources or
exercise live Voice credentials; AWS acceptance requires a separate approved
disposable-account runbook.

Run the focused static/mock boundary check with:

    deploy/split-agent/scripts/accept-local.test.sh

AWS CloudControl/workload capabilities remain unavailable until an explicitly
authorized disposable-account runbook supplies typed readiness proof. This
Compose harness must not be used as AWS authorization. When Execution V2 is
enabled, configure the Agent's non-secret
`core_aws_cloudformation_service_role_arn` to one exact allowlisted role ARN
and pass the same value to `deploy/split-agent/aws/validate-policy.sh`; the
policy gate requires a separate `iam:PassRole` statement constrained to
`iam:PassedToService=cloudformation.amazonaws.com` and fails closed when it is
missing or wildcarded. The Agent never falls back to the caller role.

## Production shape

The neutral Capability API is published and locally verified at v1.0.3. Both
committed Go modules resolve this public release without a replace directive,
submodule, workspace path, or capability-api build context. Both Go consumers
must require `github.com/YingSuiAI/dirextalk-capability-api v1.0.3`. The image
attestation therefore includes `capability_api_source=published`; the verifier
rejects `local-relative-replace`.

Use `compose.yaml` together with `compose.production.yaml` and a reviewed
`.env` containing:

- immutable digest-pinned application images for message-server and Agent; the
  extension-runner and Core workload-runner containers resolve the exact same
  Agent digest;
- an immutable digest-pinned PostgreSQL 18 plus pgvector image;
- a unique stack name and unique network/volume names;
- a protected output directory with mode 0700 and secret files mode 0400;
- fresh message-server/Agent instance IDs and the generated positive account
  generation pair (never copy these values into a replacement stack).

Before a production render, create a mode 0400
`# dirextalk-image-attestation-v2` file containing the exact five digest
references, `capability_api_version=v1.0.3`,
`capability_api_source=published`, and the full lowercase Git commits in
`message_source_revision` and `agent_source_revision`, then run:

    deploy/split-agent/scripts/verify-production-images.sh \
      /absolute/path/.run/split/.env \
      /absolute/path/release/image-attestation

The image gate inspects both immutable application digests and requires each
`org.opencontainers.image.revision` label to match its repository-specific
attested revision. It runs `docker run --rm --entrypoint` smoke checks against
the Agent digest for `/usr/local/bin/dirextalk-agent`,
`/usr/local/bin/dirextalk-extension-runner`, and
`/usr/local/bin/dirextalk-core-runner`; a missing binary, unexpected exit, or
metadata mismatch blocks the production render.

Production requires either the canonical
`DIREXTALK_MESSAGE_TLS_MODE=edge-terminated` contract or the explicit
`external` direct-TLS contract. The gate checks that edge-terminated mode has
an `https://` canonical client URL, empty protected message-server TLS
placeholders, and no direct certificate input. External mode instead requires
a trusted certificate whose SAN/CN matches `DIREXTALK_MESSAGE_SERVER_NAME`, a
matching certificate/private key pair (each mode 0400), and at least seven
days remaining:

    deploy/split-agent/scripts/verify-production-tls.sh \
      /absolute/path/.run/split/.env

The `local` TLS mode intentionally generates a disposable self-signed
certificate and is never a trusted-public production claim. Edge termination
does not weaken the independent Agent/Product Capability mTLS, exact direction
tokens, instance/generation fencing, or signed capability grants.

Run config --quiet before any migration or service start. The migration services
use the exact same application image as their serving process. A rollback is a
fresh deployment with a matching image and a newly provisioned namespace;
never attach a newer image to an old namespace in this harness.

## Guarded public-edge cutover

Public TLS termination is a separate, tracked Compose project in
`edge-compose.yaml`. It contains only one Caddy service: the service is
digest-pinned through `DIREXTALK_CADDY_IMAGE_IMMUTABLE`, read-only, drops all
Linux capabilities, then adds only `NET_BIND_SERVICE` so the official Caddy
binary's file capability can bind ports 80/443 while `no-new-privileges` is
enabled, and joins only the fresh message-server `message_public` network.
Caddy data
and config are explicitly named external volumes. The Caddyfile is a reviewed,
mode-0400 regular file and, for the canonical edge-terminated contract, must
reverse-proxy only to `message-server:8008`.

`adopt-edge.sh` is the one-shot first-edge adoption procedure. It is followed
by `cutover-edge.sh` for later fresh-stack switches; adoption is not a
cutover compatibility fallback. Prepare a mode-0400 edge environment with
`DIREXTALK_MESSAGE_TLS_MODE=edge-terminated` and a mode-0700 receipt directory,
then probe the exact full legacy Caddy ID. Adoption fails closed unless the
reviewed Caddyfile proxies exactly to `message-server:8008`:

    deploy/split-agent/scripts/adopt-edge.sh probe \
      /absolute/path/.run/legacy-edge.env \
      0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
      /absolute/path/.run/edge-adoption/probe.receipt \
      edge-adopt-20260805 revision-1

The probe is read-only with respect to Docker. Its protected mode-0400
receipt binds the current UID, host/machine identity and Docker Engine ID,
operation/revision confirmation, the exact legacy container ID, its approved
Docker Hub Caddy Config.Image tag, exact image ID and normalized RepoDigest,
Compose labels, public network object and labels,
both Caddy volume objects, ports, reviewed Caddyfile and edge Compose file
device/inode/UID/mode plus SHA-256, the same binding for the edge environment
and optional public CA file, and public health, Matrix well-known, and TLS
checks. The receipt path must not exist and is never overwritten.
The legacy readiness field is one of exactly two values: `healthy` requires
the existing Docker health status to remain healthy, while
`unconfigured-public-probe` requires both Docker health configuration and
state to remain absent and relies on the successful public probes. The same
readiness mode and public endpoints are checked again immediately before the
exact legacy container is stopped. This exception applies only to the legacy
container; the new candidate always requires a configured, healthy Docker
healthcheck.

After reviewing the receipt, commit only with the exact operation and revision
confirmation. The commit takes an exclusive operation lock, revalidates every
probe-bound identity, renders and pre-creates a digest-pinned hardened Caddy
candidate in a distinct edge project on the same network and volumes, and
binds its full immutable container ID before stopping the legacy ID:

    deploy/split-agent/scripts/adopt-edge.sh commit \
      /absolute/path/.run/legacy-edge.env \
      /absolute/path/.run/edge-adoption/probe.receipt \
      edge-adopt-20260805 revision-1 \
      /absolute/path/.run/edge-adoption/active.receipt \
      /absolute/path/.run/edge-adoption/legacy.snapshot

Only the exact legacy ID is stopped and only the exact candidate ID is
started. The candidate must expose 80/443, retain the recorded network and
volume object identities, use the digest-pinned image/RepoDigest, expose a
usable healthcheck command, and satisfy read-only-rootfs, capability-drop with
exactly NET_BIND_SERVICE added, and no-new-privileges checks before the legacy
stop boundary. Failure verifies
the candidate identity before removing it, re-inspects the exact legacy ID
before starting it, and verifies public health/TLS after rollback. A running
legacy must remain in the public network object's container map; after an
exact stop, an exited/created legacy may be omitted while the bound network
object identity remains unchanged. No
Compose `down`, volume deletion, or name-based mutation is used. A successful
commit writes a separate mode-0400 legacy snapshot and a compliant
`# dirextalk-edge-receipt-v1` active receipt, preserves the stopped legacy
container, marks the operation consumed, and rejects replay or receipt
collisions.

For subsequent fresh-stack switches, `cutover-edge.sh` takes four protected
paths:

    deploy/split-agent/scripts/cutover-edge.sh \
      /absolute/path/.run/new-split/.env \
      /absolute/path/.run/new-edge.env \
      /absolute/path/.run/edge/active.receipt \
      /absolute/path/.run/edge/cutover.receipt

The new message stack must already be healthy, use
`DIREXTALK_MESSAGE_TLS_MODE=edge-terminated`, and publish only its HTTP port on
host loopback; port 8448 must be absent. The edge environment names the new edge Compose project,
public domain, fresh message public network, immutable Caddy image, reviewed
Caddyfile, and the existing external Caddy data/config volumes. The active
receipt records the exact current Caddy container ID, immutable image, Compose
project, network, ports, and mounts; its owner is checked against the current
UID and the path must be mode 0400 and non-symlink.

Before stopping anything, the helper renders both Compose files and verifies
the fresh message-server/Agent container and network identities, host health,
and the private HTTP health boundary. It then re-inspects the exact recorded old Caddy ID immediately before
stopping it, starts the new edge with `up -d --wait caddy`, and checks public
health, Matrix well-known, and certificate validation. A failed cutover removes
or stops only a newly verified Caddy container and starts the exact old ID;
unknown same-name replacements are never stopped or removed. The helper never
runs a stack-wide teardown, deletes a volume, or touches either database. A
mode-0400 receipt is atomically created only after all checks pass; an existing
output receipt is rejected so prior audit evidence cannot be overwritten. The
helper also binds the host/machine and Docker Engine identity plus every
control-file device/inode/UID/mode/SHA-256 and revalidates them immediately
before a stop/start/remove mutation. Rollback re-inspects the exact recorded
old ID before attempting its start, and receipt temporary files are created
with unpredictable names inside the private receipt directory.

Run the mock boundary checks before a production change:

    deploy/split-agent/scripts/adopt-edge.test.sh
    deploy/split-agent/scripts/cutover-edge.test.sh
