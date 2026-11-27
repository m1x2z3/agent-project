package agent

import (
	"context"
	"strings"

	"agent-project/internal/recording"
	"agent-project/internal/runstore"
)

// runExecutionOptions 保存两种 Runtime 创建执行账本时不同的依赖和运行模式。
// 公共执行准备只读取这些参数，不依赖具体 Runtime，也不改变版本选择职责。
type runExecutionOptions struct {
	budgetResolver BudgetResolver       // 解析可信用户、租户和 Run 级预算；空值使用安全默认预算。
	runStore       runstore.Store       // 可选的 Run 记录存储，由执行记录器旁路写入。
	onStoreError   RunStoreErrorHandler // 接收旁路持久化失败，不改变模型和工具主流程。
	shadow         bool                 // 标记是否为候选版本的影子运行，并同步到脱敏观测。
}

// prepareRunExecution 为普通 Runtime 或工具循环准备独立预算、总超时和执行状态。
//
// 总时间预算从配置选择之前开始计算，避免前置阶段脱离本次 Run 的执行约束。
// 返回的 Context 同时携带总超时取消信号和本次 Run 独立的 runtimeExecution，
// 后续模型、工具、观测与 RunStore 记录都使用同一份执行账本。
func prepareRunExecution(
	ctx context.Context,
	input RunInput,
	options runExecutionOptions,
) (context.Context, context.CancelFunc, *runtimeExecution, error) {
	started := recording.ClockFromContext(ctx).Now()
	budgetSet := BudgetSet{}
	var err error
	if options.budgetResolver != nil {
		budgetSet, err = options.budgetResolver.Resolve(ctx, input)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	budget, err := newBudgetController(budgetSet, started)
	if err != nil {
		return nil, nil, nil, err
	}
	// 只有配置用户级预算时才强制要求可信用户身份，保持无用户预算调用的兼容性。
	if budgetSet.User != (BudgetLimits{}) && strings.TrimSpace(input.UserID) == "" {
		return nil, nil, nil, ErrInvalidRunInput
	}
	// 总超时携带结构化预算原因，失败收口时据此恢复准确的超预算终态。
	budgetCause := BudgetViolation{
		Scope:  budget.effective.DurationSource,
		Reason: "total_time_budget_exceeded",
		Limit:  budget.effective.Limits.MaxDuration.Milliseconds(),
		Used:   budget.effective.Limits.MaxDuration.Milliseconds(),
	}
	ctx, cancel := context.WithTimeoutCause(ctx, budget.effective.Limits.MaxDuration, budgetCause)

	execution := &runtimeExecution{
		started:  started,
		budget:   budget,
		shadow:   options.shadow,
		recorder: &runStoreRecorder{store: options.runStore, onError: options.onStoreError},
		observation: RunObservation{
			RequestID: input.RequestID,
			TenantID:  input.TenantID,
			RunID:     input.RunID,
			Shadow:    options.shadow,
		},
	}
	// 通过 Context 把同一个执行账本交给快照绑定和后续阶段，避免重新创建控制器。
	ctx = context.WithValue(ctx, executionContextKey{}, execution)
	return ctx, cancel, execution, nil
}

// prepareExecution 为工具循环适配共享执行准备流程。
// 工具循环没有影子运行模式，版本选择和 Run 快照绑定仍由后续阶段负责。
func (r *ToolLoopRuntime) prepareExecution(
	ctx context.Context,
	input RunInput,
) (context.Context, context.CancelFunc, *runtimeExecution, error) {
	return prepareRunExecution(ctx, input, runExecutionOptions{
		budgetResolver: r.budgetResolver,
		runStore:       r.runStore,
		onStoreError:   r.onStoreError,
	})
}
