package runner

import "errors"

var (
	// ErrExecutorNotConfigured 表示单轮工具执行器缺少注册中心依赖。
	ErrExecutorNotConfigured = errors.New("tool executor is not configured")
	// ErrInvalidToolCallRequest 表示工具调用请求缺少必要的业务上下文。
	ErrInvalidToolCallRequest = errors.New("tool call request is invalid")
	// ErrInvalidToolDecision 表示模型工具决策不满足结构化输出契约。
	ErrInvalidToolDecision = errors.New("tool decision is invalid")
	// ErrToolVersionRequired 表示调用方没有显式提供工具版本。
	ErrToolVersionRequired = errors.New("tool version is required")
	// ErrArgumentsEncodingFailed 表示工具决策参数无法编码为 JSON。
	ErrArgumentsEncodingFailed = errors.New("tool arguments encoding failed")
)
