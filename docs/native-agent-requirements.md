# Native Agent Requirements

> Current contract: Native Agent is exposed through first-class owner `agent.*`
> product actions and realtime `client.native_agent_stream` /
> `client.native_agent_stream.cancel` frames. `io.dirextalk.agent` is not
> listed, installed, enabled, configured, invoked, checked for health, or tailed
> through the plugin catalog/list/lifecycle/invoke/log surfaces. Ops and future
> non-Agent plugins continue to use the plugin manager and Docker runner.
> Native Agent config storage uses the portal Agent config JSON; old hidden
> `io.dirextalk.agent` plugin config is only a sanitized, idempotent startup
> migration source.

## Scope

Dirextalk Message Server exposes Native Agent as a first-class server feature. Its existing local Runner remains a compatibility path, while Chat, StreamChat, and owner-selected runtime model profile can be delegated to the independent `dirextalk-agent` service. The old Agent Docker/plugin-runtime reuse path is deprecated for Agent only. `dirextalk-plugins` is not changed in this version, and non-Agent plugins such as `io.dirextalk.ops` may continue to use the Docker plugin runner.

Clients use the current call surface:

- First-class owner `agent.*` body actions for Native Agent chat, model listing, runtime, skills, MCP, context compression, config patch proposal, and built-in Dirextalk tools. The remote runtime profile uses the owner-only, HTTP-only `agent.runtime.profile.get` and `agent.runtime.profile.update` actions.
- `client.native_agent_stream` over realtime WebSocket for Native Agent streaming, with `client.native_agent_stream.cancel` for cancellation.
- Standard external MCP clients call `POST /mcp` with MCP Streamable HTTP JSON-RPC and `Authorization: Bearer <agent_token>`.
- Fixed `mcp.*` body actions are removed from `/_p2p/query` and `/_p2p/command`; Native Agent Dirextalk tools and `POST /mcp` both call the shared `internal/dirextalkmcp` service.
- Plugin manager actions remain for Ops and future non-Agent plugins only.

## Runtime Requirements

- Native Agent owner actions never route to a Docker Agent container. Existing local Runner actions remain local; remote Chat, StreamChat, and runtime-profile selection use the independent Agent only when its fail-closed gRPC boundary is configured.
- Local Runner config remains in native portal Agent config storage, and `agent.config.update` is not a model profile store. For the independent Agent path, the Agent deployment owns an immutable, secret-free runtime profile catalog and durable runtime selection. On startup, old hidden Agent plugin config/runtime state is imported once in a sanitized, idempotent way; current clients must not use plugin management as the Native Agent contract.
- Native Agent uses CloudWeGo Eino as the only model orchestration path. The runtime must track the latest stable Eino release, use Eino ReAct for model/tool loops, use maintained Eino model components for OpenAI and DeepSeek, use direct-only Anthropic Messages API as an Eino `ToolCallingChatModel` adapter, and use Eino official MCP tooling backed by `modelcontextprotocol/go-sdk`.
- Native Agent supports `openai`, `anthropic`, `deepseek`, and `openai_compatible`.
- `anthropic` first-version support is direct Anthropic API only. Bedrock and Vertex are intentionally not supported, and AWS/Google SDK dependencies must not be introduced for this provider.
- Legacy request-scoped `model_profile` remains local Runner compatibility only. Remote Chat rejects both `model_profile_id` and `model_profile`; clients select the independent Agent profile through `agent.runtime.profile.get/update` and omit all profile and key fields from chat.
- `agent.models.list` remains a local Runner compatibility action that uses request-scoped provider/base URL/key data. It must not configure or discover the remote Agent catalog, persist or echo API keys, or invent model defaults. Remote clients use only the sorted `available_profile_ids` returned by `agent.runtime.profile.get`.
- The remote runtime-profile update accepts only canonical UUID `idempotency_key`, catalog `profile_id`, nonnegative `expected_revision`, and optional `temperature`, `top_p`, and `max_output_tokens`. Its exact de-secreted response is `{available, configured, revision, available_profile_ids, profile}`, where `profile` is `null` or exactly `{profile_id, provider, model, base_url, temperature, top_p, max_output_tokens, context_window, reasoning_effort}`. Immutable model metadata, owner identity, secret references, credential presence, and keys are never exposed.
- The Agent service key used by Message Server must authorize `runtime.read`, `runtime.write`, and `runtime.chat`. A model credential enters the independent Agent only through an operator-provisioned read-only mounted secret file; it must not enter Flutter, a ProductCore request, Message Server storage/logging, Agent gRPC, command arguments, or environment variables.
- The first remote profile selection creates the Agent runtime config with 48 context messages, 12 memory messages, and 24 steps. Later profile changes preserve non-profile runtime configuration. If an update result is ambiguous, the client must reconcile through GET and the current revision/state rather than rebuilding and blindly replaying the mutation.
- DeepSeek defaults to the OpenAI-compatible endpoint `https://api.deepseek.com` in the local compatibility path; the remote endpoint is catalog-owned.
- System prompts start with the built-in Dirextalk Native Agent product rules, then append native Agent config, request overrides, and enabled static skills. User-provided prompts must not override the built-in product or tool-use rules.
- `agent.chat` returns a complete response.
- Native stream emits `delta`, `error`, `trace`, and `done` events through `server.native_agent_stream.*` frames and respects client cancellation.
- Remote Chat normally carries no Cloud scope. For only the closed Worker control-plane diagnostic intent, Message Server may read owner-scoped Connections through the Agent service key, require exactly one canonical `active` Connection, and attach that server-derived Connection to `CloudDialogueScopeV1`. Client parameters cannot select a Connection, Recipe, provider resource, or approval. Ordinary chat and real OpenClaw/Hermes/Knowledge/database/install/build/training workloads perform no diagnostic Cloud lookup, and no diagnostic request may launch compute without a separate quote and device approval.
- Chat responses and stream completion payloads expose observable `steps` and `trace` data for UI display of context use, tool calls, tool results, and final output. Streamed chats also emit a `trace` event before `done`.
- Trace data must not expose hidden model chain-of-thought. It is limited to observable runtime progress, tool inputs/outputs, context metadata, and final answer previews.

