# ToolLoopRuntime 执行准备与模型调用整理

## 1. 背景与目标

`ToolLoopRuntime.Run` 已经通过独立阶段方法展示受控多轮工具调用主流程，但执行准备、模型预算、调用记录和 Token 结算等横切逻辑仍然内联。其中，执行准备占用主流程入口，三个模型阶段则重复实现相同的受控调用规则。

本次优化只整理 `ToolLoopRuntime` 的内部结构，目标是让主流程突出业务编排、消除三个模型阶段的重复实现，同时保持既有运行语义不变。

## 2. 执行准备边界

新增 `internal/agent/tool_loop_execution.go`，由 `ToolLoopRuntime.prepareExecution` 统一负责：

1. 在配置选择之前记录 Run 开始时间；
2. 解析用户、租户和 Run 三级预算；
3. 创建本次 Run 独立的 `BudgetController`；
4. 配置用户级预算时校验可信 `UserID`；
5. 创建携带 `BudgetViolation` Cause 的总超时 Context；
6. 初始化 `runtimeExecution`、RunStore recorder 和脱敏观测字段；
7. 将同一个执行账本注入 Context，供后续阶段共享。

`Run` 的公开签名不变。执行准备失败仍直接返回；后续配置绑定、任务分析、工具循环、最终回答、观测和失败收口的顺序不变。

## 3. 受控模型调用边界

新增 `internal/agent/tool_loop_model_call.go`：

- `toolLoopModelCall` 描述不同业务阶段的任务、Prompt、阶段错误名称和稳定模型错误哨兵；
- `callModelWithinBudget` 统一实现模型路由观测、步数与预算检查、单次 Timeout 和 MaxTokens 收紧、模型调用、RunStore 记录、响应与成本记录、Token 结算和错误包装。

公共模型调用顺序为：

```text
记录路由
→ BeginModelStep
→ 按 Run 剩余时间收紧 Timeout
→ 按剩余 Token 收紧 MaxTokens
→ ModelCaller.Chat
→ 记录 model.call
→ 记录模型响应与成本
→ 结算真实或估算 Token
→ 返回阶段化错误
```

供应商同时返回响应和错误时，仍先记录并结算已经发生的用量，再返回模型错误。供应商未返回 Usage 时，仍按当前阶段 Prompt 和实际响应文本执行确定性 Token 估算。

## 4. 各阶段保留的职责

| 阶段 | 保留职责 | 稳定模型错误 |
| --- | --- | --- |
| `runAnalysis` | 渲染分析 Prompt、读取分析路由、保存响应、解析 `TaskAnalysis` | `ErrAnalysisModelFailed` |
| `callToolDecisionModel` | 读取工具决策路由、传入本轮 Prompt、保存待解析决策响应 | `ErrToolDecisionModelFailed` |
| `callToolLoopFinalModel` | 读取最终回答路由、传入最终 Prompt、保存待解析回答响应 | `ErrFinalModelFailed` |

原有阶段错误名称继续保留，包括 `tool_loop_analysis_budget`、`tool_loop_tool_decision_token_budget` 和 `tool_loop_final_model` 等，不因公共调用流程而合并错误分类。

## 5. 未改变的范围

- `Run` 公开契约、工具循环和最终输出结构不变；
- 授权、精确重复检测、通用循环检测、Executor 和继续决策顺序不变；
- 预算生效时机、Token 扣减顺序、RunStore 事件和 Observer 语义不变；
- `executeTool`、`failToolLoop` 和 Repair 包装不在本次调整范围内；
- 普通 `Runtime` 暂未迁移，其类似重复需要后续独立方案；
- 不涉及 Tape、Replay、Checkpoint 或上下文治理。

## 6. 验收结果

2026-11-27 完成以下检查，全部通过：

```bash
gofmt -l .
git diff --check
go test -count=1 ./...
go vet ./...
go test -race -count=1 ./...
```

`gofmt -l .` 无输出，`git diff --check` 无差异格式错误；全量普通测试和 race 测试均通过。本次结论仅覆盖上述内部结构整理，不扩大为生产级配额、普通 Runtime 全量重构或确定性 Replay 验收。

## 7. 后续共享优化

同日完成后续独立优化后，上述私有实现已泛化：`tool_loop_execution.go` 和 `tool_loop_model_call.go` 最初分别由 `runtime_execution.go` 和 `runtime_model_call.go` 取代，`toolLoopModelCall` 已更名为 `runtimeModelCall`。普通 Runtime 也已复用共享执行准备与模型调用内核，两个 Runtime 的公开入口仍保留。随后的功能域整理又将共享模型调用与辅助函数归并到 `runtime_execution.go`，并将 ToolLoop 分为生命周期、模型阶段和工具执行三个文件。最终结构见 `docs/notes/Agent功能域文件整理-2026-11-27.md`。
