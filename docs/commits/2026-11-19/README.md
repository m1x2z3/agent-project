# 2026-11-19 提交记录

本目录记录 2026 年 11 月 19 日实际完成的项目变更。当天执行第二周第 3 天
计划，完成结构化输出内核并等待用户确认后提交。

## 已完成阶段

### 第二周第 3 天：结构化输出内核

状态：已完成开发、验证和提交。

代码提交：`c19512e`（完善结构化输出内核并拆分契约文件）
记录修正：`f729ee9`（修正结构化输出提交记录）

主要内容：

- 新增 `internal/structuredoutput` 包。
- 增加 `TaskAnalysis`、`ToolDecision`、`Citation` 和 `FinalAnswer` 类型。
- 使用 `github.com/eino-contrib/jsonschema` 生成 Schema。
- 使用 `github.com/santhosh-tekuri/jsonschema/v6` 执行 Schema 校验。
- Schema 版本固定为 `task_analysis.v1`、`tool_decision.v1` 和 `final_answer.v1`。
- 增加 JSON 严格解析、必填字段、类型、枚举、数值范围和额外字段校验。
- 增加工具调用和 answered/refused 分支的业务语义校验。
- 明确拆分 Schema 结构校验、Go 反序列化和 `Validate()` 业务语义校验。
- 增加最多一次的注入式修复机制和 `DecodeReport` 版本元数据。
- 错误不回显完整模型输出、Prompt、API Key 或供应商原始错误。
- 不修改 `/chat`、`/chat/stream`、Eino 现有链路和模型网关行为。

涉及文件：

- `internal/structuredoutput/kinds.go`
- `internal/structuredoutput/task_analysis.go`
- `internal/structuredoutput/tool_decision.go`
- `internal/structuredoutput/final_answer.go`
- `internal/structuredoutput/metadata.go`
- `internal/structuredoutput/errors.go`
- `internal/structuredoutput/schema.go`
- `internal/structuredoutput/decode.go`
- `internal/structuredoutput/structuredoutput_test.go`
- `docs/notes/结构化输出边界-2026-11-19.md`
- `README.md`
- `go.mod`
- `go.sum`

验证结果：

- `gofmt`：通过。
- `go test ./...`：通过。
- `go vet ./...`：通过。
- `go test -race ./...`：通过。
- 未进入第二周第 4 天。
