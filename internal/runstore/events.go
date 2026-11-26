package runstore

import "time"

// EventType 表示 Run 事件的业务类型。
type EventType string

const (
	// EventRunStarted 表示 Run 已创建并开始执行。
	EventRunStarted EventType = "run.started"
	// EventModelCall 表示一次模型调用已经发生。
	EventModelCall EventType = "model.call"
	// EventToolCall 表示一次工具调用已经进入执行边界。
	EventToolCall EventType = "tool.call"
	// EventToolResult 表示一次工具调用已经产生结果。
	EventToolResult EventType = "tool.result"
	// EventRunSucceeded 表示 Run 成功结束。
	EventRunSucceeded EventType = "run.succeeded"
	// EventRunFailed 表示 Run 因错误结束。
	EventRunFailed         EventType = "run.failed"
	EventRunBudgetExceeded EventType = "run.budget_exceeded" // 表示 Run 因预算超限进入确定性终态。
	// EventRunCanceled 表示 Run 因 Context 取消或主动取消而结束。
	EventRunCanceled EventType = "run.canceled"
)

// RunEvent 是一次 Run 执行过程中的脱敏事件。
//
// 事件只保存身份、类型、耗时、状态和摘要等元数据，不保存完整 Prompt、
// 模型回答、工具参数、工具返回数据或 API Key。Sequence 由存储层分配，
// 调用方不能自行指定事件顺序。
type RunEvent struct {
	Sequence        uint64
	RunID           string
	RequestID       string
	TenantID        string
	Type            EventType
	OccurredAt      time.Time
	Provider        string
	Model           string
	ToolName        string
	ToolVersion     string
	Status          string
	DurationMS      int64
	ErrorKind       string
	ArgumentsDigest string
	ResultDigest    string
	BudgetScope     string // 保存触发预算终止的限额来源作用域。
	BudgetLimit     int64  // 保存触发终止的生效限额；时间预算使用毫秒。
	BudgetUsed      int64  // 保存触发终止时的实际消耗；时间预算使用毫秒。
}
