package runner

import (
	"context"
	"encoding/json"
	"strings"
	"time"

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

// Validate 检查一次工具调用请求是否具备进入执行编排的基本条件。
//
// 这里校验的是模型决策之外的可信上下文：请求、租户、Run、工具版本、
// CapabilityToken 以及资源范围。它不会替代 tooling.Invoke 的完整校验；
// 工具定义、参数 Schema、权限和副作用边界仍必须在最终执行前再次检查。
func (r ToolCallRequest) Validate(now time.Time) error {
	if err := r.Decision.Validate(); err != nil || !r.Decision.ShouldCall {
		return ErrInvalidToolDecision
	}
	if strings.TrimSpace(r.RequestID) == "" ||
		strings.TrimSpace(r.TenantID) == "" ||
		strings.TrimSpace(r.RunID) == "" {
		return ErrInvalidToolCallRequest
	}
	if strings.TrimSpace(r.ToolVersion) == "" {
		return ErrToolVersionRequired
	}

	if strings.TrimSpace(r.CapabilityToken.TokenID) == "" {
		return tooling.ErrCapabilityTokenMissing
	}
	if err := r.CapabilityToken.Validate(now); err != nil {
		return err
	}
	if r.CapabilityToken.TenantID != r.TenantID {
		return tooling.ErrTenantMismatch
	}
	if r.CapabilityToken.RunID != r.RunID {
		return tooling.ErrRunMismatch
	}
	if r.CapabilityToken.ToolName != r.Decision.ToolName {
		return tooling.ErrToolMismatch
	}
	if r.CapabilityToken.ToolVersion != r.ToolVersion {
		return tooling.ErrToolVersionMismatch
	}

	allowedResources := make(map[string]struct{}, len(r.CapabilityToken.ResourceScope))
	for _, resource := range r.CapabilityToken.ResourceScope {
		resource = strings.TrimSpace(resource)
		if resource != "" {
			allowedResources[resource] = struct{}{}
		}
	}
	for _, resource := range r.ResourceScope {
		resource = strings.TrimSpace(resource)
		if resource == "" {
			return tooling.ErrResourceScopeDenied
		}
		if _, ok := allowedResources[resource]; !ok {
			return tooling.ErrResourceScopeDenied
		}
	}
	return nil
}

// Executor 是单轮工具调用编排器。
//
// Executor 负责把结构化工具决策转换为一次受控执行；工具发现由 Registry
// 提供，真正的参数、租户、Run、权限和能力令牌校验仍由 tooling.Invoke 完成。
// 当前只支持一次工具调用，不负责自动循环、多工具并行、重试或降级。
type Executor struct {
	registry *registry.Registry
}

// NewExecutor 创建单轮工具调用编排器。
//
// 注册中心是执行器发现工具的必要依赖；如果没有注册中心，执行器无法安全
// 解析模型指定的工具，因此在构造阶段直接拒绝。
func NewExecutor(toolRegistry *registry.Registry) (*Executor, error) {
	if toolRegistry == nil {
		return nil, ErrExecutorNotConfigured
	}
	return &Executor{registry: toolRegistry}, nil
}

// resolveTool 校验调用请求后，从注册中心解析指定版本的工具。
//
// 工具版本必须由可信调用方显式提供，不能根据模型输出或注册中心中的版本
// 列表自动推断。该方法只完成“请求校验 + 工具发现”，不会编码参数，也不会
// 调用 tooling.Invoke 或 Tool.Execute。
func (e *Executor) resolveTool(request ToolCallRequest) (registry.RegisteredTool, error) {
	if e == nil || e.registry == nil {
		return registry.RegisteredTool{}, ErrExecutorNotConfigured
	}
	if err := request.Validate(time.Now()); err != nil {
		return registry.RegisteredTool{}, err
	}
	return e.registry.Resolve(request.Decision.ToolName, request.ToolVersion)
}

// buildExecutionRequest 将模型决策和可信调用上下文转换为工具层请求。
//
// 模型只提供 Arguments；工具版本、租户、Run、请求 ID、资源范围和能力令牌
// 都来自 ToolCallRequest。参数必须先编码为 JSON，后续才能交给 tooling.Invoke
// 执行 Schema、权限和副作用边界校验。
func buildExecutionRequest(request ToolCallRequest) (tooling.ToolExecutionRequest, error) {
	arguments, err := json.Marshal(request.Decision.Arguments)
	if err != nil {
		return tooling.ToolExecutionRequest{}, ErrArgumentsEncodingFailed
	}
	return tooling.ToolExecutionRequest{
		ToolName:        request.Decision.ToolName,
		ToolVersion:     request.ToolVersion,
		TenantID:        request.TenantID,
		RunID:           request.RunID,
		RequestID:       request.RequestID,
		Arguments:       arguments,
		ResourceScope:   append([]string(nil), request.ResourceScope...),
		CapabilityToken: request.CapabilityToken,
		IdempotencyKey:  request.IdempotencyKey,
	}, nil
}

// Execute 执行一次单轮工具调用。
//
// Executor 只负责编排各层边界：先解析工具，再把请求转换为工具层结构，
// 最后统一交给 tooling.Invoke。Invoke 是最终安全门禁，负责参数 Schema、
// 权限、租户、能力令牌和副作用策略校验，Executor 不绕过这些检查。
func (e *Executor) Execute(
	ctx context.Context,
	request ToolCallRequest,
) (tooling.ToolResult, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return tooling.ToolResult{}, err
		}
	}
	resolvedTool, err := e.resolveTool(request)
	if err != nil {
		return tooling.ToolResult{}, err
	}
	executionRequest, err := buildExecutionRequest(request)
	if err != nil {
		return tooling.ToolResult{}, err
	}
	return tooling.Invoke(ctx, resolvedTool.Tool, executionRequest)
}