## Native Tools

The runtime exposes Dirextalk tools generated from the shared `internal/dirextalkmcp` registry and invoked through the same capability service as the standard `POST /mcp` endpoint:

- `agent.contacts.list`
- `agent.contacts.search`
- `agent.rooms.search`
- `agent.messages.list`
- `agent.messages.send`
- `agent.room_members.list`
- `agent.channel_posts.list`
- `agent.channel_comments.list`
- `agent.channel_comments.create`
- `agent.summarize`

Matrix writes must continue through roomserver/`p2p.Transport`. Direct DB access is read-only and only for context/history/state material.

The external `POST /mcp` transport must call the same `internal/dirextalkmcp` service as these built-in tools. Do not duplicate Dirextalk MCP business logic in Native Agent or the MCP HTTP transport. Fixed `mcp.*` body-action wrappers are removed from `/_p2p/query` and `/_p2p/command`; remaining `mcp.*` strings are internal capability action IDs.

## Skills

- Skills can be installed, listed, enabled, disabled, and uninstalled.
- Installed skill content is cached below the native Agent data directory.
- Only static `SKILL.md` text is read into the prompt. Remote scripts or arbitrary skill code are not executed.
- Skill install supports explicit `content` and URL/GitHub raw retrieval.
- Agent conversations expose native skill management tools, so the model can install, list, enable, disable, and uninstall skills when the user explicitly asks for that operation. These management tools are base Agent capabilities and remain available even when older `enabled_tools` config/request values list only Dirextalk content tools.
- Skill install requests that look like CLI examples, such as `npx skills add https://github.com/owner/repo --skill name`, should be handled through `native_agent_skills_install` rather than shell. When given `repo_url` plus `name` or `id`, the backend tries common GitHub monorepo paths such as `skills/<name>/SKILL.md`, `<name>/SKILL.md`, and root `SKILL.md`.
- A newly installed or re-enabled skill affects the next Agent turn after the system prompt is rebuilt.

## MCP

