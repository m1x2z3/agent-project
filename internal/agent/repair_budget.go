package agent

import (
	"context"

	"agent-project/internal/recording"
	"agent-project/internal/structuredoutput"
)

// budgetedRepair 将一次结构化修复纳入当前 Run 的步数和 Token 预算。
//
// RepairFunc 不携带供应商 Usage，因此修复结束后按非法原文和修复结果执行
// 确定性估算；即使修复返回错误，也先结算已经发生的输入和输出消耗。
func budgetedRepair(
	execution *runtimeExecution,
	repair structuredoutput.RepairFunc,
) structuredoutput.RepairFunc {
	if repair == nil {
		return nil
	}
	return func(ctx context.Context, request structuredoutput.RepairRequest) ([]byte, error) {
		if execution == nil || execution.budget == nil {
			return nil, ErrRuntimeNotConfigured
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		// 修复调用与普通模型调用共享步数和 Token 余额，被拒绝时不调用原修复函数。
		if err := execution.budget.BeginModelStep(recording.ClockFromContext(ctx).Now()); err != nil {
			return nil, err
		}
		repaired, repairErr := repair(ctx, request)
		estimated := estimateTokenCount(string(request.Raw)) + estimateTokenCount(string(repaired))
		if err := execution.budget.ChargeEstimatedTokens(estimated); err != nil {
			return nil, err
		}
		if repairErr != nil {
			return nil, repairErr
		}
		return repaired, nil
	}
}
