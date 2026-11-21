# 2026-11-21 提交记录

本目录记录 2026 年 11 月 21 日实际完成的第三周第 2 天工作。

## 第三周第 2 天：只读工具实现

状态：已完成开发、验证并提交。

对应 commit：`24dd23c`。

主要内容：

- 将工具相关代码统一收拢到 `internal/tool/`，其中 `tooling` 是公共契约层，
  `readtools` 是具体只读工具层。
- 实现 `get_current_time`，支持 IANA 时区、注入时钟和统一结果 Envelope。
- 实现 `search_project_docs`，支持项目文档查询、来源信息和结果数量限制。
- 新增 `ProjectDocStore` 接口和内存数据源，按租户和项目隔离数据。
- 增加项目不存在、空结果、超时、内部错误和资源范围拒绝处理。
- 增加 `InvokeWithObservation`，内部复用 `tooling.Invoke`，补充执行前边界拒绝观测。
- 观测只记录工具名称、版本、请求 ID、耗时、结果状态、错误码和参数摘要。
- 增加工具定义、参数 Schema、跨租户、Context 取消和观测去重测试。
- 不修改 `/chat`、`/chat/stream`、Agent Runtime、模型网关和结构化输出行为。
- 不接入数据库、MCP、工具注册中心、RBAC、审批系统或自动工具循环。

验证结果：

```text
go test ./internal/tool/readtools
go vet ./internal/tool/readtools
go test -race ./internal/tool/readtools
```
