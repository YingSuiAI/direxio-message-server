#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd -P)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
export DIREXTALK_CLOUD_WORKER_CONTROL_UPSTREAM=10.0.1.20:10443
export DIREXTALK_CLOUD_WORKER_MODEL_RELAY_UPSTREAM=10.0.1.20:11443
export DIREXTALK_CLOUD_WORKER_PROXY_UPSTREAM=cloud-worker-controlled-proxy:12443

render() {
  DIREXTALK_CLOUD_WORKER_ENDPOINT_MODE=$1 \
  DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME=worker.example.internal \
  DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME=model.example.internal \
  DIREXTALK_CLOUD_WORKER_PROXY_HOSTNAME=proxy.example.internal \
  DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS='worker.example.internal model.example.internal bucket.s3.ap-east-1.amazonaws.com sts.ap-east-1.amazonaws.com' \
    "$script_dir/render-cloud-worker-edge.sh" "$2" >/dev/null
}

render private "$tmp/private"
grep -Fqx 'DIREXTALK_CLOUD_WORKER_REGION=ap-east-1' "$tmp/private/endpoints.env"
grep -Fqx 'DIREXTALK_CLOUD_WORKER_ENDPOINT_MODE=private' "$tmp/private/endpoints.env"
grep -Fqx 'DIREXTALK_CLOUD_WORKER_CONTROL_ENDPOINT=https://worker.example.internal:443' "$tmp/private/endpoints.env"
grep -Fqx '  server agent 10.0.1.20:10443 check' "$tmp/private/haproxy.cfg"
grep -Fqx '  server agent 10.0.1.20:11443 check' "$tmp/private/haproxy.cfg"
grep -Fqx '  server proxy cloud-worker-controlled-proxy:12443 check' "$tmp/private/haproxy.cfg"
grep -Fqx 'http_access allow CONNECT TLS_port permitted_inner_tls' "$tmp/private/squid.conf"
grep -Fqx 'https_port 127.0.0.1:12443 tls-cert=/run/secrets/proxy-tls-cert.pem tls-key=/run/secrets/proxy-tls-key.pem' "$tmp/private/squid.conf"
grep -Fqx 'acl permitted_inner_tls dstdomain worker.example.internal model.example.internal bucket.s3.ap-east-1.amazonaws.com sts.ap-east-1.amazonaws.com' "$tmp/private/squid.conf"
grep -Fqx 'http_access deny all' "$tmp/private/squid.conf"
grep -Fqx 'visible_hostname proxy.example.internal' "$tmp/private/squid.conf"
[ "$(stat -c '%a' "$tmp/private/haproxy.cfg")" = 444 ]
[ "$(stat -c '%a' "$tmp/private/squid.conf")" = 444 ]
[ "$(stat -c '%a' "$tmp/private/endpoints.env")" = 400 ]

DIREXTALK_CLOUD_WORKER_REGION=ap-southeast-2 \
  DIREXTALK_CLOUD_WORKER_ENDPOINT_MODE=controlled-public \
  DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME=worker.example.com \
  DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME=model.example.com \
  DIREXTALK_CLOUD_WORKER_PROXY_HOSTNAME=proxy.example.com \
  DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS='worker.example.com model.example.com bucket.s3.ap-southeast-2.amazonaws.com sts.ap-southeast-2.amazonaws.com' \
  "$script_dir/render-cloud-worker-edge.sh" "$tmp/public" >/dev/null
grep -Fqx 'DIREXTALK_CLOUD_WORKER_REGION=ap-southeast-2' "$tmp/public/endpoints.env"
grep -Fqx 'DIREXTALK_CLOUD_WORKER_ENDPOINT_MODE=controlled-public' "$tmp/public/endpoints.env"

if DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME=worker.example.com \
  DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME=model.example.com \
  DIREXTALK_CLOUD_WORKER_PROXY_HOSTNAME=proxy.example.com \
  DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS='worker.example.com model.example.com bucket.s3.ap-east-1.amazonaws.com sts.ap-east-1.amazonaws.com' \
  "$script_dir/render-cloud-worker-edge.sh" "$tmp/implicit" >/dev/null 2>&1; then
  echo "implicit endpoint selection unexpectedly passed" >&2
  exit 1
fi

if DIREXTALK_CLOUD_WORKER_ENDPOINT_MODE=private \
  DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME=same.example.com \
  DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME=same.example.com \
  DIREXTALK_CLOUD_WORKER_PROXY_HOSTNAME=proxy.example.com \
  DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS='same.example.com proxy.example.com bucket.s3.ap-east-1.amazonaws.com sts.ap-east-1.amazonaws.com' \
  "$script_dir/render-cloud-worker-edge.sh" "$tmp/duplicate" >/dev/null 2>&1; then
  echo "duplicate TLS identities unexpectedly passed" >&2
  exit 1
fi

if DIREXTALK_CLOUD_WORKER_ENDPOINT_MODE=private \
  DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME=worker.example.com \
  DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME=model.example.com \
  DIREXTALK_CLOUD_WORKER_PROXY_HOSTNAME=proxy.example.com \
  DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS='worker.example.com model.example.com *.amazonaws.com' \
  "$script_dir/render-cloud-worker-edge.sh" "$tmp/wildcard" >/dev/null 2>&1; then
  echo "wildcard CONNECT authority unexpectedly passed" >&2
  exit 1
fi

if DIREXTALK_CLOUD_WORKER_ENDPOINT_MODE=private \
  DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME=worker.example.com \
  DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME=model.example.com \
  DIREXTALK_CLOUD_WORKER_PROXY_HOSTNAME=proxy.example.com \
  DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS='worker.example.com model.example.com model.example.com' \
  "$script_dir/render-cloud-worker-edge.sh" "$tmp/repeated" >/dev/null 2>&1; then
  echo "duplicate CONNECT authority unexpectedly passed" >&2
  exit 1
fi

printf '%s\n' 'Cloud Worker edge rendering tests passed'
