# 2026-11-24 实现记录

## 第三周第 5 天：受控多轮工具循环

状态：已完成开发和验证，已提交。

主要内容：

- 新增 `ToolLoopRuntime.Run`，串联任务分析、工具决策、授权、工具执行、
  工具结果回传和最终回答。
- 增加工具调用次数上限和重复工具调用检测。
- 通过 `ToolCallAuthorizer` 注入工具版本、租户、Run、资源范围和 Capability Token。
- 保持 `runner.Executor -> Registry -> tooling.Invoke -> Tool.Execute` 的执行边界。
- 新增 `ToolLoopOutput` 格式化，只返回工具调用脱敏摘要。
- 普通 Runtime 和工具循环复用统一的模型用量、成本和最终观测逻辑。
- 增加完整多轮工具循环集成测试、取消测试、模型错误测试和观测测试。
- 新增边界说明：
  `docs/notes/受控多轮工具循环边界-2026-11-24.md`。

验证结果：

```text
go test ./...
go vet ./...
go test -race ./...
```

当前不接入 HTTP、MCP、数据库、RBAC、审批、自动重试、自动降级或多工具并行。
