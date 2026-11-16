# 研发协作 Agent

这是一个使用 Go 构建的研发协作 Agent。项目会逐步支持项目资料检索、研发工具调用、风险审批、任务状态保存和结果生成。

## 当前阶段

第一周：工程准备与 LLM 最小闭环。

本周目标：

- 建立可维护的 Go 项目骨架
- 封装 OpenAI 兼容模型接口
- 提供非流式和流式聊天 API
- 建立超时、取消、错误响应和单元测试基础

## 项目结构

```text
.
├── cmd/
│   └── server/          # 服务启动入口
├── internal/
│   ├── agent/           # Agent 相关逻辑
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
```

## 配置说明

| 配置项 | 说明 |
| --- | --- |
| `HTTP_ADDR` | HTTP 服务监听地址 |
| `LLM_BASE_URL` | OpenAI 兼容接口地址 |
| `LLM_API_KEY` | 模型服务 API Key |
| `LLM_MODEL` | 使用的模型名称 |
| `LLM_TEMPERATURE` | 模型采样温度 |
| `LLM_TIMEOUT` | 单次模型调用超时时间 |

## 开发约定

- 业务代码放在 `internal/`，启动入口放在 `cmd/`
- 配置通过环境变量注入，不提交真实密钥
- 模型调用通过独立客户端封装，HTTP 层不直接依赖供应商 SDK
- 错误需要携带上下文，并在 HTTP 层转换为统一响应
- 每个阶段完成后补充测试和文档
