package tooling

import (
	"encoding/json"
	"time"
)

// ToolDefinition 描述工具的能力、风险和执行约束。
type ToolDefinition struct {
	Name            string
	Version         string
	Description     string
	Permission      Permission
	RiskLevel       RiskLevel
	EffectClass     EffectClass
	Timeout         time.Duration
	ParameterSchema json.RawMessage
	TenantScope     TenantScope
	AllowedTenants  []string
	CapabilityTags  []string
	Deprecated      bool
}

// ToolExecutionRequest 是经过模型决策转换后，准备进入工具执行边界的请求。
type ToolExecutionRequest struct {
	ToolName        string
	ToolVersion     string
	TenantID        string
	RunID           string
	RequestID       string
	Arguments       json.RawMessage
	ResourceScope   []string
	CapabilityToken CapabilityToken
	IdempotencyKey  string
}

// SourceRef 表示工具结果的来源位置。
type SourceRef struct {
	Source   string
	Location string
}

// ToolError 是不绑定供应商协议的工具错误结构。
type ToolError struct {
	Code      string
	Message   string
	Retryable bool
}

// ToolResult 是工具返回给 Agent Runtime 的统一结果 Envelope。
type ToolResult struct {
	ToolName    string
	ToolVersion string
	Status      ResultStatus
	Data        json.RawMessage
	Source      []SourceRef
	Error       *ToolError
}
