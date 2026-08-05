#!/bin/sh
set -eu

die() {
  printf 'message-server initialization: %s\n' "$*" >&2
  exit 1
}

config_dir=/etc/dirextalk-message-server
data_dir=/var/dirextalk-message-server
registration_secret=/run/secrets/message_registration_shared_secret

install -d -m 0700 "$config_dir" "$data_dir" "$data_dir/agent"
if [ ! -f "$config_dir/matrix_key.pem" ]; then
  /usr/bin/generate-keys -private-key "$config_dir/matrix_key.pem"
fi

case ${MESSAGE_SERVER_TLS_MODE:?set MESSAGE_SERVER_TLS_MODE} in
  external)
    test -s /bootstrap/external/server.crt || die 'external TLS certificate is missing or empty'
    test -s /bootstrap/external/server.key || die 'external TLS private key is missing or empty'
    install -m 0400 /bootstrap/external/server.crt "$config_dir/server.crt"
    install -m 0400 /bootstrap/external/server.key "$config_dir/server.key"
    ;;
  local)
    if [ ! -f "$config_dir/server.crt" ] || [ ! -f "$config_dir/server.key" ]; then
      /usr/bin/generate-keys -tls-cert "$config_dir/server.crt" -tls-key "$config_dir/server.key" \
        -server "${MESSAGE_SERVER_NAME:?set MESSAGE_SERVER_NAME}"
    fi
    ;;
  *) die 'MESSAGE_SERVER_TLS_MODE must be local or external' ;;
esac

/usr/bin/generate-config -dir "$data_dir" -db '__DIREXTALK_DB_DSN__' \
  -server "${MESSAGE_SERVER_NAME:?set MESSAGE_SERVER_NAME}" >"$config_dir/message-server.yaml"

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
/usr/local/bin/initialize-capability-ca \
  /var/lib/dirextalk-message-server/capability-authority \
  /var/lib/dirextalk-message-server/capability \
  /var/lib/dirextalk-message-server/capability-private
chmod 0400 "$config_dir/message-server.yaml"
