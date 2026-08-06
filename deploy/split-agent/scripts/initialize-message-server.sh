#!/bin/sh
set -eu

die() {
  printf 'message-server initialization: %s\n' "$*" >&2
  exit 1
}

config_dir=${MESSAGE_CONFIG_DIR:-/etc/dirextalk-message-server}
data_dir=${MESSAGE_DATA_DIR:-/var/dirextalk-message-server}
registration_secret=${MESSAGE_REGISTRATION_SECRET_FILE:-/run/secrets/message_registration_shared_secret}
turn_secret_file=${MESSAGE_TURN_SHARED_SECRET_FILE:-/run/secrets/turn_shared_secret}
external_cert=${MESSAGE_EXTERNAL_TLS_CERT_FILE:-/bootstrap/external/server.crt}
external_key=${MESSAGE_EXTERNAL_TLS_KEY_FILE:-/bootstrap/external/server.key}
generate_keys=${MESSAGE_GENERATE_KEYS_BINARY:-/usr/bin/generate-keys}
generate_config=${MESSAGE_GENERATE_CONFIG_BINARY:-/usr/bin/generate-config}
capability_initializer=${MESSAGE_CAPABILITY_INITIALIZER:-/usr/local/bin/initialize-capability-ca}
capability_authority_dir=${MESSAGE_CAPABILITY_AUTHORITY_DIR:-/var/lib/dirextalk-message-server/capability-authority}
capability_shared_dir=${MESSAGE_CAPABILITY_SHARED_DIR:-/var/lib/dirextalk-message-server/capability}
capability_private_dir=${MESSAGE_CAPABILITY_PRIVATE_DIR:-/var/lib/dirextalk-message-server/capability-private}

install -d -m 0700 "$config_dir" "$data_dir" "$data_dir/agent"
if [ ! -f "$config_dir/matrix_key.pem" ]; then
  "$generate_keys" -private-key "$config_dir/matrix_key.pem"
fi

deployment_mode=${MESSAGE_DEPLOYMENT_MODE:?set MESSAGE_DEPLOYMENT_MODE}
case "$deployment_mode" in
  local|production) ;;
  *) die 'MESSAGE_DEPLOYMENT_MODE must be local or production' ;;
esac

case ${MESSAGE_SERVER_TLS_MODE:?set MESSAGE_SERVER_TLS_MODE} in
  edge-terminated)
    [ "$deployment_mode" = production ] || \
      die 'edge-terminated TLS is valid only for production'
    test ! -s "$external_cert" || die 'edge-terminated TLS forbids a message-server certificate'
    test ! -s "$external_key" || die 'edge-terminated TLS forbids a message-server private key'
    rm -f "$config_dir/server.crt" "$config_dir/server.key"
    ;;
  external)
    test -s "$external_cert" || die 'external TLS certificate is missing or empty'
    test -s "$external_key" || die 'external TLS private key is missing or empty'
    install -m 0400 "$external_cert" "$config_dir/server.crt"
    install -m 0400 "$external_key" "$config_dir/server.key"
    ;;
  local)
    [ "$deployment_mode" = local ] || die 'local TLS is forbidden for production'
    if [ ! -f "$config_dir/server.crt" ] || [ ! -f "$config_dir/server.key" ]; then
      "$generate_keys" -tls-cert "$config_dir/server.crt" -tls-key "$config_dir/server.key" \
        -server "${MESSAGE_SERVER_NAME:?set MESSAGE_SERVER_NAME}"
    fi
    ;;
  *) die 'MESSAGE_SERVER_TLS_MODE must be local, external, or edge-terminated' ;;
esac

"$generate_config" -dir "$data_dir" -db '__DIREXTALK_DB_DSN__' \
  -server "${MESSAGE_SERVER_NAME:?set MESSAGE_SERVER_NAME}" >"$config_dir/message-server.yaml"

# Populate the generated TURN section without ever placing the shared secret
# in a child process environment or argv. The shell builtins below keep the
# value in this init process and write the final protected config directly.
test -s "$turn_secret_file" || die 'TURN shared secret is missing or empty'
IFS= read -r turn_secret <"$turn_secret_file" || die 'TURN shared secret cannot be read'
case "$turn_secret" in
  *[!0-9a-f]*) die 'TURN shared secret must be lowercase hexadecimal' ;;
esac
[ "${#turn_secret}" -eq 64 ] || die 'TURN shared secret must contain exactly 32 bytes'
turn_config_tmp=$config_dir/.message-server.yaml.turn.$$
turn_lifetime_count=0
turn_uris_count=0
turn_secret_count=0
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    '    turn_user_lifetime:'*)
      printf '%s\n' '    turn_user_lifetime: "24h"' >>"$turn_config_tmp"
      turn_lifetime_count=$((turn_lifetime_count + 1))
      ;;
    '    turn_uris:'*)
      printf '%s\n' \
        '    turn_uris:' \
        "      - turn:${MESSAGE_SERVER_NAME}:3478?transport=udp" \
        "      - turn:${MESSAGE_SERVER_NAME}:3478?transport=tcp" >>"$turn_config_tmp"
      turn_uris_count=$((turn_uris_count + 1))
      ;;
    '    turn_shared_secret:'*)
      printf '    turn_shared_secret: "%s"\n' "$turn_secret" >>"$turn_config_tmp"
      turn_secret_count=$((turn_secret_count + 1))
      ;;
    *) printf '%s\n' "$line" >>"$turn_config_tmp" ;;
  esac
done <"$config_dir/message-server.yaml"
[ "$turn_lifetime_count" -eq 1 ] || die 'generated config must contain one TURN lifetime field'
[ "$turn_uris_count" -eq 1 ] || die 'generated config must contain one TURN URI field'
[ "$turn_secret_count" -eq 1 ] || die 'generated config must contain one TURN shared-secret field'
chmod 0400 "$turn_config_tmp"
mv -f "$turn_config_tmp" "$config_dir/message-server.yaml"
unset turn_secret

case ${MESSAGE_LOCAL_BOOTSTRAP_ENABLED:?set MESSAGE_LOCAL_BOOTSTRAP_ENABLED} in
  true)
    test -s "$registration_secret" || die 'local bootstrap shared secret is missing or empty'
    secret=$(cat "$registration_secret")
    if grep -Eq '^  registration_shared_secret:' "$config_dir/message-server.yaml"; then
      sed -i "s|^  registration_shared_secret:.*|  registration_shared_secret: \"$secret\"|" "$config_dir/message-server.yaml"
    elif grep -Eq '^client_api:' "$config_dir/message-server.yaml"; then
      sed -i "/^client_api:/a\\  registration_shared_secret: \"$secret\"" "$config_dir/message-server.yaml"
    else
      printf '\nclient_api:\n  registration_shared_secret: "%s"\n' "$secret" >>"$config_dir/message-server.yaml"
    fi
    unset secret
    ;;
  false) ;;
  *) die 'MESSAGE_LOCAL_BOOTSTRAP_ENABLED must be true or false' ;;
esac

sed -i "s|well_known_client_name: .*|well_known_client_name: \"${MESSAGE_CLIENT_BASE_URL:?set MESSAGE_CLIENT_BASE_URL}\"|" "$config_dir/message-server.yaml"
"$capability_initializer" \
  "$capability_authority_dir" \
  "$capability_shared_dir" \
  "$capability_private_dir"
chmod 0400 "$config_dir/message-server.yaml"
