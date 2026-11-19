# 研发协作 Agent

这是一个使用 Go 构建的研发协作 Agent。项目会逐步支持项目资料检索、研发工具调用、风险审批、任务状态保存和结果生成。

## 当前阶段

第二周第 4 天：配置版本和发布。

本周目标：

- 建立可维护的 Go 项目骨架
- 封装 OpenAI 兼容模型接口
- 提供非流式 HTTP 聊天 API
- 提供 SSE 流式 HTTP 聊天 API
- 建立超时、取消、错误响应和单元测试基础
- 使用 Eino 验证 Prompt、Model、Lambda、Chain 和 Graph 的最小调用链
- 使用模型网关统一 DeepSeek、Kimi 等 OpenAI 兼容供应商
- 对模型输出执行 JSON Schema 和业务语义双重校验
- 通过最多一次的注入式修复处理有限的格式错误
- 对 Prompt、Schema、模型路由和价格配置进行版本化管理
- 为 Run 绑定不可变的配置快照

## 项目结构

```text
.
├── cmd/
│   └── server/          # 服务启动入口
├── internal/
│   ├── agent/           # Agent 相关逻辑
│   ├── configregistry/   # 配置版本、发布、回滚和 Run 快照
│   ├── structuredoutput/ # 模型结构化输出、Schema 和语义校验
│   ├── httpapi/         # HTTP Handler 和 API 类型
│   ├── observability/   # 日志、指标和 Trace
│   └── llm/             # 模型客户端封装
├── docs/                # 设计和面试笔记
├── evals/               # 离线评测数据
├── migrations/          # 数据库迁移
├── testdata/            # 测试数据
├── .env.example         # 本地配置示例
└── go.mod
```

## 环境要求

- Go 1.25+
- 一个 OpenAI 兼容接口及其 API Key

## 本地启动

复制配置示例并填写模型配置，程序启动时会自动读取项目根目录的 `.env`：

```bash
cp .env.example .env
go run ./cmd/server
```

配置优先级为：进程环境变量 > `.env` > 代码默认值。

`LLM_API_KEY` 和 `LLM_MODEL` 是必填配置，缺失时程序应在启动阶段直接报错。监听地址、日志级别、温度和超时时间有安全的默认值。

运行测试：

```bash
go test ./...
go vet ./...
go test -race ./...
```

API 契约保存在 [`docs/openapi.json`](docs/openapi.json)，包含 `/health`、`/chat`
和 `/chat/stream` 的请求、响应、统一错误结构及 SSE 事件示例。契约测试会在
`go test ./...` 中校验文档与当前 Handler 路由和响应结构的一致性。

Eino 最小调用链使用确定性模型替身，不访问外部模型服务，可直接运行：

```bash
go run ./cmd/eino-demo
```

示例同时执行 `Prompt -> Model -> Lambda` 的 Chain 和
`START -> Prompt -> Model -> Lambda -> END` 的 Graph。Eino 只存在于
`internal/agent` 编排边界，HTTP Handler 和 `internal/llm` 不依赖 Eino 类型。

模型网关位于 `internal/modelgateway`，负责供应商选择、任务路由、配置健康状态
和统一错误分类；`internal/llm` 继续负责 OpenAI 兼容 HTTP/SSE 协议。当前 HTTP
接口仍保持原有 `llm.Client` 链路，网关先作为独立的可测试能力存在，避免本日改动
已有 `/chat` 和 `/chat/stream` 契约。

结构化输出内核位于 `internal/structuredoutput`，提供
`DecodeTaskAnalysis`、`DecodeToolDecision` 和 `DecodeFinalAnswer` 三个类型安全
入口。它先严格解析单个 JSON 值，再执行 JSON Schema 校验和业务语义校验；失败时
最多调用一次注入的 `RepairFunc`，修复后仍需完整复核。调用方必须传入
`provider`、`model`、`prompt_version`、`schema_version` 和
`model_config_version`，解码报告会保留这些元数据。该包当前是独立内核，尚未接入
HTTP、模型网关或数据库持久化。

配置版本注册中心位于 `internal/configregistry`，当前使用内存实现保存不可变的
Prompt、Schema、模型路由和价格配置快照。`Publish` 与 `Activate` 分离，回滚会
创建新的发布版本而不是修改历史版本；`BindRun` 会把 Run 固定到创建时的 active
版本，避免新配置污染历史执行。当前不提供 HTTP 管理接口、不保存 API Key，也不
接入数据库，后续再扩展为持久化控制面。

项目提供三个 Go Fuzz 入口，用于验证请求 JSON、消息校验和 SSE 解析边界：

