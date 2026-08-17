#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd -P)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/bin"
cat >"$tmp/.env" <<EOF
DIREXTALK_SPLIT_STACK_NAME=d-abcdefghijklmnopqrstuvwxyz
DIREXTALK_MESSAGE_SERVER_IMAGE=docker.io/dirextalk/message-server:v1.1.33
DIREXTALK_MESSAGE_SERVER_VERSION=v1.1.33
DIREXTALK_MESSAGE_SOURCE_REVISION=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
DIREXTALK_AGENT_IMAGE=docker.io/dirextalk/agent:v1.0.69
DIREXTALK_AGENT_VERSION=v1.0.69
DIREXTALK_AGENT_SOURCE_REVISION=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
DIREXTALK_POSTGRES_IMAGE_IMMUTABLE=docker.io/pgvector/pgvector:pg18@sha256:1111111111111111111111111111111111111111111111111111111111111111
DIREXTALK_UTILITY_IMAGE_IMMUTABLE=docker.io/pgvector/pgvector:pg18@sha256:1111111111111111111111111111111111111111111111111111111111111111
DIREXTALK_COTURN_IMAGE_IMMUTABLE=docker.io/coturn/coturn:4.6.3-alpine@sha256:1111111111111111111111111111111111111111111111111111111111111111
EOF
chmod 400 "$tmp/.env"
cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
case "$1 $2" in
  'image inspect') case "$3" in *message-server*) printf 'v1.1.33|aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n';; *agent*) printf 'v1.0.69|bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n';; sha256:message*) printf 'v1.1.33|aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n';; sha256:agent*) printf 'v1.0.69|bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n';; esac ;;
  'run --rm') case "$*" in *message-server*) echo v1.1.33;; *) echo v1.0.69;; esac ;;
  'ps -q') case "$*" in *message-server*) echo message-id;; *extension-runner*) echo extension-id;; *core-runner*) echo core-id;; *agent*) echo agent-id;; esac ;;
  'inspect message-id') printf '[{"Config":{"Image":"docker.io/dirextalk/message-server:v1.1.33"},"Image":"sha256:message","State":{"Health":{"Status":"healthy"}}}]\n' ;;
  'inspect agent-id'|'inspect extension-id'|'inspect core-id') printf '[{"Config":{"Image":"docker.io/dirextalk/agent:v1.0.69"},"Image":"sha256:agent","State":{"Health":{"Status":"healthy"}}}]\n' ;;
  'exec message-id') echo v1.1.33 ;;
  'exec agent-id'|'exec extension-id'|'exec core-id') echo v1.0.69 ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$tmp/bin/docker"
PATH="$tmp/bin:$PATH" "$script_dir/verify-production-images.sh" "$tmp/.env" >/dev/null
PATH="$tmp/bin:$PATH" "$script_dir/verify-production-images.sh" "$tmp/.env" --running >/dev/null
sed -i 's#dirextalk/agent:v1.0.69#dirextalk/agent:v1.0.68#' "$tmp/.env"
if PATH="$tmp/bin:$PATH" "$script_dir/verify-production-images.sh" "$tmp/.env" >/dev/null 2>&1; then echo 'mismatched Agent version tag unexpectedly accepted' >&2; exit 1; fi
printf 'production version-tag image and running version checks verified\n'
