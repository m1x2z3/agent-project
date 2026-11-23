# 2026-11-23 实现记录

本目录记录 2026 年 11 月 23 日实际完成的第三周第 3 天工作。

## 第三周第 3 天：工具注册中心与能力发现

状态：已完成开发和阶段验证，等待确认后提交。

本阶段暂不填写 commit hash。

主要内容：

- 新增 `internal/tool/registry` 内存工具注册中心。
- 复用 `tooling.Tool` 和 `tooling.ToolDefinition`，按工具名称和版本保存注册记录。
- 支持工具注册、重复版本拒绝、显式版本解析和版本列表查询。
- 支持列出全部工具、列出可用工具和按能力标签查找工具。
- 已废弃工具保留在历史列表中，但不进入默认可用结果，也不能通过 `Resolve` 执行。
- 注册和查询都保护工具定义中的 Schema、租户列表和能力标签，避免外部修改污染内部状态。
- 增加注册中心与 `tooling.Invoke` 的集成测试，确认注册中心不会绕过执行边界直接调用工具。
- 增加并发注册和查询测试，普通测试与 race 测试通过。
- 新增工具注册与能力发现边界说明：
  `docs/notes/工具注册与能力发现边界-2026-11-23.md`。

本阶段不接入 HTTP、数据库、MCP、RBAC、审批系统、动态卸载或 Agent 自动工具循环。

阶段验证结果：

```text
go test ./internal/tool/registry
go test -race ./internal/tool/registry
```

## 第三周第 4 天：单轮工具调用编排

状态：已完成开发和验证，等待确认后提交。

本阶段暂不填写 commit hash。

主要内容：

- 新增 `internal/tool/runner` 单轮工具调用编排包。
- 新增 `ToolCallRequest`，区分模型调用意图和外部可信执行上下文。
- 新增请求前置校验，检查请求 ID、租户、Run、显式工具版本、
  Capability Token 和资源范围。
- 新增 `Executor`，串联 `ToolCallRequest`、`Registry.Resolve`、
  `tooling.Invoke` 和具体 `Tool.Execute`。
- 参数通过标准库 JSON 编码后，仍由 `tooling.Invoke` 执行最终 Schema、
  权限、租户、资源范围和副作用边界校验。
- 增加真实只读工具的跨包执行测试，覆盖 `get_current_time`、
  `search_project_docs` 和租户隔离。
- 增加 Context 取消、工具错误、参数 Schema、高风险写操作和废弃版本测试。
- 增加单轮工具调用编排边界说明：
  `docs/notes/单轮工具调用编排边界-2026-11-23.md`。

本阶段不接入 HTTP、Agent Runtime、MCP、数据库、RBAC、审批系统、自动工具循环、
多工具并行、自动重试或自动降级。
