# 第三周第 6 天：工具循环验收证据

## 1. 证据范围

本报告记录第三周第 5 天受控多轮工具循环的离线验收结果。

验收对象：

```text
ToolLoopRuntime
  -> 配置版本选择与 Run 快照绑定
  -> TaskAnalysis
  -> ToolDecision
  -> 可信授权
  -> runner.Executor
  -> tool registry
  -> tooling.Invoke
  -> Tool.Execute
  -> 工具历史回传
  -> 下一轮 ToolDecision
  -> FinalAnswer
  -> ToolLoopOutput
```

本阶段使用确定性 Fake Model 和本地测试工具，不访问真实模型服务，不产生真实外部副作用。

## 2. 已验证场景

### 成功链路

- 任务分析成功；
- 模型产生工具调用意图；
- 授权回调注入工具版本、租户、Run 和 Capability Token；
- 工具经过 `Registry -> tooling.Invoke` 后执行；
- 工具结果进入下一轮模型决策；
- 模型停止调用工具后生成最终回答；
- 最终输出只保留工具调用摘要。

### 失败边界

- 工具决策 JSON、Schema 或业务语义校验失败；
- 授权回调失败；
- 工具不存在或版本无法解析；
- 最终回答结构化解析失败；
- 模型调用失败；
- 工具执行边界失败。

失败时会停止后续流程，并记录一次最终失败观测。

### 循环控制

- `should_call=false` 不进入工具执行；
- 相同工具、版本和参数摘要的重复调用会被拒绝；
- 达到最大工具调用次数后停止；
- 工具执行成功后才会继续下一轮模型决策。

### 取消和并发

- 分析阶段后取消不会继续调用工具决策模型；
- 工具执行过程中取消会传播到工具；
- 取消后不会继续下一轮模型调用；
- 同一个 Runtime 可以被多个 Run 并发使用，不同 Run 的状态不会互相污染。

### 脱敏

以下内容不会进入 `ToolLoopOutput`、`RunObservation` 或错误文本：

- API Key；
- 完整 Prompt；
- 完整模型输出；
- 完整工具参数；
- 完整工具返回数据；
- 供应商原始错误消息。

工具调用摘要只保留工具名称、版本、状态、错误分类和参数哈希。

## 3. 观测结果

运行观测只记录 Run 级元数据：

```text
request_id
tenant_id
run_id
release_version
provider
model
duration
success
structured_valid
token usage
cost
error_kind
```

成功和失败路径均验证为每个 Run 只提交一次最终观测。

错误分类包括：

```text
tool_decision_decode_error
tool_authorization_error
tool_execution_error
duplicate_tool_call
max_tool_calls_exceeded
final_decode_error
canceled
```

## 4. 当前验证命令

已完成的阶段性验证：

```bash
go test ./internal/agent
go test -race ./internal/agent -run 'TestToolLoopRunStopsWhenContextCanceledDuringToolExecution|TestToolLoopRuntimeSupportsConcurrentRuns'
git diff --check
```

完整仓库验证已在本日最后阶段执行并通过：

```bash
gofmt -l .
go test ./...
go vet ./...
go test -race ./...
```

验证结果：

```text
gofmt -l .       通过，无格式问题
go test ./...    通过
go vet ./...     通过
go test -race ./... 通过
```

## 5. 当前边界

当前工具循环仍然是本地可测试的内部编排能力，尚未接入：

- `/chat` 或 `/chat/stream`；
- HTTP 工具调用接口；
- MCP Adapter；
- 数据库持久化；
- 用户身份认证和 RBAC；
- 审批系统；
- 写工具和真实外部副作用；
- 自动重试、自动降级、多工具并行和异步 Worker。

生产使用前仍需要补充身份、权限、持久化、防重放和副作用治理能力。

## 6. 结论

工具循环的成功路径、失败收口、循环保护、取消传播、并发隔离和脱敏边界已经具备自动化验收覆盖。

本报告不代表真实供应商或真实工具质量结论，只证明当前本地确定性实现满足既定的编排和安全边界。
