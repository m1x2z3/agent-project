# 2026-11-25 实现记录

## 第三周第 6 天：工具循环验收与证据

状态：已完成开发和阶段验证，等待确认后提交。

主要内容：

- 补充完整工具循环成功链路验收；
- 补充工具决策解析、授权、工具解析和最终回答失败测试；
- 补充重复调用和最大工具调用次数测试；
- 补充 Context 取消和并发运行测试；
- 补充 API Key、Prompt、模型原文、工具参数和工具返回数据脱敏测试；
- 新增脱敏验收报告：
  `docs/evidence/2026-11-25/week3-day6.md`；
- 更新工具循环边界说明，明确本日只进行离线验收，不新增生产接口。

阶段性验证：

```text
go test ./internal/agent
go test -race ./internal/agent -run 'TestToolLoopRunStopsWhenContextCanceledDuringToolExecution|TestToolLoopRuntimeSupportsConcurrentRuns'
git diff --check
```

完整仓库验证已在本日最后阶段执行并通过：

```text
gofmt -l .
go test ./...
go vet ./...
go test -race ./...
```

本日不接入 HTTP、MCP、数据库、RBAC、审批、自动重试、自动降级或真实工具副作用。
验证结果：格式检查、全部测试、`go vet` 和 race 检查均通过。
本阶段不填写 commit hash，等待用户确认后再提交。

## 第四周第 2 天：RunStore 接入 Agent Runtime

状态：已完成开发和验证，暂不提交 Git。

主要内容：

- 为普通 `Runtime` 和 `ToolLoopRuntime` 增加可选 `runstore.Store` 依赖；
- 新增 `runStoreRecorder`，统一创建 Run、追加脱敏事件和收口最终状态；
- 普通 Runtime 记录 `run.started`、`model.call` 和 `run.succeeded`、
  `run.failed`、`run.canceled`；
- ToolLoopRuntime 额外记录 `tool.call` 和 `tool.result`，并统一记录分析、
  工具决策和最终回答模型调用；
- 新增 `run.canceled` 事件类型；
- RunStore 只保存发布版本、Prompt、Schema、路由和价格版本引用，不保存完整配置；
- 工具参数和工具结果只保存 SHA-256 摘要，模型 Prompt、回答、API Key 和底层错误
  原文不会写入 RunStore；
- RunStore 写入失败通过 `RunStoreErrorHandler` 脱敏通知，不影响主流程；
- 主请求取消后使用脱离取消信号的存储 Context，尽力保存取消结果；
- 影子运行不写入 RunStore；
- 增加普通 Runtime 和 ToolLoopRuntime 的 MemoryStore 集成测试，覆盖成功、失败、
  取消、工具解析失败、工具执行失败、并发运行、影子运行和脱敏；
- 更新 RunStore 边界说明，明确配置注册中心、Runtime、Recorder 和 Store 的职责。

验证结果：

```text
gofmt -l .
go test ./...
go vet ./...
go test -race ./...
```

以上检查通过。当前仍使用进程内 `MemoryStore`，未接入数据库、HTTP 管理接口、
RBAC、审批系统或新的 HTTP API。

## 第四周第 1 天：Run 记录与事件存储基础

状态：已完成开发和阶段验证，等待确认后提交。

主要内容：

- 新增 `internal/runstore`，定义 Run 状态、配置版本引用、脱敏观测、事件类型、
  Run 记录和 `Store` 接口；
- 新增 `internal/runstore/memory`，实现线程安全的内存 Run 存储；
- 实现 Run 创建、事件追加、Run 完成、Run 查询和事件查询；
- 由存储层统一分配事件序号，拒绝身份不匹配、非法状态流转和已完成 Run 的写入；
- 新增 `internal/clone`，统一配置注册中心和 RunStore 的 Map 复制逻辑；
- 增加配置引用隔离、事件顺序、Context 取消、并发访问和 race 测试；
- 新增边界说明：
  `docs/notes/Run记录与事件持久化边界-2026-11-25.md`。

数据边界：

- Run 只保存发布版本、Prompt、Schema、路由和价格版本引用；
- 观测和事件只保存脱敏元数据、错误分类、耗时和摘要；
- 不保存 API Key、完整 Prompt、模型回答、工具参数或工具返回数据；
- 当前不接入 Agent Runtime、HTTP、数据库、RBAC 或审计系统。

阶段验证：

```text
go test ./internal/clone ./internal/configregistry ./internal/runstore/...
go test -race ./internal/runstore/memory
```

本阶段不填写 commit hash，等待用户确认后再提交。
