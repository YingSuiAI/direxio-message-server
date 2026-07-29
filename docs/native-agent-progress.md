# Native Agent Progress

> Superseded/current contract note: older checklist items below may describe
> the temporary Agent-as-plugin implementation path used during early Native
> Agent work. The current backend contract is first-class owner `agent.*`
> actions plus realtime `client.native_agent_stream` /
> `client.native_agent_stream.cancel`. `io.dirextalk.agent` is not exposed
> through plugin catalog/list/lifecycle/config/invoke/health/logs. Ops and
> future non-Agent plugins continue to use the plugin manager. Native Agent
> runtime config now lives in native portal Agent config storage; startup
> performs an idempotent sanitized import from any old hidden
> `io.dirextalk.agent` plugin config without exposing Agent through plugin
> management.

Only mark an item after implementation and verification are complete. Do not pre-check planned or partially finished work.

## Requirements And Tests

- [x] Requirements document created in `docs/native-agent-requirements.md`.
- [x] Native Agent progress document created in `docs/native-agent-progress.md`.
- [x] Native Agent contract tests added before production implementation.
- [x] Final diff reviewed before commit.

## Runtime Integration

- [x] Native Agent routes to native runtime and never to Docker runner.
- [x] Ops and future non-Agent plugins still route to Docker runner when enabled.
- [x] Historical temporary Agent-as-plugin lifecycle path is superseded by first-class `agent.*` actions and protected by negative plugin-surface tests.
- [x] First-class `agent.*` actions support native Agent calls.
- [x] `client.native_agent_stream` WebSocket path supports native Agent streaming and cancellation.
- [x] `P2P_NATIVE_AGENT_DATA_DIR` is supported with default `/var/dirextalk-message-server/agent`.
- [x] Docker compose includes a durable native Agent data volume/path.

## Agent Orchestration And Memory

- [x] Model orchestration loop supports model request, tool call, tool result feedback, and final answer.
- [x] Tool results can be summarized by a subsequent model call in the same loop.
- [x] Conversation memory stores recent user/assistant turns.
- [x] Context compression summarizes older turns and keeps a bounded recent window.
- [x] Streaming chat stores completed output in memory.

## Model And Prompt

- [x] Provider factory supports `openai`.
- [x] Provider factory supports `anthropic`.
- [x] Provider factory supports `deepseek`.
- [x] Provider factory supports `openai_compatible`.
- [x] Request-level `model_profile` supports provider/model/base_url/api_key plus optional temperature/top_p/max_output_tokens/context_window when the client explicitly configures them.
- [x] `agent.models.list` proxies real provider model lists without persisting API keys, server-side profile lists, or invented tuning defaults.
- [x] System prompt supports config prompt, request override, and enabled skills.

- [x] API keys are server-owned encrypted profile credentials and are not persisted/logged/returned outside the profile boundary.
## Tools

- [x] Built-in contacts list/search tool works.
- [x] Built-in rooms search tool works.
- [x] Built-in messages list tool works through read-only homeserver/sync DB context.
- [x] Built-in messages send tool writes through `p2p.Transport`.
- [x] Built-in room members tool works.
- [x] Built-in channel content tools work.
- [x] Built-in summarize tool works.

## Skills

- [x] Built-in Message Server instructions and first-party tools remain available.
- [x] Third-party Skill install/manage/execute paths are unavailable.
- [x] The embedded backend never advertises the `skill` capability.

## MCP

- [x] Durable `agent.core.mcp.*` install/list/execute uses pinned HTTPS
  Streamable HTTP only.
- [x] stdio, local MCP, HTTP/SSE and subprocess candidates are rejected before
  side effects.
- [x] Dirextalk's authenticated `POST /mcp` contract remains independent.
- [x] Native Agent does not dynamically load third-party MCP tools.

## Runtime CLI

- [x] Runtime CLI and shell execution are unavailable in the single-process
  deployment.
- [x] No Agent child process, runner or sidecar is started.

## Verification

- [x] `go test ./p2p ./internal/productpolicy -count=1`.
- [x] `go test ./internal/httputil ./setup -count=1`.
- [x] `go test ./syncapi/storage ./syncapi/routing -count=1` if sync reader code is touched.
- [x] `go build ./cmd/dirextalk-message-server`.
- [x] `docker compose -f docker-compose.p2p.yml config`.
- [x] `git diff --check`.

## Real Interface Acceptance

