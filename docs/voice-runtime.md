# Native Agent voice runtime

Voice sessions use a server-owned `speech` model profile (`provider:
`volc_voice`) and a server-owned `conversation` profile. The client sends only
the two profile IDs (or relies on the role defaults); API keys and Volc secrets
never cross the ProductCore response boundary.

Deployment configuration must provide all of the following: a non-empty
`VOLC_VOICE_WEBHOOK_SECRET`, an HTTPS `VOLC_VOICE_WEBHOOK_URL`, and an HTTPS
`VOLC_VOICE_CUSTOM_LLM_URL`. A valid encrypted server-owned `speech` profile is
also required. The callback URL is bound to an expiring session HMAC. RTC app
ID, app key, Volc access key and secret access key are stored in the encrypted
speech profile credential bundle. `agent.backends.get` advertises `voice.server`
only when these URLs, the webhook secret, and the profile store are available.

Client transcript submission remains disabled unless
`VOLC_VOICE_CLIENT_TRANSCRIPT_SUBMIT_ENABLED=true`; Volc CustomLLM callbacks
are the normal ASR path. Each callback maps deterministically to a durable
`agent.chat.stream` turn in the selected conversation. `turn.done` completes a
turn; `session.done` is reserved for session termination.

Session interrupt first calls the provider interrupt operation, then stops the
durable turn. Provider Stop is issued on session end and expiry.
