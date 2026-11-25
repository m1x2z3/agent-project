package runstore

import "errors"

var (
	// ErrInvalidRun 表示 Run 记录缺少必要字段或字段值不合法。
	ErrInvalidRun = errors.New("runstore: invalid run")
	// ErrRunAlreadyExists 表示相同 Run ID 已经存在，不能重复创建。
	ErrRunAlreadyExists = errors.New("runstore: run already exists")
	// ErrRunNotFound 表示请求的 Run 不存在。
	ErrRunNotFound = errors.New("runstore: run not found")
	// ErrInvalidEvent 表示事件缺少必要字段或字段值不合法。
	ErrInvalidEvent = errors.New("runstore: invalid event")
	// ErrInvalidEventType 表示事件类型不在允许的生命周期事件集合中。
	ErrInvalidEventType = errors.New("runstore: invalid event type")
	// ErrRunIdentityMismatch 表示事件中的 Run、请求或租户标识与已保存 Run 不一致。
	ErrRunIdentityMismatch = errors.New("runstore: run identity mismatch")
	// ErrRunAlreadyFinished 表示已结束的 Run 不能继续追加事件或重复完成。
	ErrRunAlreadyFinished = errors.New("runstore: run already finished")
	// ErrInvalidStateTransition 表示 Run 状态不允许从当前状态转换到目标状态。
	ErrInvalidStateTransition = errors.New("runstore: invalid state transition")
)
