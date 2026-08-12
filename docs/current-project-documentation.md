# Dirextalk Message Server 当前项目文档

本文只描述当前代码、架构与接口。当前实现、生成 contract 和 focused tests
优先于叙述性说明。

## 2026-08-07 Release V2 组件版本格式

`release.v2.apply.component=server` 的 `target_version` 接受 canonical 稳定版
`vX.Y.Z` 和测试版 `devX.Y.Z`；`component=agent` 只接受稳定版 `vX.Y.Z`。
中台 server `version`、updater status/current version 以及 server active-job
版本使用相同 server 格式，Agent receipt/active-job 版本只使用稳定格式。
服务端升级通道严格隔离：`v` 运行版本只能
升级到更高的 `v` 版本，`dev` 运行版本只能升级到更高的 `dev` 版本，禁止
跨通道升级。客户端版本与中台 server `preVersion` 仍只接受稳定版
`vX.Y.Z`；中台 agents `preVersion` 是 Agent 目标要求的最低 Message Server
版本。

## 1. 项目定位

本仓库是 Dirextalk 对 Element Dendrite 的集成式 fork：同一个 Go monolith 同时提供 Matrix homeserver 能力和 Dirextalk P2P 产品 API。

当前架构原则：

- Matrix 事件与房间状态是好友、群、频道、成员、普通消息的事实源。
- P2P action 是产品语义 facade，负责鉴权、参数校验、远端转发、Matrix 写入编排和投影读取。
- P2P 数据表保留为 projection/read model，不作为成员关系和普通消息的最终事实源。
- 产品代码不得直接写 Matrix SQL 底表；房间、成员、消息、redaction 等 Matrix 行为必须通过 `p2p.Transport` 或 Matrix Client-Server API 进入 Dirextalk Message Server。
- Flutter 只连接 Message Server。Native Agent runtime 和数据归独立部署的
  `dirextalk-agent`；本服务保留稳定的 ProductCore action、WS stream 和
  Product Capability 边界。

## 2. 对外入口

Matrix 协议入口保持 Dirextalk Message Server 原有路径：

- `/_matrix/*`
- `/_synapse/*`
- `/_dendrite/*`
- `/.well-known/matrix/*`

Dirextalk 产品 API 以 body-action surface 为主；标准 MCP 客户端使用单独的 Streamable HTTP endpoint：

- `GET /_p2p/health`
- `POST /_p2p/query`
- `POST /_p2p/command`
- `POST /mcp`
- `GET /_p2p/ws`
- `GET /.well-known/portal/owner.json`

请求 envelope：

```json
{
  "action": "channels.public.get",
  "params": {
    "room_id": "!room:dendrite-a:8448",
    "remote_node_base_url": "https://dendrite-a:8448/_p2p"
  }
}
```

Protected action 通过 HTTP route 调用时需要 `Authorization: Bearer <access_token>`。登录后的客户端 product action 在 WS 已收到 `server.ready` 时优先走 `GET /_p2p/ws` 上的 `client.request`/`server.response`；点击时 WS 未 ready 或已断线时，当前 action 立即用 `POST /_p2p/query` 或 `POST /_p2p/command` 作为 owner HTTP fallback，同时 realtime WS 在后台继续重连。已发出 WS request 后响应丢失时，只对可安全重复的 action 做 HTTP fallback。`client.version.report` 支持 owner HTTP/WS；`release.v2.status`、`release.v2.apply` 与 `portal.account.delete` 是 owner `access_token` 保护的 HTTP-only 命令。`release.v2.status` 不接受参数，并行读取 receipt-bound updater 状态与固定中台 `appId=1&channelId=agents` 记录；中台故障不会阻止返回 updater/server/job/watchdog 事实，Agent 目标不可用时以结构化 reason 表示。状态不执行 GitHub discovery、不暴露执行计划或基础设施字段。`release.v2.apply` 只接受 `component=server|agent`、canonical `target_version`、小写 UUID `idempotency_key` 与 `confirm=apply_release_change`。server 更新每次重取固定 server 记录，并用其 `preVersion` 复核当前 device 报告的 client；Agent 更新每次重取固定 agents 记录，用其 `preVersion` 复核实际运行的 Message Server，并要求目标严格新于 updater receipt 绑定的 Agent 当前版本。通过 Gate 后仅向 updater 发送 `component`、`target_version`、`minimum_server_version`、`idempotency_key`、`confirm` 五个字段；server 的 `minimum_server_version` 必须为空字符串，Agent 使用 agents `preVersion`。它拒绝 image/digest/url/shell/Compose/service 和未知基础设施字段。`realtime.ws_ticket.create` 只接受 owner `access_token` 创建 owner WS ticket。`agent_token` 只允许通过 product body-action 访问 `agent.matrix_session.create`，并可访问标准 `POST /mcp` MCP endpoint，不能通过 HTTP fallback 调用 owner product action。固定 `mcp.*` HTTP body action 已从 `/_p2p/query` 和 `/_p2p/command` 删除；外部 MCP客户端必须使用 `POST /mcp` JSON-RPC。标准 MCP endpoint 与 `agent.matrix_session.create` 不迁移到 WS `client.request`。`GET /_p2p/ws` 只接受短期单次 owner WS ticket，不直接接受 bearer token。当前 public action 是：

- `portal.bootstrap`
- `portal.auth`
- `portal.status`
- `contacts.reactivate`
- `rooms.reactivate`
- `reports.submit`
- `channels.public.search`
- `channels.public.get`
- `channels.public.posts.list`
- `channels.public.join_request`
- `channels.public.join_result`
- `users.public_channels`

Action auth and transport metadata is generated from `p2p/serviceapi.ActionSpecs` into `docs/product-action-contract.json`; contract-critical docs and clients should treat that generated file as the checkable action list.

