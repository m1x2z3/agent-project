package agent

import (
	"context"

	"agent-project/internal/configregistry"
	"agent-project/internal/rollout"
	"agent-project/internal/runstore"
	"agent-project/internal/structuredoutput"
	"agent-project/internal/tool/runner"
	"agent-project/internal/tool/tooling"
)

// ToolCallRecord 表示一次已经进入工具执行边界的调用摘要。
//
// 这里只保留工具名称、版本、结果状态、错误分类和参数摘要，
// 不保存完整参数、Prompt、模型原文或 CapabilityToken。
type ToolCallRecord struct {
	ToolName        string
	ToolVersion     string
	Status          tooling.ResultStatus
	ErrorCode       string
	ArgumentsDigest string
}

// ToolLoopOutput 是受控多轮工具循环的最终输出。
//
// 它嵌入现有 RunOutput，复用发布版本、模型响应和结构化结果；
// 额外记录工具调用摘要与工具决策轮次，普通 Runtime 不需要感知这些字段。
type ToolLoopOutput struct {
	RunOutput
	ToolCalls          []ToolCallRecord
	ToolDecisionRounds int
}

// ToolCallAuthorizer 是可信调用方提供的工具授权回调。
//
// 回调接收模型的工具意图和本次 Run 上下文，负责补充显式工具版本、
// 资源范围、CapabilityToken 和幂等键，返回给 runner.Executor 执行。
// 模型输出本身不能通过这个回调获得授权。
type ToolCallAuthorizer func(
	context.Context,
	RunInput,
	structuredoutput.ToolDecision,
) (runner.ToolCallRequest, error)

// ToolLoopConfig 是受控多轮工具 Runtime 的依赖配置。
//
// Registry、Selector 和 Model 负责配置版本选择与模型调用；Executor
// 负责工具发现和执行；Authorizer 负责注入模型之外的可信授权上下文。
type ToolLoopConfig struct {
	Registry     *configregistry.Registry
	Selector     *rollout.Selector
	Model        ModelCaller
	Executor     *runner.Executor
	Authorizer   ToolCallAuthorizer
	Observer     Observer
	RunStore     runstore.Store
	OnStoreError RunStoreErrorHandler

	MaxToolCalls int

	AnalysisRepair     structuredoutput.RepairFunc
	ToolDecisionRepair structuredoutput.RepairFunc
	FinalAnswerRepair  structuredoutput.RepairFunc
}