- [x] Local service starts for native Agent testing.
- [x] Owner token can call first-class Native Agent actions.
- [x] DeepSeek direct `agent.chat` returns a Chinese reply.
- [x] DeepSeek Native Agent realtime stream emits `delta`, `trace`, and `done`.
- [x] Test skill install/list works and changes system prompt behavior.
- [x] Test MCP server install/list works and tool is callable.
- [x] Runtime CLI install/which/run works.
- [x] Built-in contacts, rooms, message list, summarize, and send tools are exercised.
- [x] Temporary DeepSeek key is absent from logs, docs, git diff, persisted config, and test output.

## Eino Correction

- [x] Eino examples were downloaded locally and reviewed before correcting implementation.
- [x] Eino latest stable core (`v0.9.12`), Eino OpenAI model, Eino DeepSeek model, Eino official MCP, and `modelcontextprotocol/go-sdk` dependencies are added.
- [x] Native Agent model/tool loop uses Eino ReAct instead of the previous hand-written loop.
- [x] OpenAI, DeepSeek, and OpenAI-compatible providers use Eino model components.
- [x] Anthropic uses a direct-only Eino `ToolCallingChatModel` adapter and does not introduce AWS/Google SDK dependencies.
- [x] Built-in Dirextalk tools are wrapped as Eino `InvokableTool` implementations.
- [x] Third-party MCP tools use Eino official MCP and the official Go MCP SDK transports.
- [x] Conversation memory stores Eino `schema.Message` history captured from ReAct `WithMessageFuture`.
- [x] Context compaction runs through Eino message rewriting and optional Eino model summarization.
- [x] Installed runtime CLI tools are exposed as Eino tools and covered by an Agent loop test.
- [x] Eino-specific native Agent unit tests pass: `go test ./p2p/nativeagent -count=1`.
- [x] Full backend verification has been rerun after the Eino correction.
- [x] Real interface acceptance has been rerun after the Eino correction.
- [x] Final diff and secret scan have been rerun after the Eino correction.

## Extended Agent-Reach And Context7 Smoke

- [x] Agent-Reach skill was installed through the Native Agent skill install action from the GitHub `SKILL.md`.
- [x] Agent-Reach runtime CLI was installed/registered through the Native Agent runtime install action.
- [x] Bilibili runtime search tool was installed through `agent.runtime.install` and called through `agent.runtime.run`.
- [x] DeepSeek/Eino `agent.chat` called `runtime__agent_reach` and `runtime__bili`, then summarized Bilibili Shanghai food-guide results.
- [x] Context7 was installed through the Native Agent MCP server install action.
- [x] DeepSeek/Eino `agent.chat` called Context7 MCP tools and answered from retrieved Gin documentation.
- [x] XiaoHongShu and Twitter/X runtime tools were invoked and returned real missing-prerequisite output for login/cookies/native CLI dependency setup.
- [ ] Authenticated XiaoHongShu search succeeds after `xhs` cookies or `xiaohongshu-mcp` QR login backend is configured.
- [ ] Authenticated Twitter/X search succeeds after `twitter-cli` cookies/login are configured.

## Dialogue Management Tools

- [x] Native skill management tools are exposed to Eino conversations through the same `enabledTools` pipeline as Dirextalk tools.
- [x] Native skill management tools stay available when older config/request `enabled_tools` values list only the original Dirextalk tools.
- [x] Agent model loop can install a skill during `agent.chat`, persist it, and make it available to the next system prompt.
- [x] Native MCP server management tools are exposed to Eino conversations through the same `enabledTools` pipeline as Dirextalk tools.
- [x] Native MCP server management tools stay available when older config/request `enabled_tools` values list only the original Dirextalk tools.
- [x] Agent model loop can install an HTTP MCP server during `agent.chat`, discover tools, and persist it for the next turn.

## Extended DeepSeek Multi-Scenario Smoke

- [x] Multiple enabled skills were installed and listed through Native Agent skill actions: content skills plus GitHub Agent-Reach.
- [x] Runtime tools were installed through `agent.runtime.install` while using temporary China mirrors only in the smoke run.
- [x] DeepSeek `agent.chat` used enabled skill prompt behavior and returned observable `trace`/`steps`.
- [x] Local HTTP MCP server was installed through `agent.mcp.servers.install`, discovered, called by DeepSeek, and summarized in the final answer.
- [x] Context7 MCP was installed and called by DeepSeek in the same native Eino tool framework.
- [x] Conversation memory was retained, compressed through `agent.context.compress`, and reused by a later DeepSeek chat.
- [x] WS `client.native_agent_stream` emitted streamed `delta`, `trace`, and `done` events, and the trace showed the runtime tool call.
- [x] Temporary mirror/proxy settings were restored after the smoke run and were not written to repository code.