`portal.bootstrap`、`portal.auth`、`portal.password` 响应只暴露一个初始化状态：`initialized`。它只表示用户是否已通过 `portal.password` 修改过初始密码；profile 是否填写不影响该状态。`client.version.report` 绑定发起 HTTP 请求或创建 WS ticket 时认证的 portal device/session；设备或会话切换后的旧请求、旧 WS 会以 `client_session_stale` 拒绝。报告通过只更新 client build 字段的 device-CAS 写入，其他 portal 字段不会被旧快照覆盖；新 portal device 会原子清掉旧设备报告。同 device 的 `portal.password` token/generation 轮换和 portal 持久化与 report 复核/CAS 共用 session mutex，完成后释放锁再刷新 Matrix session，旧 report 不会越过轮换落库。`release.v2.status` 的 Agent 当前版本来自 host updater 的 receipt-bound runtime，中台 agents `version` 仅是比较目标：只有它严格高于当前版本时才返回 `update_available=true`；中台版本低于或等于当前版本时，`latest_version` 返回当前版本，避免把落后的中台记录标成最新版本。`minimum_server_version` 来自本次固定中台 agents 记录；兼容性只在存在更高 Agent 目标时比较实际 Message Server 版本与 agents `preVersion`。中台或 updater 任一侧失败都保留另一侧可验证事实，不使用缓存或旧 action fallback。`portal.account.delete` 要求 `params.confirm="delete_account"`，先持久化 updater desired state `deprovisioned`，失败时不执行后续破坏操作；成功后向 accepted direct contacts 发布带 `account_deleted` 的 `io.dirextalk.room.profile` 解散状态，让对端隐藏已注销联系人，随后退出直聊、解散 owner 创建的群聊和频道、退出 owner 只是成员的群聊/频道、停用本地 owner/agent Matrix 账号并写入非密钥 deprovision 标记。设置 `deprovisioned` 后任一阶段失败都会 best-effort 恢复 `running`；恢复失败返回安全结构化错误 `account_delete_watchdog_restore_failed`。该动作只清理本机数据库并关闭 message-server 进程，不销毁 AWS/云服务器实例。

`rooms.reactivate` 与 `channels.public.join_result` 是 HTTP-only 节点间回调，不是 WS `client.request` 或客户端常规入口。`rooms.reactivate` 只用于在群/私有频道成员节点重建后恢复对方节点上的邀请/待加入提示，不能让对方静默加入；最终加入仍由对方客户端调用 `groups.join` 或 `channels.join`。

`plugins.*` 只服务非 Agent 插件，并使用 owner `access_token`；Agent 不进入
plugin catalog、生命周期、配置或 invoke 路径。插件未启用时不发布对应能力。

Native Agent 当前边界：

- `dirextalk-agent` 拥有模型与加密凭据、对话/turn、Knowledge/长期记忆、
  Tasks、调度、Skills/MCP、AWS、Execution V2 和 runner 数据；Message Server
  不挂载或解释这些表和数据卷。
- Flutter 继续使用现有 `agent.*` actions 和 `client.native_agent_stream`。
  Chat 只转发完整的 `model_profile_id`、`model_profile_revision`、
  `credential_version` 三元组；inline profile、历史消息、工具凭据及嵌套
  credential-like 字段在到达 Agent 前拒绝。
- `agent.models.list` 返回 provider catalog；model-profile actions 返回持久化
  profiles。Tavily 只从 Agent 加密配置读取，Knowledge upload 要求整文件
  SHA-256，搜索分页返回并固定 embedding provenance；长期记忆与对话摘要分离。
- 自动用户事实记忆通过 `agent.memory.config.get/update/status` 和
  `agent.memory.facts.update/delete` 代理到 Agent
  `agent.memory.v1`。新实例默认关闭；只有 Agent 验证当前向量模型和密钥有效后，
  owner 才能显式开启。关闭会停止新的事实提取及结构化事实/时间线召回，但保留
  已有事实、冲突时间线、Knowledge 资料和对话历史。事实编辑会替换指定的活动事实
  并保留时间线，删除会撤回指定事实；Message Server 只做闭集请求/结果校验与投影，
  不持久化事实。
- Cloud Worker `agent.execution.v2.runs.events` 固定返回
  `events`、`next_sequence`、`history_truncated`。只有 `worker_progress` 事件可携带
  完整且有界的结构化 `progress`；普通生命周期事件禁止混入 progress。Message
  Server 严格拒绝模型原文、stderr、路径、环境变量、secret 与对象存储地址等私有
  字段；CPU 或内存值为 `0` 只表示 Worker 没有可信指标源，不代表零资源消耗。
  Agent 每个 run 最多保留 4096 个事件，游标早于保留窗口时显式返回
  `history_truncated=true`，不伪造完整历史；非截断页从请求游标下一条开始，页内
  后续事件必须连续。
- Product Capability 是 Agent 回调产品数据的独立 mTLS 方向，handler 不得同步
  回调 Agent。两向调用都绑定 owner、account generation、scope、operation 和
  call-chain，检测到循环即失败。
- `deploy/split-agent/compose.yaml` 使用一个 Agent 镜像启动 Core、extension
  runner、Core Runner 三个隔离容器；Message Server 与 Agent 可共用 PostgreSQL
  集群，但必须使用互不可读的角色和 database/schema。Agent 只支持 fresh-state，
  不做嵌入式数据导入、双写或回退。

完整字段、readiness 和限制以 [当前 Agent/MCP 合约](agent-mcp-current-contract.md)、
[Agent Core 集成合约](agent-core-integration-development-contract.md) 与生成的
[`product-action-contract.json`](product-action-contract.json) 为准。

