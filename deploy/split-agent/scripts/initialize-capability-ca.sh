#!/bin/sh
set -eu

# Generate the private Capability PKI once for a fresh stack.  The signing key
# stays in the init-only authority volume; only the issued material is shared.
authority_dir=${1:?usage: initialize-capability-ca.sh AUTHORITY_DIR SHARED_DIR PRIVATE_DIR}
shared_dir=${2:?usage: initialize-capability-ca.sh AUTHORITY_DIR SHARED_DIR PRIVATE_DIR}
private_dir=${3:?usage: initialize-capability-ca.sh AUTHORITY_DIR SHARED_DIR PRIVATE_DIR}

die() {
  printf 'capability CA initialization: %s\n' "$*" >&2
  exit 1
}

authority_marker=$authority_dir/ready-v1
required_shared='ca-cert.pem agent-server-cert.pem agent-server-key.pem ms-client-cert.pem ms-client-key.pem ms-server-cert.pem ms-server-key.pem agent-client-cert.pem agent-client-key.pem ms-to-agent.token agent-to-ms.token agent-voice-relay.token grant-public.key'
required_private='grant-private.key'

all_present() {
  for item in $required_shared; do [ -s "$shared_dir/$item" ] || return 1; done
  for item in $required_private; do [ -s "$private_dir/$item" ] || return 1; done
}

mkdir -p "$authority_dir" "$shared_dir" "$private_dir"
chmod 0700 "$authority_dir" "$shared_dir" "$private_dir"

if [ -f "$authority_marker" ]; then
  all_present || die 'authority marker exists but issued material is incomplete; refuse implicit rotation'
  exit 0
fi

for directory in "$authority_dir" "$shared_dir" "$private_dir"; do
  find "$directory" -mindepth 1 -maxdepth 1 -print -quit | grep -q . && \
    die "capability material is partially initialized in $directory; refuse implicit rotation"
done

work_dir=$(mktemp -d /tmp/dirextalk-capability-ca.XXXXXX)
cleanup() { rm -rf "$work_dir"; }
trap cleanup EXIT HUP INT TERM
umask 077

cd "$work_dir"
openssl genrsa -out ca-key.pem 4096 2>/dev/null
openssl req -new -x509 -days 3650 -sha256 -key ca-key.pem -out ca-cert.pem \
  -subj '/O=Dirextalk/CN=Dirextalk Capability Root CA'

make_cert() {
  role=$1 cn=$2 eku=$3 san=$4
  openssl genrsa -out "$role-key.pem" 2048 2>/dev/null
  openssl req -new -sha256 -key "$role-key.pem" -out "$role.csr" \
    -subj "/O=Dirextalk/CN=$cn"
  cat >"$role.cnf" <<EOF
[v3_req]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = $eku
subjectAltName = $san
EOF
  openssl x509 -req -sha256 -in "$role.csr" -CA ca-cert.pem -CAkey ca-key.pem \
    -CAcreateserial -out "$role-cert.pem" -days 825 -extensions v3_req -extfile "$role.cnf" >/dev/null
}

make_cert agent-server dirextalk-agent serverAuth 'DNS:dirextalk-agent'
make_cert ms-server dirextalk-message-server serverAuth 'DNS:dirextalk-message-server'
make_cert ms-client message-server-client clientAuth 'DNS:message-server-client'
make_cert agent-client agent-client clientAuth 'DNS:agent-client'
for cert in agent-server-cert.pem ms-server-cert.pem ms-client-cert.pem agent-client-cert.pem; do
  openssl verify -CAfile ca-cert.pem "$cert" >/dev/null
done

openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n' >ms-to-agent.token
openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n' >agent-to-ms.token
openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n' >agent-voice-relay.token
openssl genpkey -algorithm ED25519 -out grant-signing.pem 2>/dev/null
openssl pkey -in grant-signing.pem -outform DER | tail -c 32 >grant-seed.bin
openssl pkey -in grant-signing.pem -pubout -outform DER | tail -c 32 >grant-public.key
cat grant-seed.bin grant-public.key >grant-private.key
[ "$(wc -c <grant-private.key)" -eq 64 ] || die 'generated grant private key has the wrong length'
[ "$(wc -c <grant-public.key)" -eq 32 ] || die 'generated grant public key has the wrong length'

install -m 0400 ca-key.pem "$authority_dir/ca-key.pem"
for item in ca-cert.pem agent-server-cert.pem agent-server-key.pem ms-client-cert.pem ms-client-key.pem \
  ms-server-cert.pem ms-server-key.pem agent-client-cert.pem agent-client-key.pem \
  ms-to-agent.token agent-to-ms.token agent-voice-relay.token grant-public.key; do
  install -m 0400 "$item" "$shared_dir/$item"
done
install -m 0400 grant-private.key "$private_dir/grant-private.key"
printf 'dirextalk-capability-ca-v1\n' >"$authority_marker"
chmod 0400 "$authority_marker"