```bash
go test ./internal/httpapi -run=^$ -fuzz=FuzzDecodeChatRequest -fuzztime=10s
go test ./internal/httpapi -run=^$ -fuzz=FuzzValidateChatRequest -fuzztime=10s
go test ./internal/llm -run=^$ -fuzz=FuzzSSEEventParser -fuzztime=10s
```

GitHub Actions 会自动执行格式检查、单元测试、`go vet` 和 race 测试。

HTTP 服务会通过结构化日志记录请求 ID、接口状态、请求解析耗时、模型调用耗时、
流式首 Token 延迟和完整响应耗时。通用 HTTP 完成日志由响应包装器统一记录；
请求解析由解析边界记录；模型和流式指标由客户端 Decorator 记录。业务 Handler
不维护计时器或观测状态。日志只记录调用元数据，不记录 API Key、完整 Prompt 或
完整模型回答。
阶段性性能和真实调用证据保存在
`docs/evidence/2026-11-18/`。

## HTTP API

健康检查：

```bash
curl http://localhost:8080/health
```

聊天接口：

```bash
curl -X POST http://localhost:8080/chat \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"请只回复：HTTP调用成功"}]}'
```

`/chat` 当前支持 `system`、`user` 和 `assistant` 三种角色，最多 32 条消息。请求体最大为 1 MiB。模型错误、超时和参数错误都会返回统一的 JSON 错误结构。

流式聊天接口：

```bash
curl -N -X POST http://localhost:8080/chat/stream \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"请逐字回复：流式调用成功"}]}'
```

`/chat/stream` 使用 Server-Sent Events（SSE）返回事件：

- `message.delta`：模型生成的增量内容，包含 `id`、`model`、`sequence`、`index` 和 `delta`。
- `message.done`：流结束事件，包含模型返回的 `finish_reason`；只有上游提供 usage 时才包含 `usage`。
- `error`：响应 Header 已发送后发生模型或协议错误时返回的错误事件。

客户端断开或请求 Context 取消后，服务会停止读取上游并停止写入响应。流建立前的参数错误、模型超时和上游错误仍返回普通 JSON 错误。`finish_reason` 完全使用模型服务返回的值，HTTP 层不会自行伪造。
`sequence` 是服务端对外输出的连续事件编号；OpenAI 兼容上游通常不提供序号，
因此服务只会在上游明确返回扩展序号时校验上游顺序。请求体读取有 10 秒超时，
流式响应写入受模型超时加收尾缓冲的总时长保护。

## 配置说明

| 配置项 | 说明 |
| --- | --- |
| `HTTP_ADDR` | HTTP 服务监听地址 |
| `LLM_BASE_URL` | OpenAI 兼容接口地址 |
| `LLM_API_KEY` | 模型服务 API Key |
| `LLM_MODEL` | 使用的模型名称 |
| `LLM_TEMPERATURE` | 模型采样温度 |
| `LLM_MAX_TOKENS` | 单次调用最大输出 Token，`0` 表示不发送该字段 |
| `LLM_TIMEOUT` | 单次模型调用超时时间 |

启用多供应商模型网关时，可以使用以下配置：

```dotenv
LLM_PROVIDERS=deepseek,kimi
LLM_DEFAULT_PROVIDER=deepseek
LLM_ROUTE_CHAT=deepseek

LLM_DEEPSEEK_BASE_URL=https://api.deepseek.com/v1
LLM_DEEPSEEK_API_KEY=
LLM_DEEPSEEK_MODEL=
LLM_DEEPSEEK_TEMPERATURE=0.2
LLM_DEEPSEEK_MAX_TOKENS=0
LLM_DEEPSEEK_TIMEOUT=30s

LLM_KIMI_BASE_URL=https://api.moonshot.cn/v1
LLM_KIMI_API_KEY=
LLM_KIMI_MODEL=
LLM_KIMI_TEMPERATURE=0.2
LLM_KIMI_MAX_TOKENS=0
LLM_KIMI_TIMEOUT=30s
```

网关选择顺序为：请求显式 Provider、任务路由、默认 Provider。当前阶段不自动
重试、降级或故障转移；未知或配置不健康的供应商会返回明确错误。

## 开发约定

- 业务代码放在 `internal/`，启动入口放在 `cmd/`
- 配置通过环境变量注入，不提交真实密钥
- 模型调用通过独立客户端封装，HTTP 层不直接依赖供应商 SDK
- 错误需要携带上下文，并在 HTTP 层转换为统一响应
- 每个阶段完成后补充测试和文档