官方 Ops 插件 `io.dirextalk.ops` 面向单机私有部署运维，动作包括 `ops.status.get`、`ops.containers.list`、`ops.logs.tail`、`ops.backups.list`、`ops.backup.create`、`ops.backup.status`、`ops.backup.download_chunk`、`ops.backup.delete`、`ops.cleanup.plan`、`ops.cleanup.run`、`ops.rooms.cleanup.plan`、`ops.rooms.cleanup.run`、`ops.media.orphans.plan`、`ops.migration.export`、`ops.restore.plan`、`ops.restore.run`。Ops 是唯一允许由 Docker runner 挂载 Docker socket 和专用备份 volume 的官方插件；启用时注入 `OPS_BACKUP_ROOT`、`OPS_MAX_BACKUPS`、`OPS_MESSAGE_SERVER_CONTAINER`、`OPS_POSTGRES_CONTAINER`、`OPS_POSTGRES_USER`、`OPS_POSTGRES_PASSWORD`。备份创建可异步返回任务并通过 `ops.backup.status` 轮询进度；备份下载通过 `ops.backup.download_chunk` 分片返回，客户端本地保存文件。`ops.restore.run` 必须显式传入 `confirm="restore_backup"`，用于从已有备份包恢复 Postgres dump。第一版清理必须先 plan 后 confirm：聊天记录清理只做本地缓存、隐藏/归档计划和受控安全操作，不允许 Ops 插件直接 SQL 删除 Matrix 事件表；媒体清理默认只清缓存或明确孤儿文件，仍被消息/频道引用的媒体不删除。

## 3. 运行时结构

核心入口：

- `cmd/dirextalk-message-server`：生产服务入口，monolith 模式运行。
- `setup/monolith.go`：装配 client、federation、media、sync、relay、P2P routes。
- `p2p/action_registry.go`：P2P action 到业务 handler 的注册表。
- `p2p/internal/{conversation,social,calls,blocks,contacts,members,groups,channels,reports,plugins,agent,mcp,portal,profile,release}`：按业务域拥有 ProductCore handler、DTO 与完整工作流。
- `p2p/internal/events`：拥有持久化产品事件流的序列分配、保留策略、游标校验和实时 waiter 通知。
- `p2p/internal/projector`：拥有 roomserver output 到 P2P read model 与产品事件的投影工作流。
- `p2p/internal/httpapi`：拥有 ProductCore HTTP、标准 MCP Streamable HTTP、CORS、JSON 解码和公开 health/well-known 协议处理；Gorilla 仍只在根路由边界负责精确路径与 method 挂载。
- `p2p/service_*.go`：保留公开 facade、跨域编排及 Matrix/运行时适配。
- `p2p/storage`：P2P projection/read model 持久化。
- `internal/dirextalktransport`：产品 Matrix 写入 transport contract。
- `internal/dirextalktransport/dendrite`：真实 Matrix roomserver 写入适配层；`p2p/dendrite_transport.go` 仅保留 facade 构造入口。
- `internal/dirextalkmatrix`：Matrix Client-Server HTTP profile/history reader；
  `p2p/matrix_profile_resolver.go` 与 `p2p/matrix_history_reader.go` 是 facade
  adapter。
- `internal/dirextalkprojection`：projection-only helper，例如成员 joined/pending 统计；P2P action 和 conversation view 只调用该 helper，不复制计算逻辑。
- `internal/dirextalkstate`：产品 Matrix state event content builder，例如 `io.dirextalk.room.profile`、`io.dirextalk.member.policy`、`io.dirextalk.join_request`；P2P action 仍负责决定何时通过 transport 发布。
- `internal/dirextalkdomain`：跨包共享的产品 value records 和纯 domain helper，例如 portal/agent config、conversation records、member/channel records、blocks、calls、favorites、reports、P2P event bounds 等；业务 response DTO 由各自的 `p2p/internal` 模块持有。
- `internal/dirextalkplugin`：非 Agent plugin catalog/instance/job/secret record shapes；`p2p/internal/plugins` 拥有 plugin action orchestration、Docker runner 和 Native Agent/plugin 隔离规则。
- `p2p/projector.go`、`p2p/projector_ports.go`：只保留投影公开 facade、账户生命周期门禁和 Matrix/业务模块适配；纯投影 helper 由 `internal/dirextalkprojection` 持有。
- `p2p/internal/agent` 与 `internal/agentgateway`：Message Server 的 Native Agent
  facade、action/stream adapter 和受认证 Capability gRPC client。任务、确认、
  调度、模型、知识、Skills/MCP、AWS 与 Execution V2 均由外部 Agent 拥有；
  Message Server 不在本机执行第三方 Skill、项目代码或 shell，也不暴露原始
  SSM/SSH/AWS passthrough。
- `p2p/consumer.go`：保留订阅 consumer 的公开 facade，实现在 `p2p/internal/projector`。
- `internal/productpolicy`：Matrix Client-Server 写入前的 Dirextalk 产品策略校验。

服务端所有持久化状态统一使用 PostgreSQL。SQLite/file DSN 不再支持，配置或启动阶段必须报错，不允许静默退化为内存态；P2P store 必须成功打开 PostgreSQL-backed store，不能因为迁移失败回退到 memory store。生产持久化优先使用全局 Dirextalk Message Server 数据库配置；未配置时 P2P store 会使用 roomserver 的 PostgreSQL 数据库配置。Docker 开发栈使用 PostgreSQL 18。

## 4. Matrix Native State

当前产品房间只使用 native Dirextalk state：

- `m.room.create.content.type`
  - `io.dirextalk.room.direct`
  - `io.dirextalk.room.group`
  - `io.dirextalk.room.channel`
- `io.dirextalk.room.profile`
  - direct/group/channel 的产品元数据。
- `io.dirextalk.member.policy`
  - role、mute 等成员策略。
- `io.dirextalk.join_request`
  - public channel 申请审批状态。
