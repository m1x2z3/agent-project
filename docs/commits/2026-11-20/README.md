# 2026-11-20 提交记录

本目录记录 2026 年 11 月 20 日实际完成的第二周第 6 天工作。

## 第二周第 6 天：评测与发布复盘

状态：已完成开发和验证，等待确认后提交。

本阶段暂不填写 commit hash。

主要内容：

- 新增版本化 JSONL 离线评测集 `evals/week2-day6.jsonl`。
- 新增 `internal/evaluation`，支持样例加载、确定性 Fake Provider、重复评测、
  Token/成本/延迟统计、输出稳定性摘要和错误分类。
- 新增 `cmd/eval`，可执行供应商对比、灰度选择、主运行、影子运行、发布门禁、
  回滚关系和历史重放演示。
- 复用 `rollout.EvaluateGate`，仅将质量样例纳入发布门禁，负向样例保留在错误报告中。
- 生成脱敏评测报告：
  `docs/evidence/2026-11-20/week2-day6.md`。
- 新增评测与发布复盘边界说明：
  `docs/notes/评测与发布复盘边界-2026-11-20.md`。
- 不访问真实模型服务，不修改 `/chat`、`/chat/stream`，不新增数据库、Worker 或 HTTP
  管理接口。

验证结果：

```text
go test ./...
go vet ./...
go test -race ./...
go run ./cmd/eval
```

离线演示结果：

- `v1` 主运行成功。
- `v2` 影子运行被受控 Fake Provider 触发结构化失败。
- 发布门禁返回 `block`。
- 显式创建 `v3` 回滚版本，记录 `PreviousVersion=v2`、
  `RollbackFrom=v1`。
- 历史重放结果一致。

## 第 3 周第 1 天：工具接口和能力模型

状态：已完成开发和验证，等待确认后提交。

主要内容：

- 新增 `internal/tool/tooling` 工具契约层。
- 定义工具元数据、风险等级、副作用类别、租户范围和参数 Schema。
- 定义统一工具调用请求、结果 Envelope 和错误状态。
- 增加 Capability Token 的租户、Run、工具版本、资源范围、过期时间和 nonce
  绑定校验。
- 增加 `tooling.Invoke` 作为确定性执行边界，禁止模型 ToolDecision 直接触发工具。
- 新增工具契约、能力令牌、跨租户、跨 Run、过期令牌、资源范围和 Context 取消测试。
- 新增工具契约与副作用边界说明：
  `docs/notes/工具契约与副作用边界-2026-11-20.md`。

本阶段不实现具体工具、工具注册中心、HTTP、数据库、MCP、RBAC 或自动工具循环。
本阶段暂不填写 commit hash。
