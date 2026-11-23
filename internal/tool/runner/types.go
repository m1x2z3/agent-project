package runner

import (
	"agent-project/internal/structuredoutput"
	"agent-project/internal/tool/registry"
	"agent-project/internal/tool/tooling"
)

// ToolCallRequest 表示一次单轮工具调用所需的完整上下文。
//
// Decision 只表达模型是否想调用哪个工具以及调用参数；工具版本、租户、
// Run、请求 ID 和 CapabilityToken 必须由模型之外的可信调用方提供。
type ToolCallRequest struct {
	Decision        structuredoutput.ToolDecision
	ToolVersion     string
	TenantID        string
	RunID           string
	RequestID       string
	ResourceScope   []string
	CapabilityToken tooling.CapabilityToken
	IdempotencyKey  string
}

// Executor 是单轮工具调用编排器。
//
// Executor 负责把结构化工具决策转换为一次受控执行；工具发现由 Registry
// 提供，真正的参数、租户、Run、权限和能力令牌校验仍由 tooling.Invoke 完成。
// 当前只支持一次工具调用，不负责自动循环、多工具并行、重试或降级。
type Executor struct {
	registry *registry.Registry
}
