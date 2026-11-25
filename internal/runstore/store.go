package runstore

import "context"

// Store 定义 Run 记录和事件的持久化边界。
//
// 接口只描述生命周期和查询能力，不绑定内存、数据库或具体 ORM。
// 实现必须保证返回的 Run 和事件不会暴露内部可变状态。
type Store interface {
	// CreateRun 创建一条处于 RunRunning 状态的 Run 记录。
	CreateRun(context.Context, RunRecord) error
	// AppendEvent 向已存在且仍在运行的 Run 追加一个事件。
	AppendEvent(context.Context, RunEvent) error
	// FinishRun 将运行中的 Run 更新为成功、失败或取消状态。
	FinishRun(context.Context, string, RunStatus, RunObservation) error
	// GetRun 查询一条 Run 记录。
	GetRun(context.Context, string) (RunRecord, error)
	// ListEvents 按 Sequence 顺序查询指定 Run 的事件。
	ListEvents(context.Context, string) ([]RunEvent, error)
}
