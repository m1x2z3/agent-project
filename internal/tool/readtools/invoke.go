package readtools

import (
	"context"
	"sync/atomic"
	"time"

	"agent-project/internal/tool/tooling"
)

// InvokeWithObservation 是只读工具的推荐调用入口。
//
// 它仍然把真正的执行交给 tooling.Invoke，保证公共安全边界只有一个实现；
// 这里额外补充执行前被边界拒绝时的观测。具体工具已经开始执行后，由工具
// 自己记录成功、业务错误或数据源错误，避免一次调用产生两条观测。
func InvokeWithObservation(
	ctx context.Context,
	tool tooling.Tool,
	request tooling.ToolExecutionRequest,
	observer Observer,
) (tooling.ToolResult, error) {
	if observer == nil {
		observer = NoopObserver{}
	}

	start := time.Now()
	started := &atomic.Bool{}
	wrapped := &executionTracker{
		tool:    tool,
		started: started,
	}
	result, err := tooling.Invoke(ctx, wrapped, request)
	if err != nil && !started.Load() {
		observeTool(observer, start, request, result, err)
	}
	return result, err
}

type executionTracker struct {
	tool    tooling.Tool
	started *atomic.Bool
}

func (t *executionTracker) Definition() tooling.ToolDefinition {
	if t.tool == nil {
		return tooling.ToolDefinition{}
	}
	return t.tool.Definition()
}

func (t *executionTracker) Execute(
	ctx context.Context,
	request tooling.ToolExecutionRequest,
) (tooling.ToolResult, error) {
	t.started.Store(true)
	return t.tool.Execute(ctx, request)
}

var _ tooling.Tool = (*executionTracker)(nil)
