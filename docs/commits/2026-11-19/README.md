# 2026-11-19 提交记录

本目录记录 2026 年 11 月 19 日实际完成的项目变更。当天执行第二周第 3 天至
第 5 天计划；提交记录按实际完成的事情描述，不使用“本次提交”作为标题。

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
- 已进入第二周第 4 天，新增工作尚未提交，等待确认。

## 第二周第 4 天：配置版本和发布

状态：已完成开发、验证和提交。

代码提交：`1837937`（实现配置版本注册与运行快照）

主要内容：

- 新增 `internal/configregistry` 内存配置注册中心。
- 支持 Prompt、Schema、模型路由和价格配置的不可变版本发布。
- 支持版本激活、历史查询和基于历史版本创建新回滚版本。
- 支持 Run 绑定创建时的 active 配置快照。
- 增加深拷贝、并发访问、版本隔离和结构化输出版本映射测试。
- 明确配置快照不保存 API Key，且不修改现有 HTTP、模型网关和结构化输出行为。

涉及文件：

- `internal/configregistry/types.go`
- `internal/configregistry/errors.go`
- `internal/configregistry/registry.go`
- `internal/configregistry/registry_test.go`
- `docs/notes/配置版本与发布边界-2026-11-19.md`
- `README.md`

验证结果：

- `gofmt`：通过。
- `go test ./internal/configregistry`：通过。
- `go vet ./internal/configregistry`：通过。
- 未进入第二周第 5 天。

## 第二周第 5 天：Runnable 组合与灰度

状态：已完成开发和验证，等待用户确认后提交。

暂未填写 commit hash，原因是本阶段按约定先暂停确认，不提前提交。

主要内容：

- 为 `internal/configregistry` 增加 `BindRunToVersion`，支持把 Run 绑定到指定的已发布版本。
- 新增 `internal/rollout`，实现租户白名单、稳定哈希流量桶和 active/candidate 选择。
- 增加 `EvaluateGate`，根据样本量、成功率、结构化成功率、P95 延迟和平均成本输出
  `hold`、`allow`、`block` 或 `rollback_candidate`。
- 在 `internal/agent` 增加类型化 Eino Runtime，串联版本选择、快照绑定、Prompt 渲染、
  两阶段模型调用、结构化解析和最终响应格式化。
- 增加 `ModelCaller`、`Observer`、脱敏观测、成本估算和最多一次结构化输出修复注入。
- 增加候选版本影子运行，影子运行不调用工具、不写数据库、不产生外部副作用。
- 增加灰度、Runtime、配置绑定和发布门禁测试。
- 新增 `docs/notes/Runnable组合与灰度边界-2026-11-19.md`。
- 保持现有 `/chat`、`/chat/stream` 和 Eino Chain/Graph 示例行为不变。

涉及文件：

- `internal/configregistry/registry.go`
- `internal/configregistry/registry_test.go`
- `internal/rollout/rollout.go`
- `internal/rollout/rollout_test.go`
- `internal/agent/runtime.go`
- `internal/agent/runtime_test.go`
- `docs/notes/Runnable组合与灰度边界-2026-11-19.md`
- `README.md`
