#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd -P)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

authority=$tmp/authority
shared=$tmp/shared
private=$tmp/private
"$script_dir/initialize-capability-ca.sh" "$authority" "$shared" "$private"

test "$(stat -c '%a' "$authority/ca-key.pem")" = 400
test "$(stat -c '%a' "$private/grant-private.key")" = 400
test "$(wc -c <"$private/grant-private.key")" = 64
test "$(wc -c <"$shared/grant-public.key")" = 32
test ! -e "$shared/ca-key.pem"
test ! -e "$shared/grant-private.key"
for cert in agent-server-cert.pem ms-server-cert.pem ms-client-cert.pem agent-client-cert.pem; do
  openssl verify -CAfile "$shared/ca-cert.pem" "$shared/$cert" >/dev/null
done

before=$(sha256sum "$shared/ca-cert.pem" "$private/grant-private.key")
"$script_dir/initialize-capability-ca.sh" "$authority" "$shared" "$private"
after=$(sha256sum "$shared/ca-cert.pem" "$private/grant-private.key")
test "$before" = "$after"

mkdir "$tmp/partial"
touch "$tmp/partial/unexpected"
if "$script_dir/initialize-capability-ca.sh" "$tmp/partial" "$tmp/other-shared" "$tmp/other-private"; then
  echo "Capability initializer accepted a partial authority volume" >&2
  exit 1
fi

echo "capability CA initialization ok"
