package readtools

import (
	"context"
	"time"

	"agent-project/internal/tool/tooling"
)

// InvokeWithObservation 是只读工具的推荐调用入口。
//
// 它仍然把真正的执行交给 tooling.Invoke，保证公共安全边界只有一个实现。
// 观测统一在这里记录，覆盖边界拒绝、工具成功、工具业务错误、Context
// 取消和结果 Envelope 校验失败，避免工具内部观测与外层观测重复或遗漏。
func InvokeWithObservation(
	ctx context.Context,
	tool tooling.Tool,
	request tooling.ToolExecutionRequest,
	observer Observer,
) (tooling.ToolResult, error) {
	if observer == nil {
		observer = NoopObserver{}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	start := time.Now()
	result, err := tooling.Invoke(withObservationSuppressed(ctx), tool, request)
	observeTool(observer, start, request, result, err)
	return result, err
}
