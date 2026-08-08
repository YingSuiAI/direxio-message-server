#!/bin/sh
set -eu

die() { printf '%s\n' "$*" >&2; exit 64; }
require() { eval "value=\${$1:-}"; [ -n "$value" ] || die "$1 is required"; }
hostname_ok() {
  printf '%s' "$1" | grep -Eq '^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$' &&
    printf '%s' "$1" | grep -q '\.'
}
upstream_ok() {
  printf '%s' "$1" | grep -Eq '^([a-z0-9][a-z0-9.-]*|[0-9]{1,3}(\.[0-9]{1,3}){3}):([1-9][0-9]{0,4})$'
}

[ "$#" -eq 1 ] || die "usage: render-cloud-worker-edge.sh OUTPUT_DIRECTORY"
out=$1
[ ! -e "$out" ] || die "output path already exists: $out"

region=${DIREXTALK_CLOUD_WORKER_REGION:-ap-east-1}
mode=${DIREXTALK_CLOUD_WORKER_ENDPOINT_MODE:-}
case "$region" in *[!a-z0-9-]*|'') die "DIREXTALK_CLOUD_WORKER_REGION is invalid" ;; esac
case "$mode" in
  private|controlled-public) ;;
  *) die "DIREXTALK_CLOUD_WORKER_ENDPOINT_MODE must explicitly be private or controlled-public" ;;
esac

for name in DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME DIREXTALK_CLOUD_WORKER_PROXY_HOSTNAME DIREXTALK_CLOUD_WORKER_CONTROL_UPSTREAM DIREXTALK_CLOUD_WORKER_MODEL_RELAY_UPSTREAM DIREXTALK_CLOUD_WORKER_PROXY_UPSTREAM DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS; do
  require "$name"
done
control=$DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME
relay=$DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME
proxy=$DIREXTALK_CLOUD_WORKER_PROXY_HOSTNAME
hostname_ok "$control" || die "DIREXTALK_CLOUD_WORKER_CONTROL_HOSTNAME is invalid"
hostname_ok "$relay" || die "DIREXTALK_CLOUD_WORKER_MODEL_RELAY_HOSTNAME is invalid"
hostname_ok "$proxy" || die "DIREXTALK_CLOUD_WORKER_PROXY_HOSTNAME is invalid"
[ "$control" != "$relay" ] && [ "$control" != "$proxy" ] && [ "$relay" != "$proxy" ] || die "the three Cloud Worker hostnames must be distinct"
upstream_ok "$DIREXTALK_CLOUD_WORKER_CONTROL_UPSTREAM" || die "DIREXTALK_CLOUD_WORKER_CONTROL_UPSTREAM must be exact HOST:PORT"
upstream_ok "$DIREXTALK_CLOUD_WORKER_MODEL_RELAY_UPSTREAM" || die "DIREXTALK_CLOUD_WORKER_MODEL_RELAY_UPSTREAM must be exact HOST:PORT"
upstream_ok "$DIREXTALK_CLOUD_WORKER_PROXY_UPSTREAM" || die "DIREXTALK_CLOUD_WORKER_PROXY_UPSTREAM must be exact HOST:PORT"

allowed_fqdns=
control_present=false
relay_present=false
for fqdn in $DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS; do
  hostname_ok "$fqdn" && printf '%s' "$fqdn" | grep -q '[a-z]' || die "DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS contains an invalid exact FQDN"
  case " $allowed_fqdns " in *" $fqdn "*) die "DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS contains a duplicate" ;; esac
  allowed_fqdns="${allowed_fqdns:+$allowed_fqdns }$fqdn"
  [ "$fqdn" != "$control" ] || control_present=true
  [ "$fqdn" != "$relay" ] || relay_present=true
done
[ "$control_present" = true ] && [ "$relay_present" = true ] || die "DIREXTALK_CLOUD_WORKER_ALLOWED_FQDNS must contain WorkerControl and Model Relay"

umask 077
mkdir "$out"
cat >"$out/haproxy.cfg" <<EOF
global
  log stdout format raw local0
  maxconn 1024
defaults
  mode tcp
  log global
  timeout connect 5s
  timeout client 1m
  timeout server 1m
frontend cloud_worker_tls
  bind :443
  tcp-request inspect-delay 5s
  tcp-request content accept if { req.ssl_hello_type 1 }
  acl sni_worker_control req.ssl_sni -i $control
  acl sni_model_relay req.ssl_sni -i $relay
  acl sni_controlled_proxy req.ssl_sni -i $proxy
  tcp-request content reject unless sni_worker_control or sni_model_relay or sni_controlled_proxy
  use_backend worker_control if sni_worker_control
  use_backend model_relay if sni_model_relay
  use_backend controlled_proxy if sni_controlled_proxy
backend worker_control
  server agent $DIREXTALK_CLOUD_WORKER_CONTROL_UPSTREAM check
backend model_relay
  server agent $DIREXTALK_CLOUD_WORKER_MODEL_RELAY_UPSTREAM check
backend controlled_proxy
  server proxy $DIREXTALK_CLOUD_WORKER_PROXY_UPSTREAM check
EOF

cat >"$out/squid.conf" <<EOF
https_port 127.0.0.1:12443 tls-cert=/run/secrets/proxy-tls-cert.pem tls-key=/run/secrets/proxy-tls-key.pem
acl CONNECT method CONNECT
acl TLS_port port 443
acl permitted_inner_tls dstdomain $allowed_fqdns
acl local_destination dst 127.0.0.0/8 ::1
http_access deny local_destination
http_access allow CONNECT TLS_port permitted_inner_tls
http_access deny all
cache deny all
access_log stdio:/dev/stdout
cache_log /dev/stderr
forwarded_for delete
visible_hostname $proxy
EOF

cat >"$out/endpoints.env" <<EOF
DIREXTALK_CLOUD_WORKER_REGION=$region
DIREXTALK_CLOUD_WORKER_ENDPOINT_MODE=$mode
DIREXTALK_CLOUD_WORKER_CONTROL_ENDPOINT=https://$control:443
DIREXTALK_CLOUD_WORKER_MODEL_RELAY_ENDPOINT=https://$relay:443
DIREXTALK_CLOUD_WORKER_OUTBOUND_PROXY_URL=https://$proxy:443
EOF
chmod 0444 "$out/haproxy.cfg" "$out/squid.conf"
chmod 0400 "$out/endpoints.env"
printf '%s\n' "$out"