- group room 创建时写 `m.room.history_visibility=joined`，新成员只从自己的
  Matrix join 之后接收普通 timeline 消息。channel room 是统一的帖子+聊天频道，
  创建时写 `m.room.history_visibility=shared`；`channel_type` 不参与当前频道行为，
  `channels.update` 会忽略该字段。

投影规则：

- `io.dirextalk.room.profile` 投影到 groups/channels read model。
- direct invite 的 `io.dirextalk.room.profile` stripped state 投影为 inbound contact request，但联系人身份以 Matrix membership event 的真实 sender 为准；`requester_mxid`、`domain` 或 profile 展示字段不能把申请伪造成另一个用户。
- `io.dirextalk.member.policy` 投影成员角色与禁言。
- `io.dirextalk.join_request` 投影申请审批状态。
- Matrix `m.room.member membership=join` 是最终 joined 事实。
- 普通 Matrix timeline 不复制到 P2P 普通消息表；普通消息读写走 Matrix
  Client-Server API。Online Agent 使用真实 `agent_room_id` timeline；Native
  Agent 使用 owner-authenticated `agent.*` actions 和 Native Agent WS frames。
  两者不共享消息存储，也不把 prompt 复制到 ProductCore 消息表。

## 5. 用户请求生命周期

P2P action 生命周期：

1. 登录后客户端在 WS 已收到 `server.ready` 时通过 `GET /_p2p/ws` 发送 `client.request`；点击时 WS 未 ready 或断线时，同一 `{ "action": "...", "params": ... }` envelope 立即通过 HTTP `/query` 或 `/command` 作为 owner fallback，realtime WS 后台重连恢复事件流。portal/auth/password/account-delete、WS ticket、`POST /mcp`、`agent.matrix_session.create`、public/callback action 仍按各自 HTTP/WS 边界执行；固定 `mcp.*` body action 已删除。
2. route 或 WS request 处理器调用 `Service.Authorize`：
   - public action 直接放行；
   - protected action 校验 owner access token；`agent_token` 仅允许 product body-action `agent.matrix_session.create` 和标准 `POST /mcp`。
3. `Service.Handle` 分发到对应业务函数。
4. 业务函数校验参数、所有者/成员/策略权限。
5. 需要 Matrix 事实写入时调用 `p2p.Transport`。
6. Dirextalk Message Server roomserver 产生 output event。
7. `p2p.consumer` 调用 `ProjectRoomEvent` 更新 P2P read model。
8. `/_p2p/ws` 发送产品投影事件和通用 `server.response`。Owner WS 通过 `client.request` 执行登录后 product 查询/命令，但不包含 MCP action；旧 `client.command` 兼容别名已移除，客户端必须发送 `client.request`。同一连接上的 request 以 `id` 独立关联响应并允许完成顺序不同于发送顺序，最多同时执行 8 个；超限 request 在 dispatch 前收到 `429`，仍在执行的重复 `id` 只收到不带关联 `id` 的 `duplicate_request_id` protocol error，原 request 不会重放或产生第二个关联响应。连接关闭会取消仍在执行的 request。Agents room 消息、预览和回复走 Matrix Client-Server，不通过 P2P event 或 WS stream 转发。
9. 客户端普通消息、历史、搜索、redaction 继续通过 Matrix Client-Server API。

同步策略：

- `sync.bootstrap` 是冷启动、登录后恢复、本地缓存不可用或事件缺口兜底用的基线快照；不要在每个事件后全量刷新。
- `sync.bootstrap.read_markers` 固定返回按 `room_id` 排序的 metadata-only 数组，每项只有 `room_id`、`event_id`、`origin_server_ts`，空状态返回 `[]`。它只为新设备未读恢复提供 ProductCore fallback 边界，不返回消息正文、发送者或媒体；客户端仍通过 Matrix `/sync`、receipt 与 `/rooms/{roomID}/messages` 获取未读和历史。`sync.read_marker`、`channels.read_marker` 由服务端把 `event_id` 解析为对应 `room_id` 内的 Matrix timeline topology token，并只按该权威顺序推进；请求中的 `origin_server_ts` 可省略且不参与排序，bootstrap 返回事件本身的服务端时间戳。解析固定绑定已认证 owner MXID，并复用 Matrix history-visibility 与本地隐藏事件访问检查；事件不存在、属于其他房间或对该 owner 不可见时统一返回不泄露差异的校验错误。
- 日常弱网/断线恢复使用 `GET /_p2p/ws` 增量追平。客户端先通过 `realtime.ws_ticket.create` 创建 ticket，连接后发送 `client.hello` 的 `since=<last_seq>`，并持久保存最后处理的 `seq`，对已知事件类型做本地 reducer 更新；只有遇到未知事件、解析失败、缺口无法确认或本地缓存损坏时才拉一次 `sync.bootstrap`。WS ready 时可通过 `client.request` 拉取；WS 不可用时可通过 owner HTTP fallback 立即拉取。
- 如果 `since` 是非零旧 cursor 且已经早于服务端保留的 `p2p_events` 最小序号，WS 会先发送 `server.cursor_reset`。控制事件 payload 包含 `type`、`since`、`min_seq`、`max_seq`、`count`、`recovery: "bootstrap_required"`；客户端收到后应清理本地产品缓存，优先通过 WS `client.request` 调用一次 `sync.bootstrap`，WS 不 ready 时可用 owner HTTP fallback 拉取，再用最新 `seq` 继续订阅增量。

Matrix Client-Server 写入生命周期：

1. 客户端调用 Matrix send/state/member/redaction API。
2. Dirextalk product policy 读取当前 Matrix state。
3. 如果房间是 Dirextalk product room，则校验 dissolved、member join、mute、role、join policy 等规则。
4. 合法事件进入 Dirextalk Message Server 原生 roomserver。
5. roomserver output 再投影回 P2P read model。

