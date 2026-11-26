# 第四周第 2 天：预算与停止条件验收证据

## 1. 验收范围

本次验收覆盖普通 `Runtime`、`ToolLoopRuntime`、三级预算、执行停止、循环检测、Clock、HTTP 随机源、RunStore 预算元数据和 rerun/replay 报告语义。

用户已明确要求录制 Tape 暂缓。本报告只证明非 Tape 范围的实现和测试通过，不将原方案中的完整依赖重放验收标记为完成。

验收使用确定性 Fake Model、本地工具和 MemoryStore，不访问真实模型服务，不执行真实外部写操作。

## 2. 已验证能力

### 三级预算与执行控制

- 用户、租户和 Run 的各维度限制分别取更严格的正值；
- 非法预算、缺少用户身份、默认预算和旧 `MaxToolCalls` 配置具有明确行为；
- 模型、工具和修复调用计步，被拒绝的操作不进入底层依赖；
- 工具调用在执行边界前计数和拒绝；
- Token 跨多个模型阶段累计，恰好达限与超限分别验证；
- 缺少 Usage 时使用确定性估算，且不污染真实供应商用量；
- 五个模型入口的 `MaxTokens` 不超过路由配置和剩余预算；
- Run 总时间可中断阻塞模型，并与外部取消区分；
- 并发 Run 使用独立预算控制器。

### 终止、循环和记录

- 超预算进入一次 `budget_exceeded` 终态并追加对应事件；
- Run 最终记录包含预算限额、实际消耗、步数、来源作用域和估算 Token；
- 终止事件包含脱敏 `BudgetScope`、`BudgetLimit`、`BudgetUsed`；
- 终态不可回退，终态后不能重复完成或追加事件；
- 相同工具、版本和参数的精确重复会被拒绝；
- 通用检测覆盖 `A -> B -> A` 和授权版本变化的重复模型状态；
- 循环命中后不进入 Executor，RunStore 仍记录普通 `failed`；
- 原始模型回答、工具参数和工具结果不会进入 RunStore 或 Observer。

### 确定性依赖与术语

- Clock 可注入普通 Runtime 和 ToolLoopRuntime；
- Run 开始、结束、模型和工具事件使用注入时间，固定时钟下观测耗时为零；
- 工具授权、Executor 和 `tooling.Invoke` 共用请求 Clock；
- 未注入 Clock 时回退系统时间；
- HTTP Handler 可注入随机源，request ID 格式保持不变；
- 历史演示只声明 `rerun/config comparison`，不再冒充确定性 Replay。

## 3. 全量验收命令

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
gofmt -l .                 通过，无输出
git diff --check           通过，无输出
go test -count=1 ./...     通过，全部包成功；三个 cmd 包无测试文件
go vet ./...               通过，无输出
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

## 4. 原方案验收项状态

| 验收项 | 状态 |
| --- | --- |
| 1～9：预算、停止、终态、循环和取消 | 已覆盖并通过 |
| 10：时间、随机数、模型响应和只读工具结果按录制顺序重放 | 未完成；Tape 暂缓，仅完成 Clock 和随机源注入测试 |
| 11：缺项、摘要不匹配和数据损坏失败 | 未完成；依赖 Tape 消费契约 |
| 12：写工具重放拒绝且不调用底层实现 | 未完成；依赖工具重放边界 |
| 13：原始回答和工具结果不进入 RunStore 或 Observer | 已覆盖并通过 |
| 14：并发 Run 的预算、循环和录制数据隔离 | 预算与循环状态已覆盖；录制数据不适用，Tape 暂缓 |
| 15：真实重跑与依赖重放的调用次数和结果对比 | 未完成；当前只修正术语和报告语义 |

## 5. 结论与限制

非 Tape 范围已完成全量验收。当前实现能够限制单次 Run 的步数、工具次数、Token 和总时间，确定性停止重复或无进展循环，并保存脱敏预算证据。

这不代表原方案的完整可录制依赖和确定性重放目标已经达成，也不代表生产级多租户共享配额、数据库持久化、Checkpoint、HTTP 工具循环、JWT/RBAC、MCP 或审批系统已经完成。
