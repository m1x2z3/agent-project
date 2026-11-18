# 第 1 周第 6 天阶段二：性能与真实调用证据

## 采集信息

- 实际操作日期：2026-11-18
- 服务：本地 Go HTTP Server
- 模型供应商：OpenAI 兼容接口
- 模型：`deepseek/deepseek-flash`
- 配置：来自本地 `.env`
- API Key：未记录
- Prompt 和模型回答：未记录
- 真实调用总数：10 次
- 结果：10 次全部成功

## 观测字段

HTTP 请求观测由三类结构化日志组成：

`http request completed` 日志由 ResponseWriter 包装器记录：

- `request_id`
- `method`
- `path`
- `status`
- `duration_ms`
- `total_duration_ms`

`http request parsed` 日志由请求解析边界记录：

- `parse_duration_ms`

`model call observed` 和 `model stream observed` 日志由模型客户端 Decorator 记录：

- `model_duration_ms`
- `total_duration_ms`
- `first_token_duration_ms`
- `finish_reason`
- `usage`
- `error_type`

其中：

- `/health` 记录总耗时。
- `/chat` 由三类日志关联记录 JSON 解析、模型调用和 HTTP 总耗时。
- `/chat/stream` 的模型流日志记录建立上游模型流的 `model_duration_ms`、首 Token、
  `finish_reason`、usage 和模型流总耗时；HTTP 完成日志记录整个接口总耗时。
- 非流式 `/chat` 的 HTTP 业务响应当前没有暴露 `finish_reason`，因此外部调用记录记为 `not_exposed`；本阶段没有把该字段混入通用 HTTP 完成日志。
- 本次流式响应没有返回 usage，记录为 `not_available`，没有虚构 Token 数量。

观测实现边界：

- `ServeHTTP` 通过响应包装器统一捕获最终状态码和 HTTP 总耗时。
- `decodeChatRequest` 在函数内部记录请求解析耗时，不要求调用方传入计时对象。
- `observedChatClient` 和 `observedChatStream` 通过 Decorator 记录模型调用观测。
- HTTP 业务 Handler 不再接收 `requestTiming` 或观测对象，降低观测逻辑对业务代码的侵入。

## 健康检查

| 接口 | 状态 | 服务端总耗时 |
| --- | --- | ---: |
| `GET /health` | 200 | 0.125 ms |

## 真实调用记录

以下数据来自服务端结构化日志；`curl` 客户端测得的总耗时仅用于交叉验证。所有记录均已脱敏。

| 编号 | 接口 | 状态 | 首 Token ms | 总耗时 ms | finish_reason | usage | error_type |
| ---: | --- | --- | ---: | ---: | --- | --- | --- |
| 1 | `/chat` | success | not_exposed | 1071.546 | not_exposed | prompt=38, completion=31, total=69 | none |
| 2 | `/chat` | success | not_exposed | 70.611 | not_exposed | prompt=38, completion=31, total=69 | none |
| 3 | `/chat` | success | not_exposed | 60.018 | not_exposed | prompt=38, completion=31, total=69 | none |
| 4 | `/chat` | success | not_exposed | 40.448 | not_exposed | prompt=38, completion=31, total=69 | none |
| 5 | `/chat` | success | not_exposed | 49.565 | not_exposed | prompt=38, completion=31, total=69 | none |
| 1 | `/chat/stream` | 200 | 179.553 | 808.111 | stop | not_available | none |
| 2 | `/chat/stream` | 200 | 39.341 | 41.634 | stop | not_available | none |
| 3 | `/chat/stream` | 200 | 320.594 | 390.158 | stop | not_available | none |
| 4 | `/chat/stream` | 200 | 44.306 | 45.464 | stop | not_available | none |
| 5 | `/chat/stream` | 200 | 36.929 | 40.884 | stop | not_available | none |

### 简单统计

- `/chat` 总耗时中位数：60.018 ms。
- `/chat/stream` 首 Token 中位数：44.306 ms。
- `/chat/stream` 总耗时中位数：45.464 ms。
- 第 1 次调用出现冷启动或连接建立类延迟，不能据此代表稳定 P95/P99。
- 当前样本量只有 5 次/接口，不作为生产 SLO 或性能承诺。

## 当前系统边界

### 已具备

- 非流式 HTTP 模型调用。
- SSE 流式模型调用。
- JSON、消息数量、角色、内容和请求体大小校验。
- Context 取消和超时传播。
- 统一错误响应和 request ID。
- OpenAPI 契约、契约测试和 CI。
- Go Fuzz 入口、race 测试和 `go vet` 检查。
- HTTP 层分段耗时日志。
- 真实模型调用的脱敏证据记录。

### 已知限制

- 尚未持久化 Conversation、Run、Message 和运行事件。
- 尚未实现数据库、租户、身份、权限、配额和异步 Worker。
- 尚未实现模型供应商路由、重试、熔断和降级。
- 真实模型输出受供应商、模型版本、网络和服务端负载影响。
- 当前只能记录调用元数据，不能完整重放真实模型行为。
- 流式 usage 依赖供应商是否返回 usage chunk，本次供应商未返回。
- `/chat` 业务响应暂未暴露 `finish_reason`。

## 下一周依赖

- 抽象模型提供方和供应商配置。
- 隔离模型名称、温度、最大 Token、超时等调用参数。
- 为 Prompt、Schema 和模型配置增加版本。
- 增加结构化输出校验和有限修复。
- 记录 provider、model、prompt_version 和 schema_version。

## 可重放性边界

当前可以安全记录：

- request ID 和接口元数据。
- 模型名称和非敏感配置摘要。
- 请求消息数量和请求摘要。
- 请求解析、模型调用、首 Token 和总耗时。
- usage、finish reason 和错误分类。

当前不记录：

- API Key。
- 完整 Prompt。
- 完整模型回答。
- 供应商鉴权 Header。

因此，当前真实模型调用只能进行“元数据级审计”，不能保证确定性重放。后续需要保存版本化配置快照、依赖录制和可控模型替身；真实模型重新调用必须明确标记为非确定性比较。