## 6. 频道公开申请生命周期

客户端可见状态统一为：

- `pending`
- `rejected`
- `approved`
- `joining`
- `joined`
- `join_failed`

`channels.public.join_request`：

1. 申请人节点先在本地保存 `pending` projection。
2. 如果频道属于远端 room server，申请人节点把申请转发给频道主节点。
3. 频道主节点写 `io.dirextalk.join_request status=pending`。
4. 频道主节点 projection 中成员为 `pending`。

`channels.join_request.reject`：

1. 频道主节点写 `io.dirextalk.join_request status=rejected`。
2. 本地 projection 更新为 `reject`。
3. 如果申请人是远端用户，频道主节点调用申请人节点的 `channels.public.join_result`。
4. 申请人节点更新为 `rejected` 并发送 P2P event。

`channels.join_request.approve`：

1. 频道主节点写 `io.dirextalk.join_request status=approved`。
2. 如果申请人属于本节点，频道主节点调用 `Transport.JoinRoom`。
3. 如果申请人属于远端节点，频道主节点调用申请人节点的 `channels.public.join_result`。
4. 申请人节点以申请人身份调用 `Transport.JoinRoom`，并带上 `server_names`。
5. Matrix `membership=join` 成功后，projection 才进入 `join`。
6. join 失败时 projection 为 `join_failed`，不得返回或投影成 joined。

公开 open channel 与审批通过走同一套自动 join 流程。Matrix invite 可以作为底层协议事件存在于其他邀请场景，但公开频道申请审批不把 invite 暴露成产品流程。

频道主节点不得直接把远端用户写成 joined；远端用户 join 必须由该用户所在 homeserver 发起。

## 7. 业务结构

Portal/Profile：

- 默认启动时自动初始化 portal owner、owner token、agent token、默认密码和 owner profile。
- `P2P_PORTAL_PASSWORD_FILE` 可指向 mode 0400 的受保护密码文件；配置该文件时优先使用其内容，文件缺失或权限不安全不会回退到 plaintext 环境值。
- `P2P_PORTAL_PASSWORD` 可覆盖默认密码。
- `P2P_PORTAL_CREDENTIALS_FILE` 用于启动、密码变更和 session token 变更后的 credential JSON 写出。
- `portal.bootstrap`、`portal.auth`、`portal.password` 创建新的 portal owner Matrix session 后，会删除该 owner 的其他 Matrix devices，只保留本次登录 device；旧设备后续 Matrix 请求应收到 `M_UNKNOWN_TOKEN` 并回到手动登录。`agent.matrix_session.create` 是本地 bridge bootstrap action，可由 owner `access_token` 或 `agent_token` 调用，返回本地 `@agent:<server>` 的 Matrix session，不删除用户手机 device。
- profile update 同步 P2P profile/member projection，并写入 Matrix-facing profile storage。

Contacts：

- 发起联系人请求会创建 direct Matrix room，并邀请对方。
- inbound/outbound request 来自 Matrix invite/member projection。
- accept 通过 Matrix join 进入 direct room。
- `contacts.update` 设置的是 owner 本地联系人备注名；返回的 contact 可带 `display_name_override=true`。该标记存在时，后续远端 Matrix member display name 更新不能覆盖 contact `display_name`，但 avatar 仍可按远端 profile 更新。没有本地备注名的 accepted contact 继续跟随远端 Matrix member display name。
- delete 后保留原 direct room 身份用于恢复。删除方主动重新添加时，如果对方仍保留 accepted 关系，可以通过 `contacts.reactivate` 复用旧房间；如果请求方本地联系人数据被清理并创建了新的 direct invite，而目标方仍保留 accepted 旧关系，目标方优先重新邀请真实 sender 回旧房间，不采纳新房间里的伪造身份资料。如果清库重建导致旧 invite-only direct room 无法重新加入，则双方改用真实 sender 创建的新 direct room 作为 accepted 关系，旧房间历史不会复制到新房间。双方都已离开旧房间或对端不再保留旧关系时，再次申请会创建新的 direct request room；对历史遗留的旧 room pending request，accept 如果无法 rejoin 旧房间，也会创建新的 direct room 并接受关系。
- 群聊和私有频道成员节点清库重建后，群主/频道主再次邀请该成员时，如果 Matrix 侧显示成员仍在旧房间，owner 节点会先移除该 stale joined membership，再发送新的真实 Matrix invite，并调用成员节点 `rooms.reactivate` 恢复 pending invite/card；成员节点不能静默加入，必须由用户点击后调用 `groups.join` 或 `channels.join`，Matrix join 成功后才写 joined 投影。公开频道不使用 `rooms.reactivate`，重建成员应重新调用 `channels.public.join_request`，并继续遵守 open/approval policy；如果 owner 节点仍保留该公开频道成员的 stale joined membership，owner 节点会先移除并发送新的 Matrix invite，再通过 `channels.public.join_result` 让申请节点完成 join。
- reject/delete 只改变产品 projection 与对应 Matrix leave/kick 行为，不制造普通消息副本；如果 Matrix membership 已经是 leave，`contacts.delete` 仍按幂等删除处理并更新产品 projection。

Blocks：

