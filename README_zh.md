# Dirextalk Message Server

Dirextalk Message Server 是 Dirextalk 的后端合约权威。一个 Go 服务同时提供
Matrix 兼容 homeserver、ProductCore action、产品策略、projection、外部 MCP，
并作为客户端访问独立 Native Agent runtime 的认证代理。

本仓库基于 Element Dendrite，但维护目标是 Dirextalk 产品服务，而不是通用 Matrix homeserver 发行版。

![Dirextalk Message Server 架构概览](docs/images/dirextalk-message-server-overview.png)

[English README](README.md)

每个个人节点在 Matrix、ProductCore 和 Native Agent 之间保持统一合约边界：

- Matrix 是房间、成员关系、普通消息、媒体、历史、搜索、未读状态和
  redaction 的事实源。
- ProductCore action 是经过鉴权的产品 facade，负责参数校验、远端转发、
  Matrix 写入编排和 projection 读取。
- PostgreSQL-backed P2P 表默认是 projection/read model，只有当前合约明确
  声明时才作为权威记录。
- Flutter 只连接 Message Server。外部 `dirextalk-agent` 拥有 Native Agent
  执行和数据；Message Server 保持现有 owner-authenticated `agent.*` actions
  和 Native Agent WS frames。
- `POST /mcp` 与 Agent Product Capability callback 复用 Message Server 的
  产品/Matrix 服务，但不会把 Native Agent 放入 plugin 生命周期，也不会新增
  第二套客户端 API。

## 运行时

- 生产入口：`cmd/dirextalk-message-server`
- Docker 镜像：`dirextalk/message-server:latest`
- Docker 默认配置路径：`/etc/dirextalk-message-server/message-server.yaml`
- Docker 默认数据路径：`/var/dirextalk-message-server`
- Go module：`github.com/YingSuiAI/dirextalk-message-server`
- Go 版本：`1.26.6`
- 服务端数据库：仅支持 PostgreSQL；不支持 SQLite 或 file DSN。
- Docker 开发数据库：PostgreSQL 18

## API 入口

Matrix 协议路径保持在：

- `/_matrix/*`
- `/_synapse/*`
- `/_dendrite/*`
- `/.well-known/matrix/*`

Dirextalk 产品 API 使用 body-action 入口：

- `GET /_p2p/health`
- `POST /_p2p/query`
- `POST /_p2p/command`
- `GET /_p2p/events`
- `GET /.well-known/portal/owner.json`

生成后的 ProductCore action 元数据位于
[docs/product-action-contract.json](docs/product-action-contract.json)，它是
可检查的 action 列表，能力增加时会随源码变化。Owner 客户端使用 HTTP
query/command，并通过 SSE 订阅 Product 事件；外部 MCP 客户端在 `POST /mcp` 上使用带 `agent_token`
的 JSON-RPC。

鉴权和传输边界：

- Owner ProductCore action 使用 `Authorization: Bearer <access_token>`。
- `GET /_p2p/events` 接受 owner bearer token，并通过 `after_seq` 或
  `Last-Event-ID` 断点续传。
- `agent_token` 只能调用 `agent.matrix_session.create` 和标准 `POST /mcp`；
  不能调用 owner action。

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

## 本地开发

在仓库根目录运行命令。Windows PowerShell、Linux Bash、macOS Bash/Zsh、WSL Bash 都可以使用；按当前 shell 选择对应命令格式。

构建服务：

```bash
go build ./cmd/dirextalk-message-server
go build ./cmd/dendrite
```

启动单节点 Docker 栈：

```bash
docker compose -f docker-compose.p2p.yml up --build
docker compose -f docker-compose.p2p.yml exec message-server cat /var/dirextalk-message-server/p2p/bootstrap.json
```

运行三节点回归。

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

使用本机 PostgreSQL 运行测试：

PowerShell：

```powershell
$env:POSTGRES_USER = "postgres"
$env:POSTGRES_PASSWORD = "123789"
$env:POSTGRES_HOST = "localhost"
$env:POSTGRES_PORT = "5432"
$env:POSTGRES_DB = "postgres"
go test ./p2p ./internal/productpolicy -count=1
```

Bash：

```bash
export POSTGRES_USER=postgres
export POSTGRES_PASSWORD=123789
export POSTGRES_HOST=localhost
export POSTGRES_PORT=5432
export POSTGRES_DB=postgres
go test ./p2p ./internal/productpolicy -count=1
```

测试 helper 会创建相互隔离的 `dendrite_test_*` 数据库，并在对应测试结束后删除这些测试库。

## 合约事实源

修改客户端、部署工具或 Agent/MCP 行为前，优先读取这些当前维护的事实源：

- [生成后的 ProductCore action 合约](docs/product-action-contract.json)
- [当前项目文档](docs/current-project-documentation.md)
- [当前 Agent 和 MCP 合约](docs/agent-mcp-current-contract.md)
- [外部 Agent Core 集成合约](docs/agent-core-integration-development-contract.md)
- [Execution V2 ADR](docs/adr/2026-07-31-execution-orchestration-v2.md)

## 文档

当前维护文档保持精简。继承自 Dendrite 的站点文档、过时 tracker 和一次性实施计划不再作为本 fork 的维护文档。

- [当前项目文档](docs/current-project-documentation.md)
- [当前 Agent 和 MCP 合约](docs/agent-mcp-current-contract.md)
- [Release notes](release/RELEASE_NOTES.md)
- [Docker 镜像说明](docs/dirextalk-message-server.md)
- [Push Gateway 合约](docs/dirextalk-push-gateway.md)
- [拆分 Agent 部署说明](deploy/split-agent/README.md)

## License

本项目保留来自 Element Dendrite 的上游 license 与版权声明。见 [LICENSE](LICENSE) 和 [LICENSE-COMMERCIAL](LICENSE-COMMERCIAL)。
