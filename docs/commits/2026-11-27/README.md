# 2026-11-27 实现记录

## 第四周第 3 天：上下文治理

状态：当日方案的生产代码、测试、边界说明和全量验收已完成，尚未提交 Git。

主要内容：

- 为 `ToolLoopConfig` 增加 `ContextPolicy`，支持零值默认值、自定义值和负数拒绝；
- 当前默认历史目标、单结果压缩目标和最新结果保留数为 `12000 / 2560 / 10`；
- 增加包含算法、发布版本和实际参数的确定性 `context.v1` 上下文版本；
- 新增纯本地单工具结果压缩，保存原始 Token、SHA-256 和 UTF-8 安全预览，不增加模型调用；
- 将工具历史升级为带 `context_version`、`omitted_entries` 和 `entries` 的受控 envelope；
- 历史超目标时先降级非关键旧条目，再从最旧开始省略，优先保留最新结果、失败结果及来源；
- 工具决策和最终回答使用同一模板分别构建完整历史基线与治理后 Prompt，只把治理后结果发送给模型；
- 原始 Query、TaskAnalysis、ConfigSnapshot、工具历史和 Run 事实不被治理过程修改；
- Agent Observer 与 RunStore 新增上下文版本、构建次数、裁剪次数及治理前后 Token 五个脱敏字段；
- 每次 Prompt 渲染成功后立即累计报告，中途失败保留已有指标，并发 Run 使用独立执行账本；
- 本地上下文 Token 估算与供应商真实 Usage、成本和执行步数保持分离；
- 补充非空短历史、长结果压缩、降级省略、关键证据、失败保留、多工具事实及 16 个并发 Run 的测试。

文档：

- 实现方案：`docs/notes/plans/第4周第3天-上下文治理实现方案-2026-11-27.md`；
- 边界说明：`docs/notes/上下文治理边界-2026-11-27.md`；
- 验收证据：`docs/evidence/2026-11-27/week4-day3.md`。

全量验证：

```text
gofmt -l .
git diff --check
go test -count=1 ./...
go vet ./...
go test -race -count=1 ./...
```

以上命令全部通过。验证范围、测试数据和限制详见当天 evidence 文档。

本次未实现可信度标签、Prompt Injection 拦截、数据流安全策略、Tape、Checkpoint、完整 Replay 或事件回放器。本文不填写 commit hash；未经用户要求不执行 Git commit。

## 第四周第 4 天：安全边界

状态：当日 `security.v1` 方案的生产代码、测试、边界说明和全量验收已完成，尚未提交 Git。

主要内容：

- 为普通 Runtime 和 ToolLoopRuntime 固定 `security.v1:<ReleaseVersion>`，建立系统、用户、外部内容和模型输出的封闭可信度及内容类别；
- Query、Analysis 和工具历史使用版本化 JSON envelope，所有模型调用固定为独立 system 加任务 user 消息；
- 工具结果类别只能来自可信注册定义，Data 和 Source 作为同一不可信内容投影，外部正文不能伪造或提升标签；
- 增加本地确定性、高置信度 Prompt Injection 快速拦截，不增加模型安全审查调用；命中原文不进入错误、Observer 或 RunStore；
- 工具结果命中后保留真实工具事实，但停止后续治理、模型和工具传播；执行器错误保持原优先级；
- 新增 `Executor.Inspect` 完整只读预检，在预算和执行事件之前发现工具解析、Schema、权限和审批契约错误；
- 高风险未审批操作和保留 `network_egress` 能力在执行前默认拒绝，并保留稳定底层原因；
- 增加 Run 级外部内容污染：成功结果含非空 Data/Source 后，后续非只读请求返回 `ErrDataExfiltrationBlocked`；连续只读和无污染低风险路径保持可用；
- Observer 与 RunStore 新增安全版本、检查次数、总拦截及注入、危险工具、数据流三个子类别计数；
- 新增 `security_prompt_injection`、`security_dangerous_tool`、`security_data_exfiltration` 稳定错误分类；
- 补充特殊字符、超长结果、业务失败、执行失败、普通契约失败、计数口径、敏感信息和 12 个混合并发 Run 的测试；
- 固定 system 纳入本地 Token 与上下文估算，供应商真实 Usage、成本和模型调用次数不被重复污染。

文档：

- 实现方案：`docs/notes/plans/第4周第4天-安全边界实现方案-2026-11-27.md`；
- 边界说明：`docs/notes/安全边界-2026-11-27.md`；
- 验收证据：`docs/evidence/2026-11-27/week4-day4.md`。

全量验证：

```text
gofmt -l .
git diff --check
go test -count=1 ./...
go vet ./...
go test -race -count=1 ./...
```

以上命令全部通过。固定规则的覆盖限制、误报风险和未验收范围详见边界说明及当天 evidence 文档。

本次未实现语义级注入检测或 DLP、真实审批服务、Checkpoint、完整 Replay、事件回放器、Fuzz 状态模型或第 6 天至少 40 条安全样例评测。本文不填写 commit hash；未经用户要求不执行 Git commit。
