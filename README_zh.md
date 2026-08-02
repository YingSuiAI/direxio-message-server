# Dirextalk Message Server

Dirextalk Message Server 是 Dirextalk 后端服务，将 Matrix 兼容 homeserver 与 Dirextalk P2P 产品 API 合并在一个 Go monolith 中。

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
- Native Agent 和 `POST /mcp` 是后端拥有的能力，不通过 plugin 生命周期安装、
  配置或调用。BYOK web search 只在请求级
  `tool_credentials.web_search` 中接收 Tavily key，绝不持久化。

## 运行时

- 生产入口：`cmd/dirextalk-message-server`
- 兼容入口：`cmd/dendrite`
- Docker 镜像：`dirextalk/message-server:latest`
- Docker 默认配置路径：`/etc/dirextalk-message-server/message-server.yaml`
- Docker 默认数据路径：`/var/dirextalk-message-server`
- Go module：`github.com/YingSuiAI/dirextalk-message-server`
- Go 版本：`1.26.4`

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
- `GET /_p2p/ws`
- `GET /.well-known/portal/owner.json`

生成后的 ProductCore action 元数据位于
[docs/product-action-contract.json](docs/product-action-contract.json)，它是
可检查的 action 列表，能力增加时会随源码变化。Owner 客户端优先在 realtime
WebSocket 上使用 `client.request`/`server.response`，realtime 未 ready 时回退
到 HTTP query/command；外部 MCP 客户端在 `POST /mcp` 上使用带 `agent_token`
的 JSON-RPC。

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

## 文档

当前维护文档保持精简。继承自 Dendrite 的站点文档、过时 tracker 和一次性实施计划不再作为本 fork 的维护文档。

- [当前项目文档](docs/current-project-documentation.md)
- [实现说明](docs/p2p-integrated-as-implementation.md)
- [API 变更记录](docs/api-interface-change-record.md)
- [API 审计与优化记录](docs/api-audit-and-optimization.md)
- [Postman collection](docs/postman/dirextalk-message-server.postman_collection.json)
- [插件 Postman collection](docs/postman/dirextalk-plugins.postman_collection.json)
- [Docker 镜像说明](docs/dirextalk-message-server.md)
- [Push Gateway 合约](docs/dirextalk-push-gateway.md)

## License

本项目保留来自 Element Dendrite 的上游 license 与版权声明。见 [LICENSE](LICENSE) 和 [LICENSE-COMMERCIAL](LICENSE-COMMERCIAL)。
