package tooling

import "context"

// Tool 是工具实现必须满足的最小接口。
//
// 工具实现不接收模型原始输出。调用方应通过 Invoke 进入确定性执行边界，
// 先完成权限、租户、Run 和能力令牌校验，再调用 Execute。
type Tool interface {
	Definition() ToolDefinition
	Execute(context.Context, ToolExecutionRequest) (ToolResult, error)
}

// Permission 表示工具要求的稳定权限标识。
type Permission string

// RiskLevel 表示工具执行失败或被误用时的风险等级。
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// EffectClass 表示工具可能产生的副作用类别。
type EffectClass string

const (
	EffectRead              EffectClass = "read"
	EffectIdempotentWrite   EffectClass = "idempotent_write"
	EffectNonRetryableWrite EffectClass = "non_retryable_write"
)

// TenantScope 表示工具允许访问的租户边界。
type TenantScope string

const (
	// TenantScopeGlobal 表示工具不限制到某个具体租户，但仍要求调用链携带租户上下文。
	TenantScopeGlobal TenantScope = "global"
	// TenantScopeAllowlist 表示工具只允许在 AllowedTenants 中的租户使用。
	TenantScopeAllowlist TenantScope = "tenant_allowlist"
	// TenantScopeCurrentTenant 表示工具只能访问当前令牌绑定的租户资源。
	TenantScopeCurrentTenant TenantScope = "current_tenant"
)

// ResultStatus 是工具结果的稳定状态枚举。
type ResultStatus string

const (
	ResultSuccess         ResultStatus = "success"
	ResultInvalidArgument ResultStatus = "invalid_argument"
	ResultNotFound        ResultStatus = "not_found"
	ResultForbidden       ResultStatus = "forbidden"
	ResultTimeout         ResultStatus = "timeout"
	ResultInternalError   ResultStatus = "internal_error"
)
