package tooling

import (
	"context"
	"strings"
	"time"
)

// ValidateExecutionRequest 执行工具前检查工具定义、请求、令牌和副作用边界。
//
// 这是模型 ToolDecision 与真实 Tool.Execute 之间的确定性门禁。模型输出只包含
// 工具意图，不包含能够通过这些检查的 CapabilityToken。
func ValidateExecutionRequest(
	definition ToolDefinition,
	request ToolExecutionRequest,
	now time.Time,
) error {
	if err := definition.Validate(); err != nil {
		return err
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if err := validateArguments(definition.ParameterSchema, request.Arguments); err != nil {
		return err
	}
	if err := request.CapabilityToken.Validate(now); err != nil {
		if err == ErrCapabilityExpired {
			return err
		}
		if isEmptyCapabilityToken(request.CapabilityToken) {
			return ErrCapabilityTokenMissing
		}
		return err
	}
	token := request.CapabilityToken

	if request.TenantID != token.TenantID {
		return ErrTenantMismatch
	}
	if request.RunID != token.RunID {
		return ErrRunMismatch
	}
	if request.ToolName != definition.Name || request.ToolName != token.ToolName {
		return ErrToolMismatch
	}
	if request.ToolVersion != definition.Version || request.ToolVersion != token.ToolVersion {
		return ErrToolVersionMismatch
	}
	if !token.hasPermission(definition.Permission) {
		return ErrPermissionDenied
	}
	// 当前阶段没有审批服务，因此高风险写操作必须停在边界上；
	// 后续接入审批快照后，再把已审批凭证作为执行前置条件。
	if definition.RiskLevel == RiskHigh && definition.EffectClass != EffectRead {
		return ErrApprovalRequired
	}
	if !token.covers(request.ResourceScope) {
		return ErrResourceScopeDenied
	}
	if definition.TenantScope == TenantScopeAllowlist &&
		!containsString(definition.AllowedTenants, request.TenantID) {
		return ErrTenantMismatch
	}
	if definition.TenantScope == TenantScopeCurrentTenant && len(request.ResourceScope) == 0 {
		return ErrResourceScopeDenied
	}
	return nil
}

// Invoke 是唯一推荐的工具执行入口。
//
// 先校验 Context 和所有确定性安全条件，只有全部通过后才调用具体工具。工具
// 实现不应被 Agent 直接调用，否则会绕过 Effect Boundary。
func Invoke(ctx context.Context, tool Tool, request ToolExecutionRequest) (ToolResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if tool == nil {
		return ToolResult{}, ErrInvalidDefinition
	}
	definition := tool.Definition()
	if err := ValidateExecutionRequest(definition, request, time.Now()); err != nil {
		return ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}

	// Timeout 是工具契约的一部分，由统一执行边界负责转成 Context
	// 取消信号。具体工具仍需把这个 Context 继续传递给底层数据源。
	executionCtx, cancel := context.WithTimeout(ctx, definition.Timeout)
	defer cancel()
	if err := executionCtx.Err(); err != nil {
		return ToolResult{}, err
	}

	result, err := tool.Execute(executionCtx, request)
	if err != nil {
		return result, err
	}
	if err := result.Validate(); err != nil {
		return ToolResult{}, err
	}
	if result.ToolName != definition.Name || result.ToolVersion != definition.Version {
		return ToolResult{}, ErrResultInvalid
	}
	return result, nil
}

func containsString(values []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			return true
		}
	}
	return false
}

func isEmptyCapabilityToken(token CapabilityToken) bool {
	return strings.TrimSpace(token.TokenID) == "" &&
		strings.TrimSpace(token.TenantID) == "" &&
		strings.TrimSpace(token.RunID) == "" &&
		strings.TrimSpace(token.ToolName) == "" &&
		strings.TrimSpace(token.ToolVersion) == "" &&
		len(token.ResourceScope) == 0 &&
		len(token.Permissions) == 0 &&
		token.ExpiresAt.IsZero() &&
		strings.TrimSpace(token.Nonce) == ""
}
