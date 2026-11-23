package registry

import (
	"context"
	"strings"

	"agent-project/internal/tool/tooling"
)

// RegisteredTool 表示注册中心保存的一条工具注册记录。
//
// Definition 是注册时捕获的工具定义快照，用于名称、版本、能力标签和
// 废弃状态等发现信息；Tool 是后续交给 tooling.Invoke 的工具实例。注册中心
// 只负责保存和返回它们，不直接调用 Tool.Execute。
type RegisteredTool struct {
	Definition tooling.ToolDefinition
	Tool       tooling.Tool
}

// canonicalTool 保证注册中心规范化后的工具身份也能传递到最终执行边界。
//
// Tool 接口没有修改 Definition 的方法，因此不能只修改注册中心里的快照；
// 对于名称或版本带首尾空格的工具，需要用这个轻量适配器让 Definition、
// 请求和结果使用同一组规范化身份。
type canonicalTool struct {
	tool       tooling.Tool
	definition tooling.ToolDefinition
}

func newCanonicalTool(tool tooling.Tool, definition tooling.ToolDefinition) tooling.Tool {
	return &canonicalTool{
		tool:       tool,
		definition: cloneToolDefinition(definition),
	}
}

func (t *canonicalTool) Definition() tooling.ToolDefinition {
	return cloneToolDefinition(t.definition)
}

func (t *canonicalTool) Execute(
	ctx context.Context,
	request tooling.ToolExecutionRequest,
) (tooling.ToolResult, error) {
	result, err := t.tool.Execute(ctx, request)
	if err != nil {
		return result, err
	}

	// 注册中心已经确认了工具身份；统一结果 Envelope 的身份字段，避免
	// 原工具仍返回带空格的名称或版本而被最终边界误判为不匹配。
	result.ToolName = t.definition.Name
	result.ToolVersion = t.definition.Version
	return result, nil
}

func normalizeToolIdentity(definition tooling.ToolDefinition) tooling.ToolDefinition {
	definition.Name = strings.TrimSpace(definition.Name)
	definition.Version = strings.TrimSpace(definition.Version)
	return definition
}
