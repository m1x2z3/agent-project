# 2026-11-26 实现记录

## 第四周第 2 天：预算和停止条件

状态：非 Tape 范围已完成开发、文档和全量验证，未提交 Git；录制 Tape 及其后续录制/消费工作按用户要求暂缓。

主要内容：

- 新增用户、租户、Run 三级预算解析、严格合并、安全默认值和来源记录；
- 为普通 Runtime 与 ToolLoopRuntime 接入每 Run 独立的步数、工具次数、Token 和总时间控制；
- 模型与工具在操作前检查预算，Token 在调用后结算，修复调用纳入同一账本；
- 缺少供应商 Usage 时使用确定性估算，并将估算消耗与真实 Observer 用量分离；
- 五个模型阶段按路由配置和剩余 Token 约束 `MaxTokens`；
- 新增 `budget_exceeded` 终态、结构化预算错误和稳定终止原因；
- RunStore 保存预算限额、实际消耗、步数、来源作用域和估算 Token，终止事件保存违规数值；
- 保留精确工具重复检测，新增通用无进展循环检测及 `loop_detected` 观测分类；
- 新增请求级 Clock，并接入 Runtime、ToolLoop、RunStore 和工具令牌校验；
- HTTP request ID 改为可注入随机源，保持既有响应格式；
- 将评测历史逻辑明确修正为 `rerun/config comparison`，不再称为确定性 Replay；
- 新增预算边界说明和验收证据：
  - `docs/notes/预算与可录制依赖边界-2026-11-26.md`；
  - `docs/evidence/2026-11-26/week4-day2.md`。

全量验证：

```text
gofmt -l .
git diff --check
go test -count=1 ./...
go vet ./...
go test -race -count=1 ./...
```

以上命令全部通过。验证证据和暂缓验收项详见当天 evidence 文档。

本次未实现 Tape、模型和工具结果录制装饰器、完整 Replay、Checkpoint、上下文治理、数据库配额或其他后续日期能力。本文不填写 commit hash；未经用户要求不执行 Git commit。
