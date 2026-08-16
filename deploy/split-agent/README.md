# Split Agent deployment harness

This directory is the fresh-data deployment boundary for the split architecture:

- Flutter and every public client connect through the canonical public base
  URL. In edge-terminated production, message-server has only internal HTTP
  `:8008` plus a loopback host binding; Caddy owns public HTTPS. Explicit
  direct-TLS modes additionally enable internal `:8448` and its loopback host
  binding (the local acceptance example uses 18008/18448).
- message-server owns Matrix/ProductCore, account control and short-lived Agent
  ticket issuance, external MCP, and Product Capability on private port 50053.
- Container-local health probes explicitly disable ambient HTTP(S) proxy use;
  loopback readiness must never be redirected to an outbound proxy.
- dirextalk-agent owns Native Agent Core and the direct HTTP data plane on
  container port 8082.
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
- The non-internal `message_public` bridge is attached only to message-server
  and Agent. Caddy sends ordinary traffic to message-server and same-origin
  `/agent/v1/*` to the Agent service name; Agent port 8082 is never published
  on the host. Database and runner networks remain isolated.
- The canonical production mode is `edge-terminated`: message-server listens
  only on HTTP `:8008`, publishes that listener only on host loopback, and is
  reachable from Caddy through `message_public`; Agent joins the same bridge
  only for the `/agent/v1/*` reverse proxy.
  Caddy alone owns public ports 80/443 and ACME state; the canonical client URL
  remains `https://<DIREXTALK_MESSAGE_SERVER_NAME>`. No certificate or private
  key is provisioned into message-server in this mode. `local` and `external`
  direct TLS remain explicit test/operator modes through
  `compose.direct-tls.yaml`; their healthcheck probes both internal listeners.
- Local mode disables AWS by default. Production enables the Agent-owned AWS
  credential store so a user can upload one credential and request a dynamic
  SSH Worker without a deployment-time provider binding. Worker state, keys,
  and downloaded artifacts are derived beneath
  `/var/lib/dirextalk-agent/extension-staging/cloud-worker` on the existing
  staging volume. The Agent container retains its normal egress network and
  image-provided SSH/SCP runtime; it does not expose a Worker listener or
  require S3, KMS, a custom AMI, a model relay, or an edge proxy.

Agent reaches Message Server through the Product Capability gRPC direction
with mTLS, its exact direction token, instance/generation metadata, and Ed25519
grants. Message Server has no reverse Agent Capability gRPC client. The
separate voice callback relay retains its existing mTLS client identity.
`message-server-init` creates the private CA and issues the required
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
Each run receives a fresh random account generation shared by Message Server,
the Agent HTTP ticket verifier, and Product Capability peer metadata:

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
the version label of the exact receipt-bound running message-server
container; it never compares a Flutter or other client version. An unmet
minimum, a non-newer Agent target, or a non-production stack is an expected
negative result (exit `3`) before pull or mutation. Exit `1` is an identity,
Docker, migration, health, or control-commit infrastructure failure. A successful
update pulls `docker.io/dirextalk/agent:latest`, verifies its expected version,
revision, and three binary versions, migrates Agent storage, recreates both
runners and Agent from that pulled image, and updates the expected release and
receipt-bound container IDs. The wrapper verifies the same binary versions in
the healthy running containers. A fixed owner-bound lock serializes updates.

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
control infrastructure failure. The adapter pulls
`docker.io/dirextalk/message-server:latest`, verifies its expected version,
revision, and binary version, recreates message-server, then repeats the binary
probe against the healthy container and updates its receipt-bound container ID.
Agent and database
containers are never recreated, and no wrapper performs a global prune.

