package runner

import (
	"context"
	"time"

	"agent-project/internal/tool/registry"
	"agent-project/internal/tool/tooling"
)

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