- `blocks.add`、`blocks.list`、`blocks.remove` 是 owner protected action，用于管理当前用户拉黑的联系人，不提供群聊或频道拉黑。
- 联系人拉黑使用 `target_type=contact` 与 `peer_mxid`/`mxid`；`target_type=group/channel/room` 不是当前产品能力，应返回参数错误。
- 每条黑名单记录保存 `display_name` 与 `avatar_url` 展示快照；客户端没有传昵称时，服务端从现有好友资料回填，仍为空则回退到目标 id，避免黑名单只展示 id。
- `blocks.list` 只返回 `contacts` 列表，供用户设置页展示；客户端可在好友设置页调用 `blocks.add`，在黑名单列表中调用 `blocks.remove` 取消拉黑。
- 对已拉黑联系人发起好友申请或邀请已拉黑用户加入群聊/频道时，服务端在 Matrix 写入前返回 `403 already blocked`，客户端应提示“已经拉黑”。
- 被拉黑联系人对应的 inbound Matrix direct invite 不会投影成 pending 好友申请。
- 拉黑只过滤新的 direct 消息，不改变 Matrix 成员关系，也不删除既有历史；取消拉黑后恢复正常消息发送。

Groups：

- group create 写 Matrix room type 与 `io.dirextalk.room.profile`。
- `groups.update` 负责修改群名称、头像和 `topic`；字符串字段缺失或传空字符串时保留原值。更新前校验当前身份是该群的 `owner`，普通成员返回 `403`。
- invite/join/leave/remove/mute/unmute/dissolve 通过 `p2p.Transport` 与 native state 进入 Matrix。
- member list 来自 P2P projection，但最终事实是 Matrix membership。
- 群聊和频道只有 `owner` 与 `member` 两种产品角色。

Channels：

- channel create/update 写 Matrix room type 与 `io.dirextalk.room.profile`。
- public search/get 是只读发现，不创建占位记录。
- `channels.update` 负责修改频道名称、简介和头像；字符串字段缺失或传空字符串时保留原值。更新前校验当前身份是该频道的 `owner`，普通成员返回 `403`。
- invite grant 用于私有或分享卡片加入。
- public join request 使用上面的申请审批自动 join 生命周期。
- channel member、mute、read marker、dissolve 都保持 Matrix-first。
- 频道 `is_owned`、管理能力和发帖能力只来自 `owner` 角色。

Channel posts/comments/reactions：

- 仍是产品内容 projection。
- 使用 Matrix `m.room.message` 携带 `p2p_kind=channel_post` 或 `p2p_kind=channel_comment`。
- `channels.posts.create` 的 `visibility` 只接受 `public`/`private`，缺省为 `private`；该字段随 Matrix 帖子事件传播并投影到 PostgreSQL，缺少字段的历史帖子按私有处理。
- `channels.posts.update` 由帖子作者或频道主更新单个帖子的可变设置，接受 `post_id` 与可选的 `visibility`、`comments_enabled`，两项至少提供一项。完整设置先写入当前频道房间的 `io.dirextalk.channel.post.settings` Matrix state（`state_key=post_id`），再投影到各成员节点 PostgreSQL；state 写入失败不提前修改本地投影。关闭 `comments_enabled` 只拒绝该帖的新评论，不影响频道讨论区、频道聊天、该帖点赞/收藏或其他帖子；重新开启后恢复。评论发送端的 Matrix ProductPolicy 与接收端投影均校验该帖设置；独立 durable settings 记录保证 state 先于帖子历史到达时仍能合并，Matrix 原始帖子事件重放或历史回填不得覆盖。
- `channels.posts.list` 保持原有 owner 鉴权且不接受 `visibility`；不传 `page/page_size` 时仍一次返回频道全部帖子，传任一分页字段时按最新优先分页，另一字段缺省为 `page=1`/`page_size=5`，每页最大 100 条。每条帖子都返回帖子级 `comments_enabled`；频道 DTO 原有的同名字段仍只表示频道级设置。
- `channels.public.posts.list` 是无需 bearer 的独立公开帖子只读接口，接受 `channel_id` 或 `room_id`，始终只查帖子 `visibility=public`，默认每页 5 条并支持翻页与跨节点转发；帖子可见性独立于频道可见性，已知私有频道标识的访客也只能获得其中明确公开的帖子。每条结果提供 `comment_count`、`like_count`/兼容字段 `reaction_count`、`favorite_count`，但不计算访客的 `reacted_by_me`/`favorited_by_me`。非成员不能评论、点赞或收藏，写动作在 Matrix 写入和本地 projection 写入前要求 joined 成员投影，Matrix ProductPolicy 继续做最终权限校验。
- reaction 使用 Matrix reaction/内容字段投影到 P2P reaction read model；点赞开关事件携带 `active`，因此取消点赞也会覆盖到其他节点的 read model。
- 新成员加入 channel 后，服务端会从 Matrix `/messages` 历史回填当前频道已有 posts/comments/reactions 到本节点 projection，客户端可通过 product list 接口和 Matrix history 同时看到入群前内容。普通聊天消息仍走 Matrix timeline，不写入帖子/评论 projection。
- recall 通过 Matrix redaction。

Calls/Favorites/Follows：

- calls 是产品会话 read model，支持 create/incoming/get/list/active/event，持久化接通/结束时间、结束方和原因，并通过 `call.changed` P2P event 推送实时状态。
- saved-message favorites 和 follows 是 P2P owner-local product state，使用 P2P store 持久化。频道帖子收藏是 Matrix `m.reaction` 的 `reaction=favorite` 投影：按用户独立存储，`favorite_count` 是活跃用户数，`favorited_by_me` 按当前 owner 计算，并随着普通房间同步/回填收敛。好友举报和官方举报仍走 signed imadmin public API；群聊/频道所有者举报通过 ProductCore `reports.submit` 写入 owner 节点 `p2p_reports`，并向 `system_room_id` 发送 `msg_type=report` 的 Matrix 通知。

Push：

