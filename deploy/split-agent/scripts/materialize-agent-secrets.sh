#!/bin/sh
set -eu

install -d -m 0700 /out /out-config /out-data/knowledge-content /out-data/knowledge-mount
for item in database_url core_secret_master_key; do
  install -m 0400 "/run/secrets/$item" "/out/$item"
done
install -m 0400 /bootstrap/capability/agent-server-cert.pem /out/tls_cert
install -m 0400 /bootstrap/capability/agent-server-key.pem /out/tls_key
# Keep the established private-volume filename so existing nodes can update
# without rotating the Agent Core service credential. It is materialized only
# as Agent's internal service_token; Message Server never consumes it.
install -m 0400 /bootstrap/capability/ms-to-agent.token /out/service_token
install -m 0400 /bootstrap/capability/grant-public.key /out/grant_public_key
install -m 0400 /bootstrap/capability/ca-cert.pem /out/product_ca
install -m 0400 /bootstrap/capability/agent-client-cert.pem /out/product_tls_cert
install -m 0400 /bootstrap/capability/agent-client-key.pem /out/product_tls_key
install -m 0400 /bootstrap/capability/agent-to-ms.token /out/agent_to_ms_token
install -m 0400 /bootstrap/capability/agent-voice-relay.token /out/voice_relay_token
install -m 0400 /bootstrap/config.yaml /out-config/config.yaml
chown -R 65532:65532 /out /out-config /out-data
chmod 0500 /out-data/knowledge-mount