- Third-party MCP servers can be installed, listed, enabled, disabled, and uninstalled.
- Supported transports are `stdio`, remote HTTP/SSE, and streamable HTTP.
- Dirextalk's own standard MCP server endpoint is `POST /mcp`. It supports JSON-RPC `initialize`, `tools/list`, and `tools/call` over POST, requires `Authorization: Bearer <agent_token>`, rejects query-string tokens, validates `Origin`, returns 405 for GET/SSE while server-to-client streaming is unused, and must not pass the inbound bearer token to downstream services.
- MCP tools discovered from enabled servers become dynamic Agent tools.
- MCP discovery and tool invocation must go through `github.com/cloudwego/eino-ext/components/tool/mcp/officialmcp` and `github.com/modelcontextprotocol/go-sdk/mcp`, not a custom JSON-RPC client.
- MCP server command/env configuration may be stored, but secrets must be passed through request-local values or temporary env references.
- Agent conversations expose native MCP server management tools, so the model can install, list, enable, disable, and uninstall MCP servers when the user explicitly asks for that operation. These management tools are base Agent capabilities and remain available even when older `enabled_tools` config/request values list only Dirextalk content tools.
- MCP tools discovered during a dialogue install become callable on the next Agent turn after the Eino tool list is rebuilt.

## Runtime CLI Tools

- Runtime CLI tools can be installed, recorded, found, and executed under the native Agent data directory.
- Supported actions include install, inspect, which, and run.
- Execution is bounded by timeout and returns stdout/stderr/exit status.
- Install and run commands must work in minimal Alpine runtime images that provide `sh` but not `bash`; the official runtime image also installs `bash` for bash-based deployment scripts.
- Timed-out install and run commands must clean up their child process groups so dynamic dependency installs cannot continue indefinitely after the request is cancelled.
- Enabled installed runtime CLI tools can be exposed to the Agent as Eino tools without a request confirmation field, so the model can call them inside the same orchestration loop and summarize their results.
- Agent conversations can expose a built-in `runtime__shell` Eino tool for explicit command execution requests without a request confirmation field. Multi-step runtime workflows use a default 48-tool-call / 100-graph-step budget, accept configurable `max_tool_calls` or `max_steps`, and cap explicit graph steps at 240 server-side.
- Runtime CLI, shell, and stdio MCP child processes must use a reduced runtime environment rather than inheriting all message-server environment variables. Stdio MCP servers may receive only explicitly configured extra `env` values.

## Storage And Data Directory

- `P2P_NATIVE_AGENT_DATA_DIR` configures the Agent data directory.
- Default data dir is `/var/dirextalk-message-server/agent`.
- Docker compose must mount a durable Agent data volume for skills, MCP metadata, and runtime CLI tools.
- Homeserver/sync DB access is read-only. Native Agent must not write Matrix tables directly.

## Acceptance

- Automated checks pass:
  - `go test ./p2p ./internal/productpolicy -count=1`
  - `go test ./internal/httputil ./setup -count=1`
  - `go test ./syncapi/storage ./syncapi/routing -count=1` when sync reader code is touched
  - `go build ./cmd/dirextalk-message-server`
  - `docker compose -f docker-compose.p2p.yml config`
  - `git diff --check`
- Real interface testing passes with the model key provisioned only as a read-only Agent-mounted secret file:
  - Native Agent is absent from plugin catalog/list/lifecycle/invoke surfaces.
  - `agent.runtime.profile.get` returns only the exact public catalog/state fields, and an owner HTTP-only update selects a catalog profile without transmitting credential material.
  - Direct `agent.chat` returns a Chinese reply.
  - Realtime `client.native_agent_stream` emits `delta`, `trace`, and `done`.
  - Skill install/list works and enabled skill text affects the system prompt.
  - MCP install/list works and a discovered MCP tool can be invoked by Agent.
  - Runtime CLI tool install/which/run works.
  - Built-in tools for contacts, rooms, messages, summaries, and sending messages work.
  - The key does not appear in ProductCore or Agent gRPC payloads, logs, docs, git diff, persisted config, environment variables, command arguments, or test output.

## Test Secret Handling

The operator-provisioned model key is a live secret. For independent Agent acceptance, expose it only through the deployment's read-only mounted secret file. Never place it in ProductCore parameters, Flutter state, Message Server or Agent configuration/catalog data, environment variables, command arguments, repository files, shell history snippets, docs, logs, or test output. Rotate it after acceptance testing.