Formal Agent images are built from the sibling `dirextalk-agent` repository.
The Agent repository owns its stable release workflow, version tag, GitHub
Release, `dirextalk/agent:latest` update, and three-binary version probes. This
repository does not build or publish Agent release images.

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
    sudo install -d -o root -g root -m 0755 /usr/local/libexec/dirextalk/split-agent/apparmor.d
    sudo install -o root -g root -m 0755 deploy/split-agent/scripts/prepare-runner-cgroups.sh /usr/local/libexec/dirextalk/split-agent/scripts/prepare-runner-cgroups.sh
    sudo install -o root -g root -m 0755 deploy/split-agent/scripts/manage-runner-apparmor.sh /usr/local/libexec/dirextalk/split-agent/scripts/manage-runner-apparmor.sh
    sudo install -o root -g root -m 0644 deploy/split-agent/systemd/*.service /usr/local/libexec/dirextalk/split-agent/systemd/
    sudo install -o root -g root -m 0644 deploy/split-agent/sysusers.d/dirextalk-split-agent.conf /usr/local/libexec/dirextalk/split-agent/sysusers.d/
    sudo install -o root -g root -m 0644 deploy/split-agent/apparmor.d/dirextalk-runner-userns /usr/local/libexec/dirextalk/split-agent/apparmor.d/dirextalk-runner-userns
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
release channels with their expected versions and revisions, and the public server name.
Production provisioning records that mode in both `.env` and `.manifest`.
`start-local.sh` then renders `compose.yaml` with
`compose.production.yaml`, verifies protected TLS, pulls the application
`latest` channels and digest-pinned infrastructure images, checks all real
binary versions, and starts with `--no-build --pull never`. The production override
binds only the host updater socket directory and control-token file into
message-server; local mode does not consume those host paths.
Neither the Agent nor message-server Git checkout is required on the target
host:

    DIREXTALK_FIRST_FRESH_AUTHORIZED=true \
    DIREXTALK_FIRST_FRESH_COMPOSE_MODE=production \
    DIREXTALK_CORE_EXTENSION_ENABLED=true \
    DIREXTALK_CORE_WORKLOAD_ENABLED=true \
    DIREXTALK_MESSAGE_SERVER_IMAGE=docker.io/dirextalk/message-server:latest \
    DIREXTALK_MESSAGE_SERVER_VERSION=v1.1.33 \
    DIREXTALK_MESSAGE_SOURCE_REVISION=<full-commit> \
    DIREXTALK_AGENT_IMAGE=docker.io/dirextalk/agent:latest \
    DIREXTALK_AGENT_VERSION=v1.0.69 \
    DIREXTALK_AGENT_SOURCE_REVISION=<full-commit> \
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
capability-api v1.0.3 release with no replace. The production compose file has
no build section. Application services consume only the two canonical
Docker Hub `latest` channels; PostgreSQL, utility, coturn, and Caddy remain
digest-pinned.

For a fresh baseline stack, use the guarded startup sequence after the
API/runtime acceptance gate is green:

    deploy/split-agent/scripts/start-local.sh \
      /absolute/path/.run/split-20260804/.env

The message-server health endpoint is the only host-facing service check (use
the same host-port variables written to `.env`):

    curl --fail "http://127.0.0.1:${DIREXTALK_MESSAGE_HTTP_BIND}/_p2p/health"

No Agent HTTP/Core, Product Capability, or PostgreSQL port is published to the
host.

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

## Direct Agent first-fresh acceptance

After building and starting a fresh local stack, run the public-interface
acceptance lane (never pass key values as arguments):

    deploy/split-agent/scripts/accept-local.sh \
      /absolute/path/.run/split-20260804 \
      openai/gpt-oss-20b:free \
      openai/text-embedding-3-small

The model IDs can also be supplied with
`DIREXTALK_ACCEPTANCE_CHAT_MODEL` and
`DIREXTALK_ACCEPTANCE_EMBEDDING_MODEL`. The helper obtains the owner session
through Message Server, creates a 15-minute Agent ticket, and then uses only
same-origin `/agent/v1/*` routes. In local development,
`DIREXTALK_ACCEPTANCE_MESSAGE_BASE_URL` may point at the disposable Caddy edge;
the helper uses that one origin for both planes and never accepts an Agent
private address.

The lane verifies Message Server and Agent health, ticket-authenticated catalog
readiness, asynchronous model-profile sync, identical frozen mutation replay,
`202` turn admission, independent SSE observation, disconnect/resume with both
`after_seq` and `Last-Event-ID`, and authoritative conversation/turn history
readback. It never restores a ProductCore Agent business action, waits for a
turn inside the POST, creates AWS resources, deletes an account, or prints a
token or model credential. Knowledge, memory, attachments, confirmations, and
Worker acceptance are owned by their direct Agent and Flutter workflow lanes.

Run the focused static/mock boundary check with:

    deploy/split-agent/scripts/accept-local.test.sh

Cloud Worker AWS authorization comes only from the owner credential uploaded
through the Agent API. The split deployment accepts no AWS credential,
provider, SSM target, or instance binding input.

## Production shape

The neutral Capability API is published and locally verified at v1.0.3. Both
committed Go modules resolve this public release without a replace directive,
submodule, workspace path, or capability-api build context. Both Go consumers
must require `github.com/YingSuiAI/dirextalk-capability-api v1.0.3`.

Use `compose.yaml` together with `compose.production.yaml` and a reviewed
`.env` containing:

- `docker.io/dirextalk/message-server:latest` and
  `docker.io/dirextalk/agent:latest`, with expected versions and full source
  revisions; all Agent runtime containers resolve the same pulled image ID;
- a single-execution extension-runner whose outer container is capped at two
  CPUs, 1 GiB memory, and 256 processes in addition to each workload cgroup;
- an immutable digest-pinned PostgreSQL 18 plus pgvector image;
- a unique stack name and unique network/volume names;
- a protected output directory with mode 0700 and secret files mode 0400;
- fresh message-server/Agent instance IDs and the generated positive account
  generation pair (never copy these values into a replacement stack).

Before production startup, render the expected application versions and full
lowercase source revisions into `.env`, then run:

    deploy/split-agent/scripts/verify-production-images.sh \
      /absolute/path/.run/split/.env

The image gate requires each image version/revision label to match the expected
release and runs `--version` checks for message-server and all Agent binaries:
`/usr/local/bin/dirextalk-agent`,
`/usr/local/bin/dirextalk-extension-runner`, and
`/usr/local/bin/dirextalk-core-runner`. Startup repeats the probes against the
healthy running containers.

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
enabled, and joins only the fresh `message_public` network shared by
message-server and Agent.
Caddy data
and config are explicitly named external volumes. The Caddyfile is a reviewed,
mode-0400 regular file and, for the canonical edge-terminated contract, must
reserve `/.sites/*` before the backend proxy, serve the Agent-owned
`DIREXTALK_STATIC_SITES_ROOT/public` subtree from `/srv/dirextalk-sites`
read-only with the fixed sandbox CSP, route `/agent/v1/*` without stripping the
path to `agent:8082` with unbuffered SSE, and reverse-proxy all remaining
traffic to `message-server:8008`. `Caddyfile.static-sites.example` is the current
reviewed shape. The default host root is `/var/lib/dirextalk-static-sites`;
it and its `public`/`.staging` children are owned by Agent UID/GID 65532.
Single-file HTML is stored directly as `index.html`; no archive is created.
The current contract does not accept multi-file bundles.

`adopt-edge.sh` is the one-shot first-edge adoption procedure. It is followed
by `cutover-edge.sh` for later fresh-stack switches; adoption is not a
cutover compatibility fallback. Prepare a mode-0400 edge environment with
`DIREXTALK_MESSAGE_TLS_MODE=edge-terminated` and a mode-0700 receipt directory,
then probe the exact full legacy Caddy ID. Adoption fails closed unless the
reviewed Caddyfile serves the fixed `/.sites/*` route and otherwise proxies
exactly to `message-server:8008`. The edge environment must include the same
canonical `DIREXTALK_STATIC_SITES_ROOT` bound by the Agent stack:

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
