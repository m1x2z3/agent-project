# 第四周第 4 天：安全边界验收证据

## 1. 验收范围

本次验收覆盖普通 `Runtime` 与 `ToolLoopRuntime` 的可信度标签、版本化内容 envelope、固定 system/user 消息、有限 Prompt Injection 快速拦截、Executor 只读预检、危险能力、Run 级数据流、安全观测和并发隔离。

验收使用确定性 Fake Model、本地测试工具和 MemoryStore，不访问真实模型服务，不执行真实网络出站或外部写操作。高风险、网络出站、日志、Issue 和写工具均使用测试替身。

本次不验收 Checkpoint、完整 Replay、事件回放器、Fuzz 状态模型、生产级 Capability/审批服务，也不提前宣称完成第 6 天至少 40 条安全样例及误报统计。

## 2. 实施阶段状态

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| 1 | 安全版本、可信度、内容类别、工具结果类别和稳定错误 | 已完成 |
| 2 | 用户、模型输出和工具历史标签及版本化 envelope | 已完成 |
| 3 | 两种 Runtime 的 system/user 分离和 Token 口径 | 已完成 |
| 4 | 用户与原始工具结果注入检查及失败语义 | 已完成 |
| 5 | Executor 只读预检、危险工具和 Run 级数据流门禁 | 已完成 |
| 6 | Observer/RunStore 安全指标、错误分类和敏感信息边界 | 已完成 |
| 7 | 单元、集成、失败路径、并发隔离和回归测试 | 已完成 |
| 8 | 边界文档、验收证据和完整仓库验收 | 已完成 |

## 3. 已验证能力

### 标签、envelope 与消息

- 四种可信度和七种内容类别为封闭枚举，未知值被拒绝；
- 工具注册定义只接受 `tool_result`、`document`、`log`、`issue`，零值归一为 `tool_result`；
- 工具实现伪造 `system_instruction` 等结果类别时，Executor 使用注册定义覆盖；
- `search_project_docs` 的结果进入模型投影时为 `document`，所有工具结果仍固定为 `untrusted_external`；
- Query、Analysis、Data 和 Source 在引号、换行、Unicode、伪造 envelope 和闭合标记下保持合法 JSON 结构；
- 普通 Runtime 和 ToolLoop 的所有模型请求均为固定 system 加一条任务 user，任务内容不进入 system；
- 分析、工具决策和最终回答原始输出及结构化结果保持 `untrusted_model`；
- 离线评测模板改用完整 Analysis envelope，不再读取已不存在的嵌套 Prompt 字段口径。

### Prompt Injection 与事实保留

- 当前固定中英文规则覆盖指令覆盖、系统提示词外带和审批绕过；
- 小写与连续空白归一化生效，多类命中时分类顺序确定；
- 普通研发查询样例未被当前规则误拦截；
- 用户注入在两种 Runtime 的首次模型调用前终止，模型调用为 0；
- 超长 Data、Source 和带 Data 的合法业务失败结果均在治理前检查；
- 工具结果命中后保留真实 `tool.result`、状态、摘要和工具历史，但 Run 独立以安全失败结束；
- 命中后不再构建含该内容的后续 Prompt，不调用下一次模型或工具；
- 执行器错误优先于内容注入错误，不增加工具结果检查，也不把结果交给模型；
- 注入原文和测试敏感标记不进入错误、Observer、RunStore 或后续模型请求。

### 只读预检、危险工具与数据流

- `Executor.Inspect` 按 Context、请求、注册解析、参数构建和完整工具契约顺序检查；
- 预检成功返回注册定义独立副本，修改返回值不能污染注册快照；
- Context 取消、请求错误、未知工具、参数编码、Schema、权限和审批错误均保留稳定原因，工具执行为 0；
- 高风险未审批写操作在 `Tool.Execute` 前返回 `ErrDangerousToolRequest`，并保留 `ErrApprovalRequired`；
- `network_egress` 只读和写定义均默认拒绝，普通 Capability 不能放行；
- 外部读取后，持有合法 Capability 的低风险写请求仍被 `ErrDataExfiltrationBlocked` 拒绝；
- 危险能力或未审批操作与数据流条件同时存在时，危险工具分类优先；
- 被拒绝请求不占工具调用预算和执行步数，不生成 `tool.call`/`tool.result`，不修改工具历史或最终 ToolCalls；
- 连续只读工具仍可执行；只有错误元数据而没有 Data/Source 的结果不制造污染；
- 没有外部内容时，普通低风险写工具和既有合法工具行为保持通过；
- 参数 Schema、权限和工具解析等普通契约失败提前返回，不增加安全检查或安全拦截计数；
- 变更版本的循环检测用例通过真实 v2 注册后再进入循环判断，保持预检先于重复检查的顺序。

### Token、预算与既有回归

