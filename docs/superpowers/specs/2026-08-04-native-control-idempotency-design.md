# Native Control Idempotency Fix Design

## Context

Native Agent control writes currently derive their idempotency UUID only from
the authenticated owner, conversation, and turn intent. Every idempotent write
in one turn therefore receives the same key. Execution V2 stores different
write types in a shared idempotency catalog, so a successful analysis can be
replayed as the response to a later target reservation and fail strict decoding
with immutable snapshot drift.

## Goal

Give each distinct authenticated control mutation a deterministic idempotency
identity while retaining stable replay behavior for an identical tool call.

## Non-goals

- Do not expose `idempotency_key` to the model.
- Do not change ProductCore action schemas or database tables.
- Do not change Execution V2 recipe intent validation or error mapping.
- Do not add a trusted Git source analyzer.

## Design

`controlIdempotencyKey` will build its scope from these components in order:

1. trimmed authenticated owner ID;
2. trimmed authenticated conversation ID;
3. trimmed authenticated turn intent;
4. the ProductCore action name;
5. the SHA-256 digest of the canonical JSON representation of the projected,
   validated request before `idempotency_key` is injected.

The text components and hexadecimal request digest will be separated by NUL
bytes before the scope is hashed. The resulting SHA-256 digest remains the
input to the existing deterministic UUID derivation. Go's JSON encoder sorts
string map keys, so equivalent request maps produce stable bytes even when
their insertion order differs.
The request has already passed secret rejection and validation at this point;
only the digest participates in the key, and no request contents are logged or
returned.

The key helper will return an error if the request cannot be encoded. The tool
handler will stop before invoking ProductCore in that case. The generated
project identity will use the same action-scoped helper with an empty request,
preserving its stable per-turn identity.

## Required behavior

- Repeating the same action with the same request in the same authenticated
  turn produces the same UUID.
- Changing the action in the same turn produces a different UUID.
- Changing request parameters for the same action and turn produces a different
  UUID.
- Changing only map insertion order does not change the UUID.
- Read-only tools still receive no idempotency key.
- Model-supplied idempotency keys remain rejected.

## Tests

Replace the test that intentionally requires one key per entire turn with
focused tests for identical replay, action separation, parameter separation,
and map-order stability. Retain the existing tool-handler tests that verify a
canonical UUID is injected and that model-controlled keys are rejected.

Run the focused `p2p/internal/agent` tests first, then the full `p2p` package
tests and production binary build required by the repository guidance.