- 系统推送仍使用 Matrix Push Gateway API。客户端用 `/pushers/set` 注册 APNs/FCM pusher，普通 direct/group 消息、call invite 等通知由 `userapi/consumers/roomserver.go` 按 Matrix push rules 评估后发送到 gateway。所有 channel room 事件不投递 HTTP Push Gateway。
- 服务端不能从 `/sync`、read receipt 或 pusher 注册可靠判断 App 是否处于前台。Dirextalk 客户端通过 `GET /_p2p/ws` 上报 `client.lifecycle` 和 `client.focus`：`client.lifecycle` 至少包含 `foreground`，并可携带 `state`、`hidden` 和 `flags`；`client.focus` 至少包含 `room_id`，并可携带 `focused` 和 `flags`。前台、未 hidden、且 focused room 等于收到消息的 room 时，服务端不新增 unread notification，也不调用 HTTP push gateway；后台、hidden、断线、60 秒会话过期、未聚焦或聚焦到其他 room 时继续按后台推送处理。迁移期保留全局 Matrix account data `io.dirextalk.push.context` 作为无新鲜 WS session 时的兜底，服务端按服务端时间保存 60 秒过期时间。

Agent/API：

- Agent token 不再有动态权限表，只能通过 product body-action 访问 `agent.matrix_session.create`，并可访问标准 `POST /mcp` MCP endpoint，不能调用 `realtime.ws_ticket.create` 创建 WS ticket；其他 protected action 只认 owner `access_token`。本地 bridge 使用 `agent.matrix_session.create` 得到的 Matrix session 监听 agents room 并回写消息。
- MCP capability 是 owner-scoped 代理能力：`agent_token` 只负责授权标准 MCP endpoint，联系人、房间、成员、消息和频道内容工具按 portal owner 视角执行，并在 Matrix 读写前校验 joined membership。标准 `POST /mcp` 使用 MCP Streamable HTTP JSON-RPC，支持 `initialize`、`tools/list`、`tools/call`，只接受 `Authorization: Bearer <agent_token>`，拒绝 query-string token，校验 `Origin`，并且不会把入站 bearer token 传给下游 capability。Native Agent 内置 Dirextalk tools 与标准 `POST /mcp` 共用 `internal/dirextalkmcp` registry/service；固定 `mcp.*` body action 已删除。详见 [当前 Agent 和 MCP 合约](agent-mcp-current-contract.md)。
- Native Agent 对话是独立于 Online Agent Matrix room 的 `agent.*` 业务；普通调用走 owner-protected action，流式调用走 `client.native_agent_stream` / `server.native_agent_stream.*`。Message Server 把两者代理到外部 `dirextalk-agent`，只发布已通过 mTLS、account-generation、schema catalog 和 readiness 检查的能力。durable stream 的 request/result/event schema 均精确 pin；`waiting_confirmation` 在通用事件身份之外只携带 `confirmation_id`、`execution_id` 与固定 waiting status，不公开 `attempt_id`，也不与 text/tool/result/response/error 混合。Agent authored `turn_id` 与 App start `idempotency_key` 分离投影，`agent.chat.turn.stop` 通过 typed `stop_turn` revision mutation 执行而不复用通用 operation cancel，生成中的追加指令通过 typed `agent.chat.turn.steer` 立即引导同一 turn，禁止排队 successor turn。模型 profile、持久化对话、知识/长期记忆、Skills/MCP、调度和 Execution V2 均由 Agent 拥有，详见 [当前 Agent 和 MCP 合约](agent-mcp-current-contract.md) 与 [Agent Core 集成合约](agent-core-integration-development-contract.md)。
- `agent.matrix_session.create` 使用 owner `access_token` 或 `agent_token` 调用，用于本地 cc-connect/gateway 获取 `@agent:<server>` 的 Matrix Client-Server session；它不返回 owner Matrix session，也不回显 `agent_token` 或 portal password。
- Agent 在线状态对 owner 客户端只暴露一个 Matrix 房间状态字段：真实 `agent_room_id` 内的 `io.dirextalk.agent.status`，state key 为 `@agent:<server>`，content 只含 `online`。运行中的本地 bridge 通过 `@agent:<server>` Matrix session 发布 `online=true/false`；服务端不能从 Agent 配置、`/sync` 或 WS session 推断在线，只在启动/修复 agents room 或禁用 Agent 配置时写 `online=false` 兜底。`sync.bootstrap` 只返回 `agent_room_id` 供客户端定位房间，不再返回 `agent_online`；WS `server.event` 不发送 `agent.presence`。`agent.status`/`agents.status` 已删除，客户端不得再调用。
- Agent 预览和最终可恢复正文都通过 Matrix 消息/编辑回写；客户端展示 Matrix timeline 的聚合编辑结果，不消费 `server.agent_stream`。
- 服务初始化会创建真实私有 Matrix agents room，把 owner 和本地
  `@agent:<server>` 加入同一房间，并把 `agent_room_id` 写入 bootstrap
  credentials；`portal.bootstrap`、`portal.auth`、`sync.bootstrap` 都返回该
  room id。服务为该房间写入默认 room-level 空 actions push rule；已存在的
  显式同房间 push rule 会保留。
- 新增 MCP capability 时必须先更新 `internal/dirextalkmcp` registry/schema，再同步 Agent allowlist、接口变更记录和相关测试。

Multi-node：

- 房间、成员、消息、redaction、state 通过 Matrix federation。
- public channel discovery、user public channels 和 join request 使用 `remote_node_base_url` 显式指定目标 owner 节点 P2P base URL。
- 后端校验远端 URL；本地自签名双节点开发可用 `P2P_REMOTE_NODE_INSECURE_SKIP_TLS_VERIFY=true`。

## 8. 配置与开发命令

当前工具链：

- Go 1.26.5。
- 命令从仓库根目录执行。
- Windows 使用 PowerShell；Linux、macOS 或 WSL 使用 Bash/Zsh。文档命令应按当前环境给出，不应强制限定为 WSL。

单节点 Docker：

