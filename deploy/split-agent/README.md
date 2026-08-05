# Split Agent deployment harness

This directory is the fresh-data deployment boundary for the split architecture:

- Flutter and every public client connect only to message-server on the
  provisioned host ports. Internal container listeners remain 8008 (HTTP) and
  8448 (HTTPS); host ports are validated and may be changed per fresh stack to
  avoid an existing service (the acceptance example uses 18008/18448).
- message-server owns Matrix/ProductCore, the public action envelope, the
  external Native Agent facade, and Product Capability on private port 50053.
- dirextalk-agent owns Native Agent Core on private port 9443 and Agent
  Capability on private port 50052.
- Agent and message-server use different PostgreSQL services, roles, databases,
  volumes, and private database networks. Account deletion may wipe either
  database; this harness does not attempt a historical migration.
- Native Agent Core is the only service that owns model, Knowledge, memory,
  task, schedule, extension, and workload state. The optional extension and
  Core workload runners are separate profile-gated containers; the default
  stack does not create or start them.
- Qdrant is reachable only from the Agent private network. Agent egress is
  separate and is used only for configured OpenRouter/embedding HTTPS calls.
- A dedicated non-internal `message_public` bridge is attached only to
  message-server because rootless Docker needs one non-internal bridge for
  host-port forwarding. Agent, database, Qdrant, and capability networks stay
  internal; no other service joins this edge network.
- message-server serves the same routes on both listeners: HTTP `:8008` for
  local acceptance and HTTPS `:8448` for public clients. Compose passes the
  existing server TLS flags (`--tls-cert`/`--tls-key`) explicitly. In `local`
  mode the init service generates a disposable certificate into the protected
  config volume; in `external` mode it copies the provisioned certificate and
  key there. The key value is never placed in `.env`, Compose interpolation, or
  logs. The message-server healthcheck probes both internal listeners, so a
  stack cannot become healthy while HTTPS is absent.
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
Each run receives a fresh random account generation shared by message-server,
Agent Capability, and Product Capability peer metadata:

    cd /home/adam/dirextalk/dirextalk-message-server
    export DIREXTALK_MESSAGE_HTTP_BIND=${DIREXTALK_MESSAGE_HTTP_BIND:-18008}
    export DIREXTALK_MESSAGE_HTTPS_BIND=${DIREXTALK_MESSAGE_HTTPS_BIND:-18448}
    deploy/split-agent/scripts/provision-local.sh \
      /absolute/path/.run/split-20260804 \
      /absolute/path/openrouter.key \
      /absolute/path/embedding.key

The second and third arguments are protected source files. A missing argument
creates a mode 0400 empty placeholder; replace that file before model or
Knowledge acceptance. The provisioner also creates disposable certs/tokens,
two PostgreSQL URLs, UUIDs, the non-secret Agent YAML, and a path-only .env.
Secret values are never copied to .env or printed.

Start the baseline stack through the protected environment helper:

    deploy/split-agent/scripts/start-local.sh \
      /absolute/path/.run/split-20260804/.env

The helper revalidates the mode-0400 `.env` and manifest identities, their
owner, instance/generation bindings, and every declared Docker resource. It
refuses occupied host ports or any existing project container, network, or
volume before building, then rechecks them immediately before startup. It builds
the Agent image first from the sibling Agent repository, builds message-server
second, starts only the message-server dependency graph with `--no-build`, and
waits for both Agent and message-server health. It never cleans an existing or
partially started stack; use the separately authorized cleanup command below.

Each provisioned stack has a 128-bit generated namespace (`d-` plus 26
lower-case Base32 characters) and a mode 0400 `.manifest` binding every
network/volume name, instance identity, and account generation. Keep the
manifest for audit. To remove only this exact stack, run
`deploy/split-agent/scripts/cleanup-local.sh /absolute/path/.run/split`; add
`--purge` only when the fresh databases and volumes may be discarded.

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

Each consumer resolves the public capability-api v1.0.3 module. Agent and its
optional runners build from the sibling Agent context with Dockerfiles owned by
that repository; message-server builds from this repository with its local
Dockerfile. No capability-api build context or temporary `go.mod` replace is
used:

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

The local override also builds the optional runner images from the sibling
Agent checkout using `deploy/container/extension-runner.Containerfile` and
`deploy/container/core-runner.Containerfile` from that checkout:

    docker compose \
      --env-file /absolute/path/.run/split-20260804/.env \
      -f deploy/split-agent/compose.yaml \
      -f deploy/split-agent/compose.local.yaml \
      --profile extensions --profile core-runner \
      build extension-runner core-runner

The profile services are not included in the baseline `build`/`up` model.
Keep both feature flags false unless the corresponding runner readiness gate
has passed. To exercise the isolated acceptance lane, provision with explicit
delegated cgroup roots and enable both flags:

    export STACK=d-$(head -c 16 /dev/urandom | base32 | tr '[:upper:]' '[:lower:]' | tr -d '=[:space:]')
    DIREXTALK_CORE_EXTENSION_ENABLED=true \
    DIREXTALK_CORE_WORKLOAD_ENABLED=true \
    DIREXTALK_SPLIT_STACK_NAME="$STACK" \
    DIREXTALK_EXTENSION_CGROUP_ROOT="/sys/fs/cgroup/${STACK}-extension" \
    DIREXTALK_CORE_RUNNER_CGROUP_ROOT="/sys/fs/cgroup/${STACK}-core-runner" \
    deploy/split-agent/scripts/provision-local.sh \
      /absolute/path/.run/split-20260804-runners \
      /absolute/path/openrouter.key \
      /absolute/path/embedding.key

