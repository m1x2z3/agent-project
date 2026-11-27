package runner

import (
	"context"

	"agent-project/internal/recording"
	"agent-project/internal/tool/registry"
	"agent-project/internal/tool/tooling"
)

// Executor 是单轮工具调用编排器。
//
// Executor 负责把结构化工具决策转换为一次受控执行；工具发现由 Registry
// 提供，真正的参数、租户、Run、权限和能力令牌校验仍由 tooling.Invoke 完成。
// 当前只支持一次工具调用，不负责自动循环、多工具并行、重试或降级，
// 也不负责调用最终回答模型；最终回答由上层 Agent Runtime 使用 ModelCaller
// 独立完成。
type Executor struct {
	registry *registry.ToolRegistry
}

// NewExecutor 创建单轮工具调用编排器。
//
// 注册中心是执行器发现工具的必要依赖；如果没有注册中心，执行器无法安全
// 解析模型指定的工具，因此在构造阶段直接拒绝。
func NewExecutor(toolRegistry *registry.ToolRegistry) (*Executor, error) {
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
func (e *Executor) resolveTool(ctx context.Context, request ToolCallRequest) (registry.RegisteredTool, error) {
	if e == nil || e.registry == nil {
		return registry.RegisteredTool{}, ErrExecutorNotConfigured
	}
	if err := request.Validate(recording.ClockFromContext(ctx).Now()); err != nil {
		return registry.RegisteredTool{}, err
	}
	return e.registry.Resolve(request.Decision.ToolName, request.ToolVersion)
}

// Inspect 按真实执行顺序完成全部只读契约检查，但不调用 Tool.Execute。
//
// 返回的是注册中心中已解析工具定义的独立副本，供上层在执行前判断危险能力和
// 数据流。Context、请求、工具发现、参数编码、Schema、权限、资源范围和审批错误
// 都保留原稳定原因；该入口不占用 Agent 工具预算，也不产生工具执行事件。
func (e *Executor) Inspect(
	ctx context.Context,
	request ToolCallRequest,
) (tooling.ToolDefinition, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return tooling.ToolDefinition{}, err
		}
	}
	resolvedTool, err := e.resolveTool(ctx, request)
	if err != nil {
		return tooling.ToolDefinition{}, err
	}
	executionRequest, err := buildExecutionRequest(request)
	if err != nil {
		return tooling.ToolDefinition{}, err
	}
	if err := tooling.ValidateExecutionRequest(
		resolvedTool.Definition,
		executionRequest,
		recording.ClockFromContext(ctx).Now(),
	); err != nil {
		return tooling.ToolDefinition{}, err
	}
	// Registry.Resolve 已深拷贝可变字段；再返回定义值，调用方不能修改注册快照。
	return resolvedTool.Definition, nil
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
	resolvedTool, err := e.resolveTool(ctx, request)
	if err != nil {
		return tooling.ToolResult{}, err
	}
	executionRequest, err := buildExecutionRequest(request)
	if err != nil {
		return tooling.ToolResult{}, err
	}
	result, err := tooling.Invoke(ctx, resolvedTool.Tool, executionRequest)
	if err != nil {
		return result, err
	}
	// 结果类别只能来自本次解析到的注册定义。覆盖工具实现返回的同名字段，
	// 防止不可信工具结果把自身伪装成系统指令、用户输入或模型输出。
	result.ResultContentKind = resolvedTool.Definition.ResultContentKind.Normalized()
	return result, nil
}