```bash
unset MESSAGE_SERVER_IMAGE
docker compose -f docker-compose.p2p.yml up -d --build message-server
docker compose -f docker-compose.p2p.yml exec message-server cat /var/dirextalk-message-server/p2p/bootstrap.json
```

部署已发布镜像时必须把 `MESSAGE_SERVER_IMAGE` 设为完整的不可变
`repo@sha256:<64-hex-digest>`，先显式 pull，再用 `--no-build` 启动；digest
路径不得使用 `--build`：

```bash
export MESSAGE_SERVER_IMAGE=dirextalk/message-server@sha256:<64-hex-digest>
docker compose -f docker-compose.p2p.yml pull message-server-init message-server
docker compose -f docker-compose.p2p.yml up -d --no-build message-server
```

三节点回归 compose 也读取同一个 `MESSAGE_SERVER_IMAGE`。固定 digest 时，
先 pull 三个 init 和三个 serving service，再用 `--no-build` 启动；本地源代码
验证才保留 `--build`：

```bash
export MESSAGE_SERVER_IMAGE=dirextalk/message-server@sha256:<64-hex-digest>
docker compose -f docker-compose.p2p-dual.yml pull \
  dendrite-a-init dendrite-b-init dendrite-c-init dendrite-a dendrite-b dendrite-c
docker compose -f docker-compose.p2p-dual.yml up -d --no-build dendrite-a dendrite-b dendrite-c
```

多节点 regression。

PowerShell：

```powershell
$env:P2P_DUAL_PUBLIC_HOST = if ($env:P2P_DUAL_PUBLIC_HOST) { $env:P2P_DUAL_PUBLIC_HOST } else { "host.docker.internal" }
docker compose -f docker-compose.p2p-dual.yml up -d --force-recreate dendrite-a dendrite-b dendrite-c
python scripts/p2p-three-node-regression.py
```

Bash：

```bash
export P2P_DUAL_PUBLIC_HOST="${P2P_DUAL_PUBLIC_HOST:-host.docker.internal}"
docker compose -f docker-compose.p2p-dual.yml up -d --force-recreate dendrite-a dendrite-b dendrite-c
python3 scripts/p2p-three-node-regression.py
```

本机 PostgreSQL 测试环境变量：

PowerShell：

```powershell
$env:POSTGRES_USER = "postgres"
$env:POSTGRES_PASSWORD = "123789"
$env:POSTGRES_HOST = "127.0.0.1"
$env:POSTGRES_PORT = "5432"
$env:POSTGRES_DB = "postgres"
```

Bash：

```bash
export POSTGRES_USER=postgres
export POSTGRES_PASSWORD=123789
export POSTGRES_HOST=127.0.0.1
export POSTGRES_PORT=5432
export POSTGRES_DB=postgres
```

Windows Docker Desktop users should prefer `127.0.0.1` over `localhost` for PostgreSQL ports published from containers. `localhost` may resolve to IPv6 `::1` first and wait for a failed connection before falling back to IPv4.

测试 helper 会创建相互隔离的 `dendrite_test_*` 数据库，并在对应测试结束后删除创建的测试库。

常用验证：

```bash
gofmt -w <touched go files>
go test ./p2p ./internal/productpolicy -count=1
go test ./internal/httputil ./setup -count=1
go build ./cmd/dirextalk-message-server
git diff --check
docker compose -f docker-compose.p2p-dual.yml config
```

## 9. 代码规范

- Go 代码必须 `gofmt`。
- 先从全局 Dirextalk server 视角梳理入口、鉴权、policy、storage、roomserver output、consumer/projection、sync/federation、docs 和验证路径，再把改动落在最小 owning package。
- 不新增 URL-shaped 产品接口；当前明确例外是标准 MCP Streamable HTTP endpoint `POST /mcp`。其它新增产品能力优先使用稳定 action 和 params schema。
- 不静默改变请求/响应字段；接口变化必须同步生成 contract、focused tests、
  owning current-contract 文档和受影响客户端。
- 必须持久化的产品状态不得放内存-only；扩展 `p2p.Store` 和 migration。
- 服务端数据库只支持 PostgreSQL；不要新增 SQLite storage、SQLite 测试或 `file:` 默认配置。
- Matrix 侧房间、成员、消息、redaction 不绕过 `p2p.Transport`。
- remote public lookup 不从 room ID 推导 P2P URL，必须使用请求提供的 `remote_node_base_url`。
- public channel membership 不得在 Matrix join 前标记为 joined。
- local delete 与 recall 保持语义独立：local delete 是本地隐藏；recall 是 Matrix redaction。
- 项目本地技能 `.codex/skills/*/SKILL.md` 与 AGENTS.md 必须随业务规则同步更新，并只承载 Dirextalk 项目专属事实、路径、检查矩阵和业务约束，不重复系统通用技能。
- 项目只保留两个高风险专项 skill：`dirextalk-backend-contract-state-storage`（合同、Matrix 事件状态、持久化和 migration 规则）与 `dirextalk-message-server-release`（稳定发布）。普通改动、影响面选择和验证命令由 `AGENTS.md`、代码、测试及父工作区 `COMMANDS.md` 负责。

## 10. 文档规则

- README/AGENTS 级文档只描述当前运行与开发规则，不维护继承自 Dendrite 的站点式安装、管理、FAQ 或历史计划文档。
- 本文件是当前项目事实源。
- `docs/agent-mcp-current-contract.md` 记录当前 Agent/MCP 合约。
- `docs/agent-core-integration-development-contract.md` 与 `docs/adr/2026-07-31-execution-orchestration-v2.md` 记录 Execution V2 的详细规范和发布门禁。
- `docs/dirextalk-message-server.md` 记录 Docker 镜像和运行说明，`docs/dirextalk-push-gateway.md` 记录 Push Gateway 合约。
- 不在活文档、技能规则或示例中保留旧接口作为当前可用能力。