Then render and start only the explicit profiles:

    docker compose \
      --env-file /absolute/path/.run/split-20260804-runners/.env \
      --profile extensions --profile core-runner \
      -f deploy/split-agent/compose.yaml \
      -f deploy/split-agent/compose.local.yaml \
      config --quiet

    docker compose \
      --env-file /absolute/path/.run/split-20260804-runners/.env \
      --profile extensions --profile core-runner \
      -f deploy/split-agent/compose.yaml \
      -f deploy/split-agent/compose.local.yaml \
      up -d

The runner profile services use `network_mode: none`, fixed UIDs `65531`
(extension) and `65530` (Core workload), a private Unix-domain socket volume,
and only a caller-supplied delegated cgroup-v2 subtree bind-mounted at
`/cgroup`. They receive no Docker socket, PostgreSQL network, database URL,
Agent secret, or other host filesystem mount. Socket init jobs are profile
dependencies and must complete before either runner becomes healthy; Core's
own health/readiness gate must pass before clients are accepted.

### Delegating cgroup-v2 without sudo

On a native Linux host, or a WSL distribution with a running user systemd
instance and cgroup-v2, a user-owned delegated subtree can be created without
sudo. Run this once per fresh stack (the commands deliberately use unique
unit names):

    export STACK=d-$(head -c 16 /dev/urandom | base32 | tr '[:upper:]' '[:lower:]' | tr -d '=[:space:]')
    export EXT_SLICE="${STACK}-extension.slice"
    export CORE_SLICE="${STACK}-core-runner.slice"
    systemctl --user daemon-reload
    systemd-run --user --unit="${STACK}-extension-delegate.service" \
      --property="Slice=${EXT_SLICE}" --property=Delegate=yes \
      --property=Type=oneshot --property=RemainAfterExit=yes \
      /usr/bin/sleep infinity
    systemd-run --user --unit="${STACK}-core-runner-delegate.service" \
      --property="Slice=${CORE_SLICE}" --property=Delegate=yes \
      --property=Type=oneshot --property=RemainAfterExit=yes \
      /usr/bin/sleep infinity
    EXT_CGROUP=$(systemctl --user show -p ControlGroup --value \
      "${STACK}-extension-delegate.service")
    CORE_CGROUP=$(systemctl --user show -p ControlGroup --value \
      "${STACK}-core-runner-delegate.service")
    test -n "$EXT_CGROUP" && test -n "$CORE_CGROUP"
    export DIREXTALK_EXTENSION_CGROUP_ROOT="/sys/fs/cgroup${EXT_CGROUP}"
    export DIREXTALK_CORE_RUNNER_CGROUP_ROOT="/sys/fs/cgroup${CORE_CGROUP}"
    test -f "$DIREXTALK_EXTENSION_CGROUP_ROOT/cgroup.controllers"
    test -f "$DIREXTALK_CORE_RUNNER_CGROUP_ROOT/cgroup.controllers"

Use the resulting two absolute paths in the runner-profile provision command.
Provisioning rejects `/sys/fs/cgroup` and the top-level system/user/global
slice paths. Each path must contain the fresh stack identity, be a real
cgroup-v2 directory with non-empty `cgroup.controllers`, writable
`cgroup.subtree_control`/`cgroup.procs`, and be owned by the provisioning user.
The Docker daemon must use the systemd cgroup driver (`docker info
--format '{{.CgroupDriver}}'` should report `systemd`) and the daemon must be
able to bind these user-owned paths. WSL installations without user systemd,
without cgroup-v2, or with a rootful daemon that cannot grant the runner UIDs
write access must leave both profiles disabled; do not bind-mount the host
`/sys/fs/cgroup` root as a substitute. Stop the transient delegation after
acceptance with:

    systemctl --user stop "${STACK}-extension-delegate.service" \
      "${STACK}-core-runner-delegate.service"

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

No Agent Core, Agent Capability, Product Capability, PostgreSQL, or Qdrant
port is published to the host.

message-server uses a read-only rootfs, drops all Linux capabilities except
the narrowly required `DAC_READ_SEARCH` read-only bind-secret access, and sets
`no-new-privileges`; its explicit named data/config volumes and `/tmp` tmpfs
are the only writable paths. It remains UID 0 solely because file-based
Compose secrets are root-readable and the startup shell must consume the
protected DSN/capability material before exec. The application enforces HTTPS
authority/SNI and OpenRouter host validation; Compose does not provide an
HTTPS egress allowlist or firewall, so network-level destination restriction
must be supplied by the host/cloud boundary if required.

## Secret and identity map

The provisioner writes these protected files outside Git:

