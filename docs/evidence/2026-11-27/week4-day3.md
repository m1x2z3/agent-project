# 第四周第 3 天：上下文治理验收证据

## 1. 验收范围

本次验收覆盖 `ToolLoopRuntime` 的上下文策略、确定性单结果压缩、历史降级与省略、版本化 envelope、工具决策与最终回答 Prompt、Observer/RunStore 脱敏指标及并发 Run 隔离。

普通 `Runtime` 的固定两阶段 Chain 不纳入工具历史治理。验收使用确定性 Fake Model、本地测试工具和 MemoryStore，不访问真实模型服务，不执行真实外部写操作。

本日不验收第 4 天安全策略，也不验收 Tape、Checkpoint、完整 Replay、事件回放器或 Fuzz 状态模型。

## 2. 实现阶段

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| 1 | `ContextPolicy`、默认值、非法参数校验和 Runtime 接线 | 已完成 |
| 2 | 上下文版本、治理报告和 Observer/RunStore 字段 | 已完成 |
| 3 | 单工具结果确定性压缩 | 已完成 |
| 4 | 历史降级、省略和版本化 envelope | 已完成 |
| 5 | 工具决策与最终回答 Prompt 接入 | 已完成 |
| 6 | Observer 与 RunStore 指标记录 | 已完成 |
| 7 | 单元、集成、并发和回归测试 | 已完成 |
| 8 | 文档、证据和全量验收 | 已完成 |

## 3. 已验证能力

### 策略与版本

- 零值字段分别使用当前默认值 `12000 / 2560 / 10`；
- 全部自定义值原样保留，部分零值独立填默认值；
- 三个字段的负数均返回 `ErrInvalidContextPolicy`，非法配置不能构造 Runtime；
- 上下文版本包含算法、发布版本和实际参数，格式稳定且具有确定性；
- 构造后的策略对并发 Run 只读。

### 单结果压缩

- 短 JSON 不压缩、不改写，返回数组不与原始 Data 共享底层存储；
- 以紧凑内容判断压缩必要性，避免只压缩无意义空白；
- 长 JSON 输出确定、合法，包含压缩标记、原始 Token、原始字节 SHA-256 和预览；
- 中文与 Emoji 按 Unicode 码点截断，不出现 UTF-8 截断乱码；
- 预览是指定目标的紧凑 JSON 前缀，外层压缩对象仍为合法 JSON；
- 原始 Data 在压缩前后保持不变；
- 非法 JSON 和非法压缩目标分别保留稳定错误契约。

### 历史治理

- 短历史不降级、不省略，envelope 中 `omitted_entries = 0`；
- 超出单结果目标后执行确定性 Data 压缩；
- 超出历史目标后先移除最旧非关键条目的 Data，再省略最旧非关键条目；
- 最新结果和旧失败结果在极小历史预算下仍保留；
- 保留条目的工具名称、版本、状态、错误码、参数摘要和 Source 完整保留；
- 省略数量准确进入 `omitted_entries`；
- 即使非法 JSON 位于最终会被省略的旧条目中，也仍返回编码错误；
- 相同输入和策略得到相同历史 JSON、版本和治理报告；
- 原始历史及 ConfigSnapshot 不被治理过程修改。

### Prompt 与观测