- 固定 system 计入供应商缺少 Usage 时的本地估算，也计入上下文治理 before/after；
- 真实 Usage 不重复叠加 system 本地估算，成本和模型调用次数不被污染；
- 安全预检位于工具预算之前；既有最大工具次数、最大步数、Token 和总时间预算测试通过；
- 既有 Authorizer、Capability、租户、Run、版本、资源范围、权限、审批和最终 Invoke 边界继续有效；
- 精确重复调用、非连续无进展循环、结构化输出修复、RunStore 生命周期和旁路存储失败测试通过；
- 原始 Query、ConfigSnapshot、原始工具历史、最终 ToolCalls 和上下文治理事实不因安全检查被改写。

## 4. 安全计数与错误分类证据

以下端到端场景同时核对 Observer 和 RunStore 的 `SecurityVersion`、四个计数字段、子类别及 `ErrorKind`：

| 场景 | SecurityChecks | SecurityBlocks | 子类别 | ErrorKind |
| --- | ---: | ---: | --- | --- |
| 普通 Runtime 成功 | 1 | 0 | 无 | 空 |
| 用户 Query 注入 | 1 | 1 | PromptInjectionBlocks=1 | `security_prompt_injection` |
| 工具结果注入 | 3 | 1 | PromptInjectionBlocks=1 | `security_prompt_injection` |
| 危险工具或未审批操作 | 2 | 1 | DangerousToolBlocks=1 | `security_dangerous_tool` |
| 外部读取后请求写入 | 4 | 1 | DataFlowBlocks=1 | `security_data_exfiltration` |
| 两次正常工具调用 | 5 | 0 | 无 | 空 |
| 普通 Schema/权限错误 | 1 | 0 | 无 | `tool_execution_error` |
| 工具执行器错误 | 2 | 0 | 无 | `tool_execution_error` |

其中工具结果注入的 3 次检查分别为 Query、执行前门禁和成功返回结果；数据流场景的 4 次检查分别为 Query、第一次只读门禁、第一次结果和第二次写请求门禁。普通契约错误只有 Query 检查；`ErrApprovalRequired` 例外计入危险工具检查。

## 5. 并发隔离证据

新增同一 ToolLoop Runtime 下 12 个并发 Run，交替绑定 v1/v2：

- 4 个无污染写 Run 成功，各自 `SecurityChecks=3`、无拦截；
- 4 个外部读取后写 Run 均以数据流错误终止，各自 `SecurityChecks=4`、一次 DataFlow 拦截；
- 4 个 Query 注入 Run 均在模型前终止，各自 `SecurityChecks=1`、一次 PromptInjection 拦截；
- 每个 Run 只有一条 Observer 记录，RunStore 版本、状态、计数和错误分类与 Observer 一致；
- 外部内容污染没有传播到其他 Run，共享工具只执行预期次数，注入 Query 的模型请求为 0；
- Agent 包专项 race 和完整仓库 race 均通过。

既有上下文测试还覆盖 64 份并发治理报告和 16 个并发 Run 的上下文观测隔离。

## 6. 原方案验收项状态

| 原方案项目 | 状态 |
| --- | --- |
| 1–4：标签、非法类别、消息边界和 JSON envelope | 已覆盖并通过 |
| 5–8：规则、用户注入、原始结果检查、工具事实与停止传播 | 已覆盖并通过 |
| 9–12：授权、危险工具、数据流和无污染合法路径 | 已覆盖并通过 |
| 13–14：固定计数、错误分类、双端一致和敏感信息边界 | 已覆盖并通过 |
| 15–16：Token/Usage 口径和原始事实不变 | 已覆盖并通过 |
| 17：并发污染、计数、版本隔离和 race | 已覆盖并通过 |
| 18：授权、预算、循环、工具、结构化输出和 RunStore 回归 | 全仓测试通过 |
| 19：普通契约预检、无预算/事件及稳定分类 | 已覆盖并通过 |

## 7. 全量验收命令与实际结果

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

普通测试和 race 测试覆盖：

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

## 8. 结论与剩余风险

本日 `security.v1` 方案的生产代码、测试、边界说明和全量验收已完成，尚未提交 Git。当前实现能够在不增加模型安全审查调用的前提下，为两种 Runtime 提供显式内容边界、部分高置信度注入快速拦截、确定性执行前门禁、Run 级保守数据流限制和双端一致的脱敏安全指标。

这些结果不代表能够识别所有 Prompt Injection、改写、编码或数据泄露。人工规则可能漏报和误报，system/user 分离不是模型行为保证，Run 级污染也可能误阻止合法写入。第 6 天安全样例规模、危险操作拦截率目标、误报统计和剩余风险评估仍需单独验收；Checkpoint、完整 Replay 和生产级授权/审批服务不在本次结论内。