- Capability CA, server/client certificate pairs, directional and voice-relay
  tokens, and Ed25519 grant keys: created in protected named volumes by
  `message-server-init`; they are not provisioner files and never appear in
  `.env`;
- agent-postgres-password, message-postgres-password, and the corresponding
  database URL files;
- core-secret-master-key: a fresh raw 32-byte mode-0400 Agent master key. The
  Agent keyring uses it for authenticated encryption of Agent-owned durable
  secret snapshots (AWS and any enabled model, execution, extension, or chat
  snapshot stores). It is
  mounted only into the Agent secret-init path, copied into the Agent-owned
  secret volume, and referenced by `core_secret_master_key_file`; message-server,
  runners, `.env`, argv, and images never receive the key bytes. The manifest
  binds its device/inode/UID so cleanup refuses a replacement file;
- openrouter-api-key and embedding-api-key: protected host files used by the
  acceptance helper. They are intentionally not mounted into the Agent
  process because Core v1 stores model credentials through the authenticated
  model profile API; no runtime component reads these files directly.

Agent secret-init copies only Agent runtime credentials into a UID-owned named
volume with mode 0400. The acceptance helper reads the two model key files
from the protected host directory and sends them once through the authenticated
API; no model key value is placed in Compose YAML, Agent mounts, or logs.

## Model, embedding, Knowledge, and memory acceptance

The deployment config enables Knowledge with a fresh Qdrant collection and a
generated embedding profile UUID. The default vector dimension is 1536; set
`DIREXTALK_CORE_KNOWLEDGE_VECTOR_DIMENSION` when provisioning a new stack if
the selected OpenRouter-compatible embedding model returns another fixed
dimension (1–65536). Dimension is immutable for that fresh collection and the
acceptance helper verifies the configured value while performing a real
embedding/index/search cycle. Model profiles are mutable
Agent-owned database records, so provision the OpenRouter chat and embedding
profiles through the authenticated Core/Capability API using the protected key
files. The embedding profile request must use the exact generated
core_knowledge_embedding_profile_id from agent-config.yaml; the chat profile
may use a separate UUID. Then run:

1. model profile create/list/get (read responses must not return key bytes);
2. model connection test for chat and embedding;
3. one chat request through message-server, followed by an identical
   idempotency retry;
4. Knowledge source upload, index task completion, semantic search, restart,
   and delete;
5. long-term memory create, search/list, restart recall, and delete.

Use the same embedding API key for OpenRouter-compatible embeddings when the
provider permits it; keep the two protected host files separate so rotation
does not require a YAML change. Qdrant is private and its collection name is
unique per fresh Agent instance.

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
`openrouter-api-key`, `embedding-api-key`, and portal-password files, and all
request/session/response/log files are mode 0400 under a mode-0700 temporary
workspace. It talks only to message-server `/_p2p/query` and `/_p2p/health`;
it never calls an Agent or Qdrant listener directly and never prints a token.

The lane verifies both health listeners, Compose port/network isolation,
OpenRouter chat and embedding profile sync, automatic Knowledge embedding
binding, native chat plus identical-turn replay, Product contacts and
prepared-message exact-once replay, forged-owner rejection, Knowledge upload
and automatic indexing/search, long-term memory update/re-index/search,
Agent restart recall, source/memory deletion, final `portal.account.delete`,
database-role/table isolation, Qdrant cleanup, and a key/log/config canary.
There is intentionally no `agent.knowledge.index` workaround: a binding
mismatch is a hard contract failure. Account deletion is the final public
action; post-delete checks use only Compose exec and private volumes.

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

Use compose.yaml alone with a reviewed .env containing:

- immutable digest-pinned application images for message-server and Agent;
- immutable digest-pinned extension-runner and Core workload-runner images
  (required in the rendered production model even when their profiles stay
  disabled);
- immutable digest-pinned PostgreSQL and Qdrant images;
- a unique stack name and unique network/volume names;
- a protected output directory with mode 0700 and secret files mode 0400;
- fresh message-server/Agent instance IDs and the generated positive account
  generation pair (never copy these values into a replacement stack).

Before a production render, create a mode 0400 image attestation containing
the exact seven digest references and the released capability-api v1.0.3
revision, then run:

    deploy/split-agent/scripts/verify-production-images.sh \
      /absolute/path/.run/split/.env \
      /absolute/path/release/image-attestation

Production also requires `DIREXTALK_MESSAGE_TLS_MODE=external`, a trusted
certificate whose SAN/CN matches `DIREXTALK_MESSAGE_SERVER_NAME`, a matching
private key (mode 0400), and a certificate with at least seven days remaining:

    deploy/split-agent/scripts/verify-production-tls.sh \
      /absolute/path/.run/split/.env

The `local` TLS mode intentionally generates a disposable self-signed
certificate and is never a trusted-public production claim. An ALB/reverse
proxy may terminate public TLS, but the deployment still must supply and verify
the internal certificate boundary described above.

Run config --quiet before any migration or service start. The migration services
use the exact same application image as their serving process. A rollback is a
fresh deployment with a matching image and a newly provisioned namespace;
never attach a newer image to an old namespace in this harness.