- 工具决策和最终回答使用相同上下文版本及版本化 envelope；
- 未治理基线和实际 Prompt 使用同一模板、Query 和 TaskAnalysis；
- `tokensBefore` 与 `tokensAfter` 均按完整 Prompt 独立重建验证；
- 多次构建始终从原始历史开始，不累积上一轮压缩结果；
- 非空短历史的两次 Prompt 构建前后 Token 相等，未发生无意义治理；
- 无工具成功 Run 记录 2 次构建、0 次裁剪；
- 一次工具调用且结果超长时记录 3 次构建、2 次裁剪；
- 两次工具调用且历史目标极小时记录 4 次构建、2 次裁剪，后续决策和最终回答均报告 1 条省略历史；
- 多工具 Run 的最终 `ToolCalls`、决策轮次和 RunStore 工具事件仍完整保留；
- 最终回答结构化解析失败时，已完成的 2 次构建和 Token 指标仍保留，错误分类保持 `final_decode_error`；
- Observer 与 RunStore 的五个上下文字段一致；
- 工具原文和压缩 `preview` 不进入 Observer 或 RunStore；
- 压缩没有增加模型调用；一次工具集成场景仍只有 4 次模型调用；
- 本地上下文估算不污染真实 Usage，一次工具集成场景的供应商用量仍为 `PromptTokens=8`、`CompletionTokens=4`、`TotalTokens=12`。

### 并发隔离

- 64 份并发治理报告在同一执行账本中无丢失累计；
- 16 个并发 Run 均产生且仅产生一条 Observer 记录；
- 每个 Run 独立记录相同版本、2 次构建、0 次裁剪和一致 Token 报告；
- 不同 Run 的计数、Token、Observer 与 RunStore 终态没有相互污染；
- 全仓 race 检查通过。

## 4. 原方案验收项状态

| 验收项 | 状态 |
| --- | --- |
| 1：默认、自定义和非法策略 | 已覆盖并通过 |
| 2：短历史与稳定 Token 口径 | 已覆盖并通过 |
| 3：确定性压缩、合法 JSON 和 UTF-8 | 已覆盖并通过 |
| 4：工具元数据与来源保留 | 已覆盖并通过 |
| 5：先降级再省略及省略计数 | 已覆盖并通过 |
| 6：最新、失败、Query 与 TaskAnalysis 保留 | 已覆盖并通过 |
| 7：原始历史与 ConfigSnapshot 不变 | 已覆盖并通过 |
| 8：JSON、版本和 Token 报告确定性 | 已覆盖并通过 |
| 9：决策与最终回答版本及指标累计 | 已覆盖并通过 |
| 10：不增加模型调用、执行事实或真实 Usage | 已覆盖并通过 |
| 11：Observer/RunStore 一致且不保存敏感内容 | 已覆盖并通过 |
| 12：中途失败保留指标和稳定失败原因 | 已覆盖并通过 |
| 13：并发 Run 隔离和 race 检查 | 已覆盖并通过 |
| 14：既有授权、重复检测、预算、工具和输出回归 | 全仓测试通过 |

## 5. 全量验收命令

实际执行：

```bash
gofmt -l .
git diff --check
go test -count=1 ./...
go vet ./...
go test -race -count=1 ./...
```

结果：

```text
gofmt -l .                  通过，无输出
git diff --check            通过，无输出
go test -count=1 ./...      通过，全部包成功；三个 cmd 包无测试文件
go vet ./...                通过，无输出
go test -race -count=1 ./... 通过，全部包成功；三个 cmd 包无测试文件
```

普通测试和 race 测试均覆盖：

```text
internal/agent
internal/clone
internal/config
internal/configregistry
internal/evaluation
internal/httpapi
internal/llm
internal/modelgateway
internal/recording
internal/rollout
internal/runstore
internal/runstore/memory
internal/structuredoutput
internal/tool/readtools
internal/tool/registry
internal/tool/runner
internal/tool/tooling
practice/algorithms
```

## 6. 结论与限制

本日上下文治理方案已完成实现、文档和全量验收。当前实现能够在不增加模型调用的前提下，对 ToolLoop 工具历史执行确定性本地压缩、降级和省略，并向 Observer 与 RunStore 提供一致、脱敏、并发安全的治理指标。

测试通过只证明当前策略和对应测试场景，不代表任意语义重要信息都不会丢失，也不代表默认值适用于任意模型或生产容量。第 4 天安全策略、Tape、Checkpoint、完整 Replay、数据库持久化及第 4 周整周验收均不在本次结论范围内。
