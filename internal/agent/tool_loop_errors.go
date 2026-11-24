package agent

import "errors"

var (
	// ErrToolLoopNotConfigured 表示工具循环缺少必要依赖。
	ErrToolLoopNotConfigured = errors.New("agent tool loop is not configured")
	// ErrToolDecisionDecodeFailed 表示模型返回的工具决策无法通过结构化校验。
	ErrToolDecisionDecodeFailed = errors.New("agent tool decision decode failed")
	// ErrToolDecisionModelFailed 表示工具决策阶段的模型调用失败。
	ErrToolDecisionModelFailed = errors.New("agent tool decision model failed")
	// ErrToolAuthorizationFailed 表示可信授权回调拒绝或无法补充执行上下文。
	ErrToolAuthorizationFailed = errors.New("agent tool authorization failed")
	// ErrToolExecutionFailed 表示工具执行阶段返回错误。
	ErrToolExecutionFailed = errors.New("agent tool execution failed")
	// ErrMaxToolCallsExceeded 表示一次 Run 超过了允许的工具调用次数。
	ErrMaxToolCallsExceeded = errors.New("agent maximum tool calls exceeded")
	// ErrDuplicateToolCall 表示检测到相同工具、版本和参数的重复调用。
	ErrDuplicateToolCall = errors.New("agent duplicate tool call")
	// ErrToolHistoryEncodingFailed 表示工具历史无法编码为下一轮模型输入。
	ErrToolHistoryEncodingFailed = errors.New("agent tool history encoding failed")
)

const (
	// defaultMaxToolCalls 限制单次 Run 的工具调用次数，防止模型形成无限循环。
	defaultMaxToolCalls = 3
)
